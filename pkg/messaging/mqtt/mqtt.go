package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"kube-mqtt-mirror/pkg/messaging"

	MQTT "github.com/eclipse/paho.mqtt.golang"
)

type mqttClient struct {
	client   MQTT.Client
	config   messaging.Config
	logger   *log.Logger
	handlers []func(messaging.ImageRequest) error
}

// NewClient creates a new MQTT messaging client
func NewClient(config messaging.Config, logger *log.Logger) messaging.Client {
	return &mqttClient{
		config: config,
		logger: logger,
	}
}

func (m *mqttClient) Connect(ctx context.Context) error {
	// Validate config
	if m.config.Broker == "" {
		return fmt.Errorf("broker URL is required")
	}
	if m.config.Topic == "" {
		return fmt.Errorf("topic is required")
	}

	opts := MQTT.NewClientOptions().
		AddBroker(m.config.Broker).
		SetClientID("kube-mirror").
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectionLostHandler(func(client MQTT.Client, err error) {
			m.logger.Printf("MQTT connection lost: %v", err)
		}).
		SetOnConnectHandler(func(client MQTT.Client) {
			m.logger.Println("Connected to MQTT broker")
		})

	if m.config.Username != "" {
		opts.SetUsername(m.config.Username)
		if m.config.Password != "" {
			opts.SetPassword(m.config.Password)
		}
		m.logger.Println("Using MQTT authentication")
	}

	m.client = MQTT.NewClient(opts)

	if token := m.client.Connect(); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to connect to MQTT broker: %v", token.Error())
	}

	return nil
}

func (m *mqttClient) Disconnect() {
	if m.client != nil && m.client.IsConnected() {
		m.client.Disconnect(250)
	}
}

func (m *mqttClient) Publish(image string) error {
	payload, err := json.Marshal(messaging.ImageRequest{Image: image})
	if err != nil {
		return fmt.Errorf("failed to marshal image request: %v", err)
	}

	if token := m.client.Publish(m.config.Topic, 0, false, payload); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to publish message: %v", token.Error())
	}

	return nil
}

func (m *mqttClient) Subscribe(handler func(messaging.ImageRequest) error) error {
	callback := func(client MQTT.Client, msg MQTT.Message) {
		var req messaging.ImageRequest
		if err := json.Unmarshal(msg.Payload(), &req); err != nil {
			m.logger.Printf("Failed to parse message: %v", err)
			return
		}

		if err := handler(req); err != nil {
			m.logger.Printf("Handler error: %v", err)
		}
	}

	if token := m.client.Subscribe(m.config.Topic, 0, callback); token.Wait() && token.Error() != nil {
		return fmt.Errorf("failed to subscribe: %v", token.Error())
	}

	return nil
}
