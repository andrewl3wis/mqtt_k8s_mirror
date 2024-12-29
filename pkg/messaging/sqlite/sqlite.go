package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"kube-mqtt-mirror/pkg/messaging"

	_ "modernc.org/sqlite"
)

type sqliteClient struct {
	db       *sql.DB
	config   messaging.Config
	logger   *log.Logger
	handler  func(messaging.ImageRequest) error
	stopChan chan struct{}
}

// NewClient creates a new SQLite messaging client
func NewClient(config messaging.Config, logger *log.Logger) messaging.Client {
	return &sqliteClient{
		config:   config,
		logger:   logger,
		stopChan: make(chan struct{}),
	}
}

func (s *sqliteClient) Connect(ctx context.Context) error {
	// Validate config
	if s.config.Topic == "" {
		return fmt.Errorf("topic is required")
	}

	// Open SQLite database with shared cache for in-memory database
	dsn := s.config.Broker
	if dsn == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to open SQLite database: %v", err)
	}

	// Enable WAL mode and set pragmas
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA cache_size=10000",
	}

	for _, pragma := range pragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return fmt.Errorf("failed to set pragma %s: %v", pragma, err)
		}
	}

	// Create messages table if it doesn't exist
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			topic TEXT NOT NULL,
			payload TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		db.Close()
		return fmt.Errorf("failed to create messages table: %v", err)
	}

	// Verify table exists by attempting a simple query
	if _, err := db.QueryContext(ctx, "SELECT 1 FROM messages LIMIT 1"); err != nil {
		db.Close()
		return fmt.Errorf("failed to verify messages table: %v", err)
	}

	s.db = db
	return nil
}

func (s *sqliteClient) Disconnect() {
	select {
	case <-s.stopChan:
		// Channel already closed
	default:
		close(s.stopChan)
	}
	if s.db != nil {
		s.db.Close()
		s.db = nil
	}
}

func (s *sqliteClient) Publish(image string) error {
	payload, err := json.Marshal(messaging.ImageRequest{Image: image})
	if err != nil {
		return fmt.Errorf("failed to marshal image request: %v", err)
	}

	// Retry logic for database locks
	maxRetries := 5
	backoff := 100 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Use a transaction for atomic insert
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("failed to begin transaction: %v", err)
		}
		defer tx.Rollback()

		_, err = tx.Exec(
			"INSERT INTO messages (topic, payload) VALUES (?, ?)",
			s.config.Topic,
			string(payload),
		)
		if err != nil {
			if err.Error() == "database is locked" {
				tx.Rollback()
				time.Sleep(backoff)
				backoff *= 2 // Exponential backoff
				continue
			}
			return fmt.Errorf("failed to insert message: %v", err)
		}

		if err := tx.Commit(); err != nil {
			if err.Error() == "database is locked" {
				time.Sleep(backoff)
				backoff *= 2 // Exponential backoff
				continue
			}
			return fmt.Errorf("failed to commit transaction: %v", err)
		}

		return nil
	}

	return fmt.Errorf("failed to publish after %d retries: database is locked", maxRetries)
}

func (s *sqliteClient) Subscribe(handler func(messaging.ImageRequest) error) error {
	s.handler = handler

	// Start polling for new messages
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		var lastID int64
		for {
			select {
			case <-s.stopChan:
				return
			case <-ticker.C:
				// Query for new messages
				rows, err := s.db.Query(
					"SELECT id, payload FROM messages WHERE topic = ? AND id > ? ORDER BY id",
					s.config.Topic,
					lastID,
				)
				if err != nil {
					s.logger.Printf("Failed to query messages: %v", err)
					continue
				}

				for rows.Next() {
					var id int64
					var payload string
					if err := rows.Scan(&id, &payload); err != nil {
						s.logger.Printf("Failed to scan message: %v", err)
						continue
					}

					var req messaging.ImageRequest
					if err := json.Unmarshal([]byte(payload), &req); err != nil {
						s.logger.Printf("Failed to unmarshal message: %v", err)
						continue
					}

					if err := s.handler(req); err != nil {
						s.logger.Printf("Handler error: %v", err)
					}

					lastID = id
				}
				rows.Close()
			}
		}
	}()

	return nil
}
