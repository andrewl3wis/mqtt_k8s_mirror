package postgres

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"kube-mqtt-mirror/pkg/messaging"

	_ "github.com/lib/pq"
)

func TestPostgresClientValidation(t *testing.T) {
	logger := log.New(os.Stdout, "postgres-test: ", log.LstdFlags)

	tests := []struct {
		name       string
		config     messaging.Config
		wantErrMsg string
	}{
		{
			name: "Empty broker",
			config: messaging.Config{
				Username: "test",
				Password: "test",
				Topic:    "test/images",
			},
			wantErrMsg: "broker address is required",
		},
		{
			name: "Empty username",
			config: messaging.Config{
				Broker:   "localhost:5432",
				Password: "test",
				Topic:    "test/images",
			},
			wantErrMsg: "username is required",
		},
		{
			name: "Empty password",
			config: messaging.Config{
				Broker:   "localhost:5432",
				Username: "test",
				Topic:    "test/images",
			},
			wantErrMsg: "password is required",
		},
		{
			name: "Empty topic",
			config: messaging.Config{
				Broker:   "localhost:5432",
				Username: "test",
				Password: "test",
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

func TestPostgresClient(t *testing.T) {
	// Skip if no test database available
	testDB := os.Getenv("TEST_POSTGRES_DB")
	if testDB == "" {
		t.Skip("Skipping PostgreSQL test: TEST_POSTGRES_DB not set")
	}

	// Create client
	logger := log.New(os.Stdout, "postgres-test: ", log.LstdFlags)
	client := NewClient(messaging.Config{
		Broker:   os.Getenv("TEST_POSTGRES_HOST"),
		Username: os.Getenv("TEST_POSTGRES_USER"),
		Password: os.Getenv("TEST_POSTGRES_PASSWORD"),
		Topic:    "test/images",
	}, logger)

	// Connect
	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}
	defer client.Disconnect()

	// Create test table
	db, ok := client.(*postgresClient)
	if !ok {
		t.Fatal("Failed to cast client to postgresClient")
	}

	_, err := db.db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id SERIAL PRIMARY KEY,
			topic TEXT NOT NULL,
			payload TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}

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

	// Clean up
	_, err = db.db.Exec("DROP TABLE messages")
	if err != nil {
		t.Errorf("Failed to drop table: %v", err)
	}
}
