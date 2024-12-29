package queue

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"kube-mqtt-mirror/pkg/messaging"
)

func TestQueueClient(t *testing.T) {
	logger := log.New(os.Stdout, "[Queue Test] ", log.LstdFlags)
	client := NewClient(messaging.Config{
		Topic: "test",
	}, logger)

	// Connect
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer client.Disconnect()

	// Test message channel
	received := make(chan string, 1)
	if err := client.Subscribe(func(req messaging.ImageRequest) error {
		received <- req.Image
		return nil
	}); err != nil {
		t.Fatalf("Failed to subscribe: %v", err)
	}

	// Publish test message
	testImage := "test/image:latest"
	if err := client.Publish(testImage); err != nil {
		t.Fatalf("Failed to publish: %v", err)
	}

	// Wait for message
	select {
	case img := <-received:
		if img != testImage {
			t.Errorf("Got wrong image. Want %q, got %q", testImage, img)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for message")
	}
}

func TestQueueConcurrency(t *testing.T) {
	logger := log.New(os.Stdout, "[Queue Concurrency] ", log.LstdFlags)
	client := NewClient(messaging.Config{
		Topic: "test",
	}, logger)

	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer client.Disconnect()

	// Track received messages
	received := make(map[string]bool)
	var receivedMu sync.Mutex
	done := make(chan struct{})

	if err := client.Subscribe(func(req messaging.ImageRequest) error {
		receivedMu.Lock()
		received[req.Image] = true
		count := len(received)
		receivedMu.Unlock()

		if count == 100 {
			close(done)
		}
		return nil
	}); err != nil {
		t.Fatalf("Failed to subscribe: %v", err)
	}

	// Publish 100 messages concurrently
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			image := fmt.Sprintf("test/image:%d", i)
			if err := client.Publish(image); err != nil {
				t.Errorf("Failed to publish %s: %v", image, err)
			}
		}()
	}

	// Wait for all publishes to complete
	wg.Wait()

	// Wait for all messages to be processed
	select {
	case <-done:
		// Verify all messages were received
		receivedMu.Lock()
		defer receivedMu.Unlock()
		for i := 0; i < 100; i++ {
			image := fmt.Sprintf("test/image:%d", i)
			if !received[image] {
				t.Errorf("Missing message: %s", image)
			}
		}
	case <-time.After(10 * time.Second):
		receivedMu.Lock()
		count := len(received)
		missing := []string{}
		for i := 0; i < 100; i++ {
			image := fmt.Sprintf("test/image:%d", i)
			if !received[image] {
				missing = append(missing, image)
			}
		}
		receivedMu.Unlock()
		t.Fatalf("Timeout waiting for messages. Got %d out of 100. Missing: %v", count, missing)
	}
}
