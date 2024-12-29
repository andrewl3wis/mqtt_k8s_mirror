package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"kube-mqtt-mirror/pkg/image"
	"kube-mqtt-mirror/pkg/messaging"
	"kube-mqtt-mirror/pkg/messaging/mqtt"
	"kube-mqtt-mirror/pkg/messaging/postgres"
	"kube-mqtt-mirror/pkg/messaging/queue"
	"kube-mqtt-mirror/pkg/mirror"
	"kube-mqtt-mirror/pkg/webhook"
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

	// Set up graceful shutdown
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// Create services
	logger := log.Default()
	var wg sync.WaitGroup

	// Create messaging client
	msgClient, err := createMessagingClient(config, logger)
	if err != nil {
		logger.Fatalf("Failed to create messaging client: %v", err)
	}

	// Connect to messaging system
	if err := msgClient.Connect(ctx); err != nil {
		logger.Fatalf("Failed to connect to messaging system: %v", err)
	}
	defer msgClient.Disconnect()

	// Create image operations
	imgOps := image.NewOperations(logger, config.LocalRegistry, config.InsecureRegistries)

	// Start mirror service if enabled
	if config.EnableMirror {
		mirrorService := mirror.NewService(logger, msgClient, imgOps, config.LocalRegistry)
		if err := mirrorService.Start(ctx); err != nil {
			logger.Fatalf("Failed to start mirror service: %v", err)
		}
		defer mirrorService.Shutdown()
	}

	// Start webhook server if enabled
	if config.EnableWebhook {
		webhookServer := webhook.NewServer(logger, msgClient, config.WebhookPort, config.TLSCertPath, config.TLSKeyPath)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := webhookServer.Start(ctx); err != nil {
				logger.Printf("Webhook server error: %v", err)
			}
		}()
		defer webhookServer.Shutdown(ctx)
	}

	// Wait for shutdown signal
	<-signalChan
	logger.Println("Shutdown signal received, initiating graceful shutdown...")

	// Cancel context to initiate shutdown
	cancel()

	// Wait for all goroutines to finish
	wg.Wait()
	logger.Println("Graceful shutdown completed")
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
	flag.StringVar(&config.TLSCertPath, "tls-cert", "", "Path to TLS certificate (enables TLS if provided)")
	flag.StringVar(&config.TLSKeyPath, "tls-key", "", "Path to TLS private key (enables TLS if provided)")
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

func createMessagingClient(config Config, logger *log.Logger) (messaging.Client, error) {
	msgConfig := messaging.Config{
		Broker:   config.MessagingBroker,
		Topic:    config.MessagingTopic,
		Username: config.MessagingUsername,
		Password: config.MessagingPassword,
	}

	switch config.MessagingType {
	case "queue":
		return queue.NewClient(msgConfig, logger), nil
	case "mqtt":
		return mqtt.NewClient(msgConfig, logger), nil
	case "postgres":
		return postgres.NewClient(msgConfig, logger), nil
	default:
		return nil, fmt.Errorf("unsupported messaging type: %s (must be queue, mqtt, or postgres)", config.MessagingType)
	}
}
