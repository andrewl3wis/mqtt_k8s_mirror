package sqlite

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"kube-mqtt-mirror/pkg/messaging"
)

func TestSQLiteClient(t *testing.T) {
	t.Run("In-Memory Database", func(t *testing.T) {
		testSQLiteClient(t, ":memory:")
	})

	t.Run("File Database", func(t *testing.T) {
		dbFile := "test.db"
		testSQLiteClient(t, dbFile)
		os.Remove(dbFile)
	})
}

func testSQLiteClient(t *testing.T, dbPath string) {
	// Create client
	logger := log.New(os.Stdout, "sqlite-test: ", log.LstdFlags)
	client := NewClient(messaging.Config{
		Broker: dbPath,
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

	// Test error handling
	db, ok := client.(*sqliteClient)
	if !ok {
		t.Fatal("Failed to cast client to sqliteClient")
	}

	// Test invalid JSON
	_, err := db.db.Exec("INSERT INTO messages (topic, payload) VALUES (?, ?)", "test/images", "{invalid json}")
	if err != nil {
		t.Fatalf("Failed to insert invalid JSON: %v", err)
	}

	// Wait a bit to ensure message is processed
	time.Sleep(100 * time.Millisecond)

	// Test cleanup
	if dbPath != ":memory:" {
		client.Disconnect()
		// Wait a bit for WAL files to be cleaned up
		time.Sleep(100 * time.Millisecond)
		// Check for main db file and WAL files
		for _, file := range []string{dbPath, dbPath + "-shm", dbPath + "-wal"} {
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				os.Remove(file) // Clean up any remaining files
			}
		}
	}
}

func TestSQLiteClientErrors(t *testing.T) {
	// Test invalid database path
	logger := log.New(os.Stdout, "sqlite-test: ", log.LstdFlags)
	client := NewClient(messaging.Config{
		Broker: "/nonexistent/path/db.sqlite",
		Topic:  "test/images",
	}, logger)

	if err := client.Connect(context.Background()); err == nil {
		t.Error("Expected error for invalid database path")
	}

	// Test invalid table creation
	client = NewClient(messaging.Config{
		Broker: ":memory:",
		Topic:  "", // Invalid topic
	}, logger)

	if err := client.Connect(context.Background()); err == nil {
		t.Error("Expected error for invalid topic")
	}
}
