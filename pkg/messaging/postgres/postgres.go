package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"kube-mqtt-mirror/pkg/messaging"

	"github.com/lib/pq"
)

type postgresClient struct {
	db       *sql.DB
	listener *pq.Listener
	config   messaging.Config
	logger   *log.Logger
	handler  func(messaging.ImageRequest) error
}

// NewClient creates a new PostgreSQL messaging client
func NewClient(config messaging.Config, logger *log.Logger) messaging.Client {
	return &postgresClient{
		config: config,
		logger: logger,
	}
}

func (p *postgresClient) Connect(ctx context.Context) error {
	// Validate config
	if p.config.Broker == "" {
		return fmt.Errorf("broker address is required")
	}
	if p.config.Username == "" {
		return fmt.Errorf("username is required")
	}
	if p.config.Password == "" {
		return fmt.Errorf("password is required")
	}
	if p.config.Topic == "" {
		return fmt.Errorf("topic is required")
	}

	// Convert messaging config to postgres connection string
	connStr := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		p.config.Username,
		p.config.Password,
		p.config.Broker,
		"postgres", // default database
	)

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("failed to connect to postgres: %v", err)
	}

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("failed to ping postgres: %v", err)
	}

	p.db = db

	// Create listener
	p.listener = pq.NewListener(connStr,
		10*time.Second, time.Minute,
		func(ev pq.ListenerEventType, err error) {
			if err != nil {
				p.logger.Printf("Listener error: %v\n", err)
			}
		})

	// Listen for notifications
	if err := p.listener.Listen(p.config.Topic); err != nil {
		return fmt.Errorf("failed to listen on channel: %v", err)
	}

	return nil
}

func (p *postgresClient) Disconnect() {
	if p.listener != nil {
		p.listener.Close()
	}
	if p.db != nil {
		p.db.Close()
	}
}

func (p *postgresClient) Publish(image string) error {
	payload, err := json.Marshal(messaging.ImageRequest{Image: image})
	if err != nil {
		return fmt.Errorf("failed to marshal image request: %v", err)
	}

	// Use NOTIFY to publish message
	query := fmt.Sprintf("SELECT pg_notify('%s', $1)", p.config.Topic)
	if _, err := p.db.Exec(query, string(payload)); err != nil {
		return fmt.Errorf("failed to notify: %v", err)
	}

	return nil
}

func (p *postgresClient) Subscribe(handler func(messaging.ImageRequest) error) error {
	p.handler = handler

	// Start listening for notifications in a goroutine
	go func() {
		for notification := range p.listener.Notify {
			if notification == nil {
				continue // channel was closed
			}

			var req messaging.ImageRequest
			if err := json.Unmarshal([]byte(notification.Extra), &req); err != nil {
				p.logger.Printf("Failed to unmarshal notification: %v", err)
				continue
			}

			if err := p.handler(req); err != nil {
				p.logger.Printf("Handler error: %v", err)
			}
		}
	}()

	return nil
}
