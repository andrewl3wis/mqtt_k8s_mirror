package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"

	"kube-mqtt-mirror/pkg/messaging"
	"kube-mqtt-mirror/pkg/messaging/mqtt"
	"kube-mqtt-mirror/pkg/messaging/postgres"
	"kube-mqtt-mirror/pkg/messaging/queue"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Config holds all configuration parameters
type Config struct {
	EnableWebhook      bool
	EnableMirror       bool
	MessagingType      string
	MessagingBroker    string
	MessagingTopic     string
	MessagingUsername  string
	MessagingPassword  string
	LocalRegistry      string
	WebhookPort        string
	InsecureRegistries bool
	TLSCertPath        string
	TLSKeyPath         string
	LogFile            string
	EnableTLS          bool
}

// AppContext holds application-wide context and resources
type AppContext struct {
	config                     Config
	downloadQueue              chan string
	msgClient                  messaging.Client
	wg                         *sync.WaitGroup
	logger                     *log.Logger
	server                     *http.Server
	imageExistsInLocalRegistry func(string) bool
	copyImage                  func(string, string) error
}

func (app *AppContext) doCopyImage(srcImage, destImage string) error {
	if app.copyImage != nil {
		return app.copyImage(srcImage, destImage)
	}
	return app.defaultCopyImage(srcImage, destImage)
}

func (app *AppContext) defaultCopyImage(srcImage, destImage string) error {
	// Parse source reference
	var opts []name.Option
	if app.config.InsecureRegistries {
		opts = append(opts, name.Insecure)
	}
	srcRef, err := name.ParseReference(srcImage, opts...)
	if err != nil {
		return fmt.Errorf("failed to parse source image: %v", err)
	}

	// Parse destination reference
	destRef, err := name.ParseReference(destImage, opts...)
	if err != nil {
		return fmt.Errorf("failed to parse destination image: %v", err)
	}

	app.logger.Printf("Starting image copy from %s to %s", srcImage, destImage)

	// Try to get the source as an index first (multi-arch)
	srcIdx, err := remote.Index(srcRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err == nil {
		app.logger.Printf("Source is a multi-arch image")
		// Copy the entire index with all architectures
		if err := remote.WriteIndex(destRef, srcIdx, remote.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
			return fmt.Errorf("failed to push multi-arch image: %v", err)
		}
		app.logger.Printf("Multi-arch image copy completed successfully: %s -> %s", srcImage, destImage)
		return nil
	}

	// If not an index, try as a single image
	srcImg, err := remote.Image(srcRef, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return fmt.Errorf("failed to get source image: %v", err)
	}

	// Get image config to check platform
	config, err := srcImg.ConfigFile()
	if err != nil {
		return fmt.Errorf("failed to get image config: %v", err)
	}
	app.logger.Printf("Source is a single-arch image for %s/%s", config.OS, config.Architecture)

	// Push the single image
	if err := remote.Write(destRef, srcImg, remote.WithAuthFromKeychain(authn.DefaultKeychain)); err != nil {
		return fmt.Errorf("failed to push image: %v", err)
	}

	app.logger.Printf("Single-arch image copy completed successfully: %s -> %s", srcImage, destImage)
	return nil
}

func (app *AppContext) checkImageExists(image string) bool {
	if app.imageExistsInLocalRegistry != nil {
		return app.imageExistsInLocalRegistry(image)
	}

	var opts []name.Option
	if app.config.InsecureRegistries {
		opts = append(opts, name.Insecure)
	}
	ref, err := name.ParseReference(fmt.Sprintf("%s/%s", app.config.LocalRegistry, image), opts...)
	if err != nil {
		app.logger.Printf("Failed to parse image reference: %v", err)
		return false
	}

	// Try as index first
	if _, err := remote.Index(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain)); err == nil {
		return true
	}

	// Try as single image
	if _, err := remote.Head(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain)); err == nil {
		return true
	}

	return false
}

