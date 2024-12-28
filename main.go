package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	MQTT "github.com/eclipse/paho.mqtt.golang"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// Config holds all configuration parameters
type Config struct {
	EnableWebhook      bool
	EnableMirror       bool
	MQTTBroker         string
	MQTTTopic          string
	LocalRegistry      string
	WebhookPort        string
	InsecureRegistries bool
	TLSCertPath        string
	TLSKeyPath         string
	LogFile            string
}

// ImageRequest represents the MQTT message payload structure
type ImageRequest struct {
	Image string `json:"image"`
}

// AppContext holds application-wide context and resources
type AppContext struct {
	config        Config
	downloadQueue chan string
	mqttClient    MQTT.Client
	wg            *sync.WaitGroup
	logger        *log.Logger
	server        *http.Server
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

	flag.BoolVar(&config.EnableWebhook, "webhook", false, "Enable Kubernetes webhook monitoring")
	flag.BoolVar(&config.EnableMirror, "mirror", false, "Enable MQTT-based image mirroring")
	flag.StringVar(&config.MQTTBroker, "mqtt-broker", "tcp://mqtt.broker.address:1883", "MQTT broker address")
	flag.StringVar(&config.MQTTTopic, "mqtt-topic", "image/download", "MQTT topic for image download requests")
	flag.StringVar(&config.LocalRegistry, "local-registry", "localhost:5000", "Local Docker registry address")
	flag.StringVar(&config.WebhookPort, "webhook-port", "8443", "Port to listen for webhook calls")
	flag.BoolVar(&config.InsecureRegistries, "insecure-registries", false, "Allow connections to insecure registries (HTTP)")
	flag.StringVar(&config.TLSCertPath, "tls-cert", "server.crt", "Path to TLS certificate file")
	flag.StringVar(&config.TLSKeyPath, "tls-key", "server.key", "Path to TLS key file")
	flag.StringVar(&config.LogFile, "log-file", "", "Path to log file (if empty, logs to stdout)")

	flag.Parse()

	if !config.EnableWebhook && !config.EnableMirror {
		log.Fatal("At least one of --webhook or --mirror must be enabled")
	}

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

	// Start MQTT-based image mirroring
	if app.config.EnableMirror {
		if err := app.startMQTTMirroring(ctx); err != nil {
			return fmt.Errorf("failed to start MQTT mirroring: %v", err)
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

	// Disconnect MQTT client if connected
	if app.mqttClient != nil && app.mqttClient.IsConnected() {
		app.mqttClient.Disconnect(250)
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

func (app *AppContext) startMQTTMirroring(ctx context.Context) error {
	opts := MQTT.NewClientOptions().
		AddBroker(app.config.MQTTBroker).
		SetClientID("mqtt-k8s-mirror").
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectionLostHandler(func(client MQTT.Client, err error) {
			app.logger.Printf("MQTT connection lost: %v", err)
		}).
		SetOnConnectHandler(func(client MQTT.Client) {
			app.logger.Println("Connected to MQTT broker")
		})

	app.mqttClient = MQTT.NewClient(opts)

	if token := app.mqttClient.Connect(); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to connect to MQTT broker: %v", token.Error())
	}

	if token := app.mqttClient.Subscribe(app.config.MQTTTopic, 0, app.onMessage); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to subscribe to topic: %v", token.Error())
	}

	app.logger.Printf("Subscribed to topic: %s", app.config.MQTTTopic)
	return nil
}

func (app *AppContext) onMessage(client MQTT.Client, msg MQTT.Message) {
	var request ImageRequest
	if err := json.Unmarshal(msg.Payload(), &request); err != nil {
		app.logger.Printf("Failed to parse MQTT message: %v", err)
		return
	}

	if request.Image == "" {
		app.logger.Println("Invalid payload: 'image' key not found")
		return
	}

	if app.imageExistsInLocalRegistry(request.Image) {
		app.logger.Printf("Image found in local registry: %s", request.Image)
		return
	}

	app.logger.Printf("Queueing image for download: %s", request.Image)
	app.downloadQueue <- request.Image
}

func (app *AppContext) processDownloadQueue(ctx context.Context) {
	defer app.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case image, ok := <-app.downloadQueue:
			if !ok {
				return
			}
			app.wg.Add(1)
			go func(img string) {
				defer app.wg.Done()
				if err := app.copyImage(img, fmt.Sprintf("%s/%s", app.config.LocalRegistry, img)); err != nil {
					app.logger.Printf("Failed to download image: %v", err)
				} else {
					app.logger.Printf("Successfully downloaded image: %s", img)
				}
			}(image)
		}
	}
}

func (app *AppContext) imageExistsInLocalRegistry(image string) bool {
	ctx := context.Background()
	sysCtx := &types.SystemContext{
		DockerInsecureSkipTLSVerify: types.NewOptionalBool(app.config.InsecureRegistries),
	}

	ref, err := alltransports.ParseImageName(fmt.Sprintf("docker://%s/%s", app.config.LocalRegistry, image))
	if err != nil {
		app.logger.Printf("Failed to parse image reference: %v", err)
		return false
	}

	_, err = ref.NewImageSource(ctx, sysCtx)
	return err == nil
}

func (app *AppContext) copyImage(srcImage, destImage string) error {
	ctx := context.Background()

	srcRef, err := alltransports.ParseImageName(fmt.Sprintf("docker://%s", srcImage))
	if err != nil {
		return fmt.Errorf("failed to parse source image: %v", err)
	}

	destRef, err := alltransports.ParseImageName(fmt.Sprintf("docker://%s", destImage))
	if err != nil {
		return fmt.Errorf("failed to parse destination image: %v", err)
	}

	sysCtx := &types.SystemContext{
		DockerInsecureSkipTLSVerify: types.NewOptionalBool(app.config.InsecureRegistries),
	}

	policyContext, err := signature.NewPolicyContext(&signature.Policy{
		Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()},
	})
	if err != nil {
		return fmt.Errorf("failed to create policy context: %v", err)
	}

	_, err = copy.Image(ctx, policyContext, destRef, srcRef, &copy.Options{
		ReportWriter:   app.logger.Writer(),
		SourceCtx:      sysCtx,
		DestinationCtx: sysCtx,
	})
	if err != nil {
		return fmt.Errorf("failed to copy image: %v", err)
	}

	return nil
}

func (app *AppContext) startWebhookServer(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", app.handleWebhook)

	app.server = &http.Server{
		Addr:    fmt.Sprintf(":%s", app.config.WebhookPort),
		Handler: mux,
	}

	app.wg.Add(1)
	go func() {
		defer app.wg.Done()
		app.logger.Printf("Webhook server listening on port %s", app.config.WebhookPort)
		if err := app.server.ListenAndServeTLS(app.config.TLSCertPath, app.config.TLSKeyPath); err != http.ErrServerClosed {
			app.logger.Printf("HTTP server error: %v", err)
		}
	}()

	return nil
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

	if pod.Annotations["webhook"] == "true" {
		for _, container := range pod.Spec.Containers {
			image := container.Image
			app.logger.Printf("Detected webhook pod with image: %s", image)

			payload, _ := json.Marshal(ImageRequest{Image: image})
			if token := app.mqttClient.Publish(app.config.MQTTTopic, 0, false, payload); token.Wait() && token.Error() != nil {
				app.logger.Printf("Failed to publish image to MQTT topic: %v", token.Error())
			} else {
				app.logger.Printf("Published image to MQTT topic: %s", image)
			}
		}
	}

	response.Response.Allowed = true
	return response
}
