package messaging

import (
	"encoding/json"
	"testing"
)

func TestImageRequest(t *testing.T) {
	testCases := []struct {
		name     string
		image    string
		expected string
	}{
		{
			name:     "Simple Image",
			image:    "nginx:latest",
			expected: `{"image":"nginx:latest"}`,
		},
		{
			name:     "Multi-arch Image",
			image:    "docker.io/library/postgres:14",
			expected: `{"image":"docker.io/library/postgres:14"}`,
		},
		{
			name:     "Private Registry",
			image:    "registry.example.com/app:v1.0",
			expected: `{"image":"registry.example.com/app:v1.0"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test marshaling
			req := ImageRequest{Image: tc.image}
			data, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("Failed to marshal: %v", err)
			}
			if string(data) != tc.expected {
				t.Errorf("Got %q, want %q", string(data), tc.expected)
			}

			// Test unmarshaling
			var decoded ImageRequest
			if err := json.Unmarshal([]byte(tc.expected), &decoded); err != nil {
				t.Fatalf("Failed to unmarshal: %v", err)
			}
			if decoded.Image != tc.image {
				t.Errorf("Got %q, want %q", decoded.Image, tc.image)
			}
		})
	}
}

func TestConfig(t *testing.T) {
	testCases := []struct {
		name     string
		config   Config
		validate func(Config) error
	}{
		{
			name: "Full Config",
			config: Config{
				Broker:   "localhost:1883",
				Topic:    "images",
				Username: "user",
				Password: "pass",
			},
			validate: func(c Config) error {
				if c.Broker != "localhost:1883" {
					t.Errorf("Wrong broker: got %q", c.Broker)
				}
				if c.Topic != "images" {
					t.Errorf("Wrong topic: got %q", c.Topic)
				}
				if c.Username != "user" {
					t.Errorf("Wrong username: got %q", c.Username)
				}
				if c.Password != "pass" {
					t.Errorf("Wrong password: got %q", c.Password)
				}
				return nil
			},
		},
		{
			name: "Minimal Config",
			config: Config{
				Broker: "localhost:1883",
				Topic:  "images",
			},
			validate: func(c Config) error {
				if c.Broker != "localhost:1883" {
					t.Errorf("Wrong broker: got %q", c.Broker)
				}
				if c.Topic != "images" {
					t.Errorf("Wrong topic: got %q", c.Topic)
				}
				if c.Username != "" {
					t.Errorf("Expected empty username, got %q", c.Username)
				}
				if c.Password != "" {
					t.Errorf("Expected empty password, got %q", c.Password)
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.validate(tc.config); err != nil {
				t.Errorf("Validation failed: %v", err)
			}
		})
	}
}