func main() {
	// Initialize configuration
	config := parseFlags()

	// Set up logging
	logFile := setupLogging(config.LogFile)
	if logFile != nil {
		defer logFile.Close()
	}

	// Create application context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app := &AppContext{
		config:        config,
		downloadQueue: make(chan string, 100),
		wg:            &sync.WaitGroup{},
		logger:        log.Default(),
	}

	// Set up graceful shutdown
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// Start services
	if err := app.startServices(ctx); err != nil {
		app.logger.Fatalf("Failed to start services: %v", err)
	}

	// Wait for shutdown signal
	<-signalChan
	app.logger.Println("Shutdown signal received, initiating graceful shutdown...")

	// Initiate graceful shutdown
	cancel()
	app.shutdown()

	// Wait for all goroutines to finish
	app.wg.Wait()
	app.logger.Println("Graceful shutdown completed")
}

func parseFlags() Config {
	config := Config{}

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "kube-mqtt-mirror %s (%s) built on %s\n\nUsage: %s [options]\n\nOptions:\n",
			version, commit, date, os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), "\nExamples:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  Without TLS (behind reverse proxy):\n")
		fmt.Fprintf(flag.CommandLine.Output(), "    %s -webhook -mirror -messaging-type queue\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "  With TLS (direct access):\n")
		fmt.Fprintf(flag.CommandLine.Output(), "    %s -webhook -mirror -messaging-type queue -tls-cert server.crt -tls-key server.key\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "  MQTT broker:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "    %s -webhook -mirror -messaging-type mqtt -broker tcp://localhost:1883\n", os.Args[0])
		fmt.Fprintf(flag.CommandLine.Output(), "  PostgreSQL:\n")
		fmt.Fprintf(flag.CommandLine.Output(), "    %s -webhook -mirror -messaging-type postgres -broker localhost:5432 -username user -password pass\n", os.Args[0])
	}

	flag.BoolVar(&config.EnableWebhook, "webhook", false, "Enable webhook server to detect pod creation events")
	flag.BoolVar(&config.EnableMirror, "mirror", false, "Enable image mirroring service to process download requests")
	flag.StringVar(&config.MessagingType, "messaging-type", "mqtt",
		"Messaging backend type:\n"+
			"  queue    - Local file-backed queue for single instance\n"+
			"  mqtt     - MQTT broker for distributed operation\n"+
			"  postgres - PostgreSQL for distributed operation")
	flag.StringVar(&config.MessagingBroker, "broker", "tcp://localhost:1883",
		"Messaging broker address:\n"+
			"  queue    - Path to queue file (optional)\n"+
			"  mqtt     - Broker URL (e.g., tcp://localhost:1883)\n"+
			"  postgres - Database host:port")
	flag.StringVar(&config.MessagingTopic, "topic", "image/download", "Topic/channel name for messaging")
	flag.StringVar(&config.MessagingUsername, "username", "", "Username for MQTT/PostgreSQL authentication")
	flag.StringVar(&config.MessagingPassword, "password", "", "Password for MQTT/PostgreSQL authentication")
	flag.StringVar(&config.LocalRegistry, "local-registry", "localhost:5000", "Address of local Docker registry to mirror images to")
	flag.StringVar(&config.WebhookPort, "webhook-port", "8443", "Port for webhook server to listen on")
	flag.BoolVar(&config.InsecureRegistries, "insecure-registries", false, "Allow pulling from insecure (HTTP) registries")
	flag.StringVar(&config.TLSCertPath, "tls-cert", "", "Path to TLS certificate for webhook server (enables TLS if provided)")
	flag.StringVar(&config.TLSKeyPath, "tls-key", "", "Path to TLS private key for webhook server (enables TLS if provided)")
	flag.StringVar(&config.LogFile, "log-file", "", "Log file path (defaults to stdout)")

	flag.Parse()

	if !config.EnableWebhook && !config.EnableMirror {
		log.Fatal("At least one of --webhook or --mirror must be enabled")
	}

	// Enable TLS if cert and key paths are provided
	config.EnableTLS = config.TLSCertPath != "" && config.TLSKeyPath != ""

	return config
}

