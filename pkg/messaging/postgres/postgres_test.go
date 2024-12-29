package postgres

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"kube-mqtt-mirror/pkg/messaging"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPostgresClient(t *testing.T) {
	// Create new mock database
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("Failed to create mock: %v", err)
	}
	defer db.Close()

	// Create test logger
	logger := log.New(os.Stdout, "[Postgres Test] ", log.LstdFlags)

	// Create test client
	client := &PostgresClient{
		config: messaging.Config{
			Broker:   "mock://localhost:5432",
			Topic:    "test/images",
			Username: "test",
			Password: "test",
		},
		db:     db,
		logger: logger,
	}

	// Test publishing
	t.Run("Publish", func(t *testing.T) {
		testImage := "nginx:latest"

		// Expect insert query
		mock.ExpectExec("INSERT INTO messages \\(topic, payload\\) VALUES \\(\\$1, \\$2\\)").
			WithArgs("test/images", `{"image":"nginx:latest"}`).
			WillReturnResult(sqlmock.NewResult(1, 1))

		if err := client.Publish(testImage); err != nil {
			t.Errorf("Failed to publish: %v", err)
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("Unfulfilled expectations: %v", err)
		}
	})

	// Test subscribing
	t.Run("Subscribe", func(t *testing.T) {
		// Create new mock for subscribe test
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("Failed to create mock: %v", err)
		}
		defer db.Close()

		client := &PostgresClient{
			config: messaging.Config{
				Broker:   "mock://localhost:5432",
				Topic:    "test/images",
				Username: "test",
				Password: "test",
			},
			db:     db,
			logger: logger,
		}

		// Expect select query
		rows := sqlmock.NewRows([]string{"payload"}).
			AddRow(`{"image":"nginx:latest"}`).
			AddRow(`{"image":"redis:alpine"}`)

		mock.ExpectQuery("SELECT payload FROM messages WHERE topic = \\$1 ORDER BY created_at ASC").
			WithArgs("test/images").
			WillReturnRows(rows)

		// Expect delete queries
		mock.ExpectExec("DELETE FROM messages WHERE topic = \\$1 AND payload = \\$2").
			WithArgs("test/images", `{"image":"nginx:latest"}`).
			WillReturnResult(sqlmock.NewResult(0, 1))

		mock.ExpectExec("DELETE FROM messages WHERE topic = \\$1 AND payload = \\$2").
			WithArgs("test/images", `{"image":"redis:alpine"}`).
			WillReturnResult(sqlmock.NewResult(0, 1))

		// Track received messages
		received := make([]string, 0)
		handler := func(req messaging.ImageRequest) error {
			received = append(received, req.Image)
			return nil
		}

		// Subscribe and process messages
		if err := client.Subscribe(handler); err != nil {
			t.Errorf("Failed to subscribe: %v", err)
		}

		// Wait for messages to be processed
		time.Sleep(200 * time.Millisecond)

		// Verify received messages
		expected := []string{"nginx:latest", "redis:alpine"}
		if len(received) != len(expected) {
			t.Errorf("Got %d messages, want %d", len(received), len(expected))
		}
		for i, msg := range received {
			if msg != expected[i] {
				t.Errorf("Message %d: got %q, want %q", i, msg, expected[i])
			}
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("Unfulfilled expectations: %v", err)
		}
	})
}

func TestPostgresClientValidation(t *testing.T) {
	testCases := []struct {
		name        string
		config      messaging.Config
		expectError bool
	}{
		{
			name: "Valid Config",
			config: messaging.Config{
				Broker:   "localhost:5432",
				Topic:    "test",
				Username: "user",
				Password: "pass",
			},
			expectError: false,
		},
		{
			name: "Empty Broker",
			config: messaging.Config{
				Topic:    "test",
				Username: "user",
				Password: "pass",
			},
			expectError: true,
		},
		{
			name: "Empty Username",
			config: messaging.Config{
				Broker:   "localhost:5432",
				Topic:    "test",
				Password: "pass",
			},
			expectError: true,
		},
		{
			name: "Empty Password",
			config: messaging.Config{
				Broker:   "localhost:5432",
				Topic:    "test",
				Username: "user",
			},
			expectError: true,
		},
		{
			name: "Empty Topic",
			config: messaging.Config{
				Broker:   "localhost:5432",
				Username: "user",
				Password: "pass",
			},
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create mock DB for valid config test
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("Failed to create mock: %v", err)
			}
			defer db.Close()

			// Create client with mock DB
			client := &PostgresClient{
				config: tc.config,
				db:     db,
				logger: log.New(os.Stdout, fmt.Sprintf("[%s] ", tc.name), log.LstdFlags),
			}

			// Test connection
			err = client.Connect(context.Background())

			if tc.expectError && err == nil {
				t.Error("Expected error but got nil")
			}
			if !tc.expectError && err != nil {
				t.Errorf("Expected no error but got: %v", err)
			}

			// Verify all expectations were met
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("Unfulfilled expectations: %v", err)
			}
		})
	}
}
