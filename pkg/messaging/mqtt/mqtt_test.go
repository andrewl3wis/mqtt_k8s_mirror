package mqtt

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"kube-mqtt-mirror/pkg/messaging"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

func TestMQTTClientValidation(t *testing.T) {
	logger := log.New(os.Stdout, "mqtt-test: ", log.LstdFlags)

	tests := []struct {
		name       string
		config     messaging.Config
		wantErrMsg string
	}{
		{
			name: "Empty broker",
			config: messaging.Config{
				Topic: "test/images",
			},
			wantErrMsg: "broker URL is required",
		},
		{
			name: "Empty topic",
			config: messaging.Config{
				Broker: "tcp://localhost:1883",
			},
			wantErrMsg: "topic is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(tt.config, logger)
			err := client.Connect(context.Background())
			if err == nil {
				t.Error("Expected error but got nil")
			} else if !strings.Contains(err.Error(), tt.wantErrMsg) {
				t.Errorf("Expected error containing %q, got %q", tt.wantErrMsg, err.Error())
			}
		})
	}
}

func TestMQTTClient(t *testing.T) {
	// Skip if no test broker available
	broker := os.Getenv("TEST_MQTT_BROKER")
	if broker == "" {
		// Start embedded MQTT server
		server := mqtt.New(&mqtt.Options{})
		_ = server.AddHook(new(auth.AllowHook), nil)

		// Create MQTT listener with random port
		portChan := make(chan int, 1)
		errChan := make(chan error, 1)

		go func() {
			tcp := listeners.NewTCP(listeners.Config{
				ID:      "t1",
				Address: "127.0.0.1:0",
			})

			if err := server.AddListener(tcp); err != nil {
				errChan <- err
				return
			}

			// Get assigned port
			addr := tcp.Address()
			var port int
			if _, err := fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &port); err != nil {
				errChan <- err
				return
			}
			portChan <- port

			if err := server.Serve(); err != nil {
				errChan <- err
			}
		}()

		// Wait for server to start
		select {
		case err := <-errChan:
			t.Fatalf("Failed to start MQTT server: %v", err)
		case port := <-portChan:
			broker = fmt.Sprintf("tcp://127.0.0.1:%d", port)
		case <-time.After(5 * time.Second):
			t.Fatal("Timeout waiting for MQTT server to start")
		}
	}

	// Create client
	logger := log.New(os.Stdout, "mqtt-test: ", log.LstdFlags)
	client := NewClient(messaging.Config{
		Broker: broker,
		Topic:  "test/images",
	}, logger)

	// Connect
	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer client.Disconnect()

	// Test Subscribe
	receivedChan := make(chan string, 1)
	if err := client.Subscribe(func(req messaging.ImageRequest) error {
		receivedChan <- req.Image
		return nil
	}); err != nil {
		t.Fatalf("Failed to subscribe: %v", err)
	}

	// Test Publish
	testImage := "test/image:latest"
	if err := client.Publish(testImage); err != nil {
		t.Fatalf("Failed to publish: %v", err)
	}

	// Wait for message
	select {
	case received := <-receivedChan:
		if received != testImage {
			t.Errorf("Expected image %q, got %q", testImage, received)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for message")
	}
}
