package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"kube-mqtt-mirror/pkg/messaging"
)

// QueueClient implements a simple, efficient queue for single-instance use
type QueueClient struct {
	path      string
	topic     string
	logger    *log.Logger
	handler   func(messaging.ImageRequest) error
	mu        sync.RWMutex
	stopChan  chan struct{}
	messages  []string
	persisted *os.File
	inMemory  bool
}

// NewClient creates a new queue client
func NewClient(config messaging.Config, logger *log.Logger) messaging.Client {
	// Default to in-memory if no broker specified
	inMemory := config.Broker == "" || config.Broker == ":memory:"
	queuePath := "queue.json"

	// Use specified path if provided and not in-memory
	if !inMemory && config.Broker != "" {
		queuePath = config.Broker
	}

	return &QueueClient{
		path:     queuePath,
		topic:    config.Topic,
		logger:   logger,
		stopChan: make(chan struct{}),
		inMemory: inMemory,
	}
}

func (q *QueueClient) Connect(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Initialize empty message queue
	q.messages = make([]string, 0)

	// Skip file operations for in-memory queue
	if q.inMemory {
		q.logger.Printf("Using in-memory queue")
		return nil
	}

	// Create directory if needed
	dir := filepath.Dir(q.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	// Open persistence file
	file, err := os.OpenFile(q.path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("failed to open queue file: %v", err)
	}
	q.persisted = file

	// Load existing messages
	decoder := json.NewDecoder(file)
	var messages []string
	if err := decoder.Decode(&messages); err != nil && err.Error() != "EOF" {
		file.Close()
		return fmt.Errorf("failed to load messages: %v", err)
	}
	if messages != nil {
		q.messages = messages
		q.logger.Printf("Loaded %d messages from %s", len(messages), q.path)
	}

	q.logger.Printf("Using file-backed queue at %s", q.path)
	return nil
}

func (q *QueueClient) Disconnect() {
	close(q.stopChan)
	if q.persisted != nil {
		q.persisted.Close()
	}
}

func (q *QueueClient) persistMessages() error {
	// Skip persistence for in-memory queue
	if q.inMemory {
		return nil
	}

	// Skip if no file handle
	if q.persisted == nil {
		return fmt.Errorf("no persistence file available")
	}

	q.mu.RLock()
	messages := make([]string, len(q.messages))
	copy(messages, q.messages)
	q.mu.RUnlock()

	// Truncate and write
	if err := q.persisted.Truncate(0); err != nil {
		return fmt.Errorf("failed to truncate file: %v", err)
	}
	if _, err := q.persisted.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek: %v", err)
	}

	encoder := json.NewEncoder(q.persisted)
	if err := encoder.Encode(messages); err != nil {
		return fmt.Errorf("failed to persist messages: %v", err)
	}

	q.logger.Printf("Persisted %d messages", len(messages))
	return nil
}

func (q *QueueClient) Publish(image string) error {
	payload, err := json.Marshal(messaging.ImageRequest{Image: image})
	if err != nil {
		return fmt.Errorf("failed to marshal image request: %v", err)
	}

	q.mu.Lock()
	q.messages = append(q.messages, string(payload))
	q.mu.Unlock()

	// Persist in background
	if !q.inMemory {
		go func() {
			if err := q.persistMessages(); err != nil {
				q.logger.Printf("Failed to persist messages: %v", err)
			}
		}()
	}

	return nil
}

func (q *QueueClient) Subscribe(handler func(messaging.ImageRequest) error) error {
	q.handler = handler

	// Process messages in background
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-q.stopChan:
				return
			case <-ticker.C:
				// Get next message
				q.mu.Lock()
				if len(q.messages) == 0 {
					q.mu.Unlock()
					continue
				}

				// Process first message
				payload := q.messages[0]
				q.messages = q.messages[1:]
				q.mu.Unlock()

				// Parse and handle
				var req messaging.ImageRequest
				if err := json.Unmarshal([]byte(payload), &req); err != nil {
					q.logger.Printf("Failed to unmarshal message: %v", err)
					continue
				}

				if err := q.handler(req); err != nil {
					q.logger.Printf("Handler error: %v", err)
					// Re-queue on error
					q.mu.Lock()
					q.messages = append(q.messages, payload)
					q.mu.Unlock()
				}

				// Persist changes
				if !q.inMemory {
					go func() {
						if err := q.persistMessages(); err != nil {
							q.logger.Printf("Failed to persist messages: %v", err)
						}
					}()
				}
			}
		}
	}()

	return nil
}
