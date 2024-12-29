package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"kube-mqtt-mirror/pkg/messaging"
)

// QueueClient implements a simple, in-memory queue
type QueueClient struct {
	topic    string
	logger   *log.Logger
	handler  func(messaging.ImageRequest) error
	mu       sync.RWMutex
	stopChan chan struct{}
	messages []string
	running  bool
	msgChan  chan string
}

// NewClient creates a new queue client
func NewClient(config messaging.Config, logger *log.Logger) messaging.Client {
	return &QueueClient{
		topic:    config.Topic,
		logger:   logger,
		stopChan: make(chan struct{}),
		messages: make([]string, 0, 100), // Pre-allocate space for messages
		msgChan:  make(chan string, 100), // Buffered channel for message processing
	}
}

func (q *QueueClient) Connect(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.running {
		return fmt.Errorf("queue already connected")
	}

	q.running = true
	q.logger.Printf("Using in-memory queue")
	return nil
}

func (q *QueueClient) Disconnect() {
	q.mu.Lock()
	if !q.running {
		q.mu.Unlock()
		return
	}
	q.running = false
	close(q.stopChan)
	close(q.msgChan)
	q.mu.Unlock()
}

func (q *QueueClient) Publish(image string) error {
	q.mu.RLock()
	if !q.running {
		q.mu.RUnlock()
		return fmt.Errorf("queue not connected")
	}
	q.mu.RUnlock()

	payload, err := json.Marshal(messaging.ImageRequest{Image: image})
	if err != nil {
		return fmt.Errorf("failed to marshal image request: %v", err)
	}

	// Send directly to processing channel
	select {
	case q.msgChan <- string(payload):
		return nil
	case <-q.stopChan:
		return fmt.Errorf("queue stopped")
	default:
		// Channel full, use backup slice
		q.mu.Lock()
		q.messages = append(q.messages, string(payload))
		q.mu.Unlock()
		return nil
	}
}

func (q *QueueClient) Subscribe(handler func(messaging.ImageRequest) error) error {
	q.mu.Lock()
	if !q.running {
		q.mu.Unlock()
		return fmt.Errorf("queue not connected")
	}
	if q.handler != nil {
		q.mu.Unlock()
		return fmt.Errorf("already subscribed")
	}
	q.handler = handler
	q.mu.Unlock()

	// Process messages in background
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond) // Faster ticker for backup slice
		defer ticker.Stop()

		for {
			q.mu.RLock()
			if !q.running {
				q.mu.RUnlock()
				return
			}
			q.mu.RUnlock()

			select {
			case <-q.stopChan:
				return
			case payload := <-q.msgChan:
				// Process message from channel
				q.processMessage(payload)
			case <-ticker.C:
				// Check backup slice
				q.mu.Lock()
				if len(q.messages) > 0 {
					payload := q.messages[0]
					q.messages = q.messages[1:]
					q.mu.Unlock()
					q.processMessage(payload)
				} else {
					q.mu.Unlock()
				}
			}
		}
	}()

	return nil
}

func (q *QueueClient) processMessage(payload string) {
	var req messaging.ImageRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		q.logger.Printf("Failed to unmarshal message: %v", err)
		return
	}

	if err := q.handler(req); err != nil {
		q.logger.Printf("Handler error: %v", err)
		// Re-queue on error using channel first
		select {
		case q.msgChan <- payload:
		default:
			// Channel full, use backup slice
			q.mu.Lock()
			q.messages = append(q.messages, payload)
			q.mu.Unlock()
		}
	}
}