func setupLogging(logFilePath string) *os.File {
	if logFilePath == "" {
		return nil
	}

	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}

	log.SetOutput(logFile)
	return logFile
}

func (app *AppContext) startServices(ctx context.Context) error {
	// Start background worker to process download queue
	app.wg.Add(1)
	go app.processDownloadQueue(ctx)

	// Start message-based image mirroring
	if app.config.EnableMirror {
		if err := app.startMessaging(); err != nil {
			return fmt.Errorf("failed to start messaging: %v", err)
		}
	}

	// Start webhook server
	if app.config.EnableWebhook {
		if err := app.startWebhookServer(ctx); err != nil {
			return fmt.Errorf("failed to start webhook server: %v", err)
		}
	}

	return nil
}

func (app *AppContext) shutdown() {
	// Close download queue
	close(app.downloadQueue)

	// Disconnect messaging client if connected
	if app.msgClient != nil {
		app.msgClient.Disconnect()
	}

	// Shutdown HTTP server if running
	if app.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.server.Shutdown(ctx); err != nil {
			app.logger.Printf("HTTP server shutdown error: %v", err)
		}
	}
}

func (app *AppContext) startMessaging() error {
	// Create messaging client
	var client messaging.Client
	config := messaging.Config{
		Broker:   app.config.MessagingBroker,
		Topic:    app.config.MessagingTopic,
		Username: app.config.MessagingUsername,
		Password: app.config.MessagingPassword,
	}

	// Create messaging client based on type
	switch app.config.MessagingType {
	case "queue":
		client = queue.NewClient(config, app.logger)
	case "mqtt":
		client = mqtt.NewClient(config, app.logger)
	case "postgres":
		client = postgres.NewClient(config, app.logger)
	default:
		return fmt.Errorf("unsupported messaging type: %s (must be queue, mqtt, or postgres)", app.config.MessagingType)
	}

	app.msgClient = client

	// Connect to broker
	if err := app.msgClient.Connect(context.Background()); err != nil {
		return fmt.Errorf("failed to connect to messaging broker: %v", err)
	}

	// Subscribe to topic
	if err := app.msgClient.Subscribe(func(req messaging.ImageRequest) error {
		if req.Image == "" {
			app.logger.Println("Invalid payload: 'image' key not found")
			return nil
		}

		if app.checkImageExists(req.Image) {
			app.logger.Printf("Image found in local registry: %s", req.Image)
			return nil
		}

		app.logger.Printf("Queueing image for download: %s", req.Image)
		app.downloadQueue <- req.Image
		return nil
	}); err != nil {
		return fmt.Errorf("failed to subscribe: %v", err)
	}

	app.logger.Printf("Subscribed to topic: %s", app.config.MessagingTopic)
	return nil
}

func (app *AppContext) processDownloadQueue(ctx context.Context) {
	defer app.wg.Done()

	// Track in-progress downloads to prevent duplicates
	inProgress := make(map[string]bool)
	var mu sync.Mutex

	// Rate limiter for concurrent downloads
	maxConcurrent := make(chan struct{}, 3) // Allow 3 concurrent downloads

	for {
		select {
		case <-ctx.Done():
			return
		case image, ok := <-app.downloadQueue:
			if !ok {
				return
			}

			// Check if already downloading
			mu.Lock()
			if inProgress[image] {
				app.logger.Printf("Image already being downloaded: %s", image)
				mu.Unlock()
				continue
			}
			inProgress[image] = true
			mu.Unlock()

			app.wg.Add(1)
			go func(img string) {
				defer app.wg.Done()
				defer func() {
					mu.Lock()
					delete(inProgress, img)
					mu.Unlock()
				}()

				// Rate limit concurrent downloads
				maxConcurrent <- struct{}{}
				defer func() { <-maxConcurrent }()

				// Try up to 3 times with exponential backoff
				backoff := time.Second
				for attempt := 1; attempt <= 3; attempt++ {
					if err := app.doCopyImage(img, fmt.Sprintf("%s/%s", app.config.LocalRegistry, img)); err != nil {
						if attempt == 3 {
							app.logger.Printf("Failed to download image after %d attempts: %v", attempt, err)
							return
						}
						app.logger.Printf("Download attempt %d failed: %v, retrying in %v", attempt, err, backoff)
						time.Sleep(backoff)
						backoff *= 2
						continue
					}
					app.logger.Printf("Successfully downloaded image: %s", img)
					return
				}
			}(image)
		}
	}
}

