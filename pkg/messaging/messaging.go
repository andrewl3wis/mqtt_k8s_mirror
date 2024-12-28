package messaging

import "context"

// ImageRequest represents a message payload for image mirroring
type ImageRequest struct {
	Image string `json:"image"`
}

// Client defines the interface for messaging systems
type Client interface {
	// Connect establishes connection to the messaging system
	Connect(ctx context.Context) error
	// Disconnect cleanly disconnects from the messaging system
	Disconnect()
	// Publish sends an image request to the messaging system
	Publish(image string) error
	// Subscribe starts listening for image requests
	Subscribe(handler func(ImageRequest) error) error
}

// Config holds common messaging configuration
type Config struct {
	BrokerType string // amqp, kafka, nats, sql, memory
	Broker     string
	Topic      string
	Username   string
	Password   string
}
