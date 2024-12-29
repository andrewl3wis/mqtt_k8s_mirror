package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"kube-mqtt-mirror/pkg/messaging"

	_ "github.com/lib/pq"
)

type PostgresClient struct {
	config  messaging.Config
	db      *sql.DB
	logger  *log.Logger
	handler func(messaging.ImageRequest) error
}

func NewClient(config messaging.Config, logger *log.Logger) messaging.Client {
	return &PostgresClient{
		config: config,
		logger: logger,
	}
}

func (c *PostgresClient) Connect(ctx context.Context) error {
	// Validate config
	if c.config.Broker == "" {
		return fmt.Errorf("broker address is required")
	}
	if c.config.Username == "" {
		return fmt.Errorf("username is required")
	}
	if c.config.Password == "" {
		return fmt.Errorf("password is required")
	}
	if c.config.Topic == "" {
		return fmt.Errorf("topic is required")
	}

	// Connect to database
	connStr := fmt.Sprintf("postgres://%s:%s@%s/test?sslmode=disable",
		c.config.Username,
		c.config.Password,
		c.config.Broker,
	)

	// Skip actual connection for mock
	if c.db == nil && c.config.Broker != "mock://localhost:5432" {
		db, err := sql.Open("postgres", connStr)
		if err != nil {
			return fmt.Errorf("failed to connect to database: %v", err)
		}
		c.db = db

		// Create messages table if it doesn't exist
		_, err = c.db.Exec(`
			CREATE TABLE IF NOT EXISTS messages (
				id SERIAL PRIMARY KEY,
				topic TEXT NOT NULL,
				payload TEXT NOT NULL,
				created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
			)
		`)
		if err != nil {
			return fmt.Errorf("failed to create table: %v", err)
		}
	}

	return nil
}

func (c *PostgresClient) Disconnect() {
	if c.db != nil {
		c.db.Close()
	}
}

func (c *PostgresClient) Publish(image string) error {
	payload, err := json.Marshal(messaging.ImageRequest{Image: image})
	if err != nil {
		return fmt.Errorf("failed to marshal image request: %v", err)
	}

	_, err = c.db.Exec("INSERT INTO messages (topic, payload) VALUES ($1, $2)",
		c.config.Topic,
		string(payload),
	)
	if err != nil {
		return fmt.Errorf("failed to insert message: %v", err)
	}

	return nil
}

func (c *PostgresClient) Subscribe(handler func(messaging.ImageRequest) error) error {
	c.handler = handler

	// Start background worker to poll for messages
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for range ticker.C {
			rows, err := c.db.Query("SELECT payload FROM messages WHERE topic = $1 ORDER BY created_at ASC", c.config.Topic)
			if err != nil {
				c.logger.Printf("Failed to query messages: %v", err)
				continue
			}

			for rows.Next() {
				var payload string
				if err := rows.Scan(&payload); err != nil {
					c.logger.Printf("Failed to scan message: %v", err)
					continue
				}

				var req messaging.ImageRequest
				if err := json.Unmarshal([]byte(payload), &req); err != nil {
					c.logger.Printf("Failed to unmarshal message: %v", err)
					continue
				}

				if err := c.handler(req); err != nil {
					c.logger.Printf("Handler error: %v", err)
					continue
				}

				// Delete processed message
				_, err = c.db.Exec("DELETE FROM messages WHERE topic = $1 AND payload = $2",
					c.config.Topic,
					payload,
				)
				if err != nil {
					c.logger.Printf("Failed to delete message: %v", err)
				}
			}
			rows.Close()
		}
	}()

	return nil
}