func (app *AppContext) startWebhookServer(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", app.handleWebhook)

	// Create listener first to get the actual port
	var listener net.Listener
	var err error

	if app.config.WebhookPort == "0" {
		// Random port
		listener, err = net.Listen("tcp", "0.0.0.0:0")
	} else {
		listener, err = net.Listen("tcp", "0.0.0.0:"+app.config.WebhookPort)
	}
	if err != nil {
		return fmt.Errorf("failed to create listener: %v", err)
	}

	app.server = &http.Server{
		Handler: mux,
	}

	app.wg.Add(1)
	go func() {
		<-ctx.Done()
		app.server.Shutdown(context.Background())
		app.wg.Done()
	}()

	// Get the actual port
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	if !app.config.EnableTLS {
		app.logger.Printf("Starting webhook server without TLS on 0.0.0.0:%s", port)
		return app.server.Serve(listener)
	}

	// Load TLS certificate
	cert, err := tls.LoadX509KeyPair(app.config.TLSCertPath, app.config.TLSKeyPath)
	if err != nil {
		return fmt.Errorf("failed to load TLS certificate: %v", err)
	}

	app.logger.Printf("Starting webhook server with TLS on 0.0.0.0:%s", port)

	// Wrap listener with TLS
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{cert},
	})

	return app.server.Serve(tlsListener)
}

func (app *AppContext) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var admissionReview admissionv1.AdmissionReview
	decoder := serializer.NewCodecFactory(runtime.NewScheme()).UniversalDeserializer()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		app.logger.Printf("Failed to read request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if _, _, err := decoder.Decode(body, nil, &admissionReview); err != nil {
		app.logger.Printf("Failed to decode admission review: %v", err)
		http.Error(w, "Failed to decode admission review", http.StatusBadRequest)
		return
	}

	response := app.processAdmissionReview(&admissionReview)
	responseBytes, err := json.Marshal(response)
	if err != nil {
		app.logger.Printf("Failed to encode admission response: %v", err)
		http.Error(w, "Failed to encode admission response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(responseBytes)
}

func (app *AppContext) processAdmissionReview(admissionReview *admissionv1.AdmissionReview) *admissionv1.AdmissionReview {
	response := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionReview.APIVersion,
			Kind:       admissionReview.Kind,
		},
		Response: &admissionv1.AdmissionResponse{
			UID: admissionReview.Request.UID,
		},
	}

	var pod corev1.Pod
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &pod); err != nil {
		app.logger.Printf("Failed to unmarshal pod object: %v", err)
		response.Response.Allowed = false
		response.Response.Result = &metav1.Status{
			Message: fmt.Sprintf("Failed to unmarshal pod object: %v", err),
		}
		return response
	}

	// Process all containers in the pod
	for _, container := range pod.Spec.Containers {
		image := container.Image
		app.logger.Printf("Detected webhook pod with image: %s", image)

		if err := app.msgClient.Publish(image); err != nil {
			app.logger.Printf("Failed to publish image: %v", err)
		} else {
			app.logger.Printf("Published image: %s", image)
		}
	}

	response.Response.Allowed = true
	return response
}
