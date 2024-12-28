package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

func TestImageMirroringWithAllBackends(t *testing.T) {
	// Test configuration for each backend
	testCases := []struct {
		name          string
		messagingType string
		broker        string
		setup         func() error
		cleanup       func() error
	}{
		{
			name:          "SQLite Backend",
			messagingType: "sqlite",
			broker:        ":memory:",
			setup:         func() error { return nil },
			cleanup:       func() error { return nil },
		},
		{
			name:          "SQLite File Backend",
			messagingType: "sqlite",
			broker:        "test.db",
			setup:         func() error { return nil },
			cleanup: func() error {
				return os.Remove("test.db")
			},
		},
		{
			name:          "PostgreSQL Backend",
			messagingType: "postgres",
			broker:        "", // Set during setup
			setup: func() error {
				// Pull PostgreSQL image
				cmd := exec.Command("docker", "pull", "postgres:14-alpine")
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("failed to pull postgres image: %v: %s", err, out)
				}

				// Start PostgreSQL container
				cmd = exec.Command("docker", "run", "-d", "--rm",
					"-e", "POSTGRES_USER=test",
					"-e", "POSTGRES_PASSWORD=test",
					"-e", "POSTGRES_DB=test",
					"-P", "postgres:14-alpine")
				out, err := cmd.CombinedOutput()
				if err != nil {
					return fmt.Errorf("failed to start postgres container: %v: %s", err, out)
				}
				return nil
			},
			cleanup: func() error {
				// Stop PostgreSQL container
				cmd := exec.Command("docker", "ps", "-q", "-f", "ancestor=postgres:14-alpine")
				out, err := cmd.Output()
				if err != nil {
					return fmt.Errorf("failed to find postgres container: %v", err)
				}
				containerId := strings.TrimSpace(string(out))
				if containerId != "" {
					cmd = exec.Command("docker", "stop", containerId)
					if err := cmd.Run(); err != nil {
						return fmt.Errorf("failed to stop postgres container: %v", err)
					}
				}
				return nil
			},
		},
		{
			name:          "Local Registry Backend",
			messagingType: "registry",
			broker:        "", // Set during setup
			setup: func() error {
				// Pull registry image
				cmd := exec.Command("docker", "pull", "registry:2")
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("failed to pull registry image: %v: %s", err, out)
				}

				// Start local registry container
				cmd = exec.Command("docker", "run", "-d", "--rm",
					"-p", "5000:5000",
					"registry:2")
				out, err := cmd.CombinedOutput()
				if err != nil {
					return fmt.Errorf("failed to start registry container: %v: %s", err, out)
				}
				return nil
			},
			cleanup: func() error {
				// Stop registry container
				cmd := exec.Command("docker", "ps", "-q", "-f", "ancestor=registry:2")
				out, err := cmd.Output()
				if err != nil {
					return fmt.Errorf("failed to find registry container: %v", err)
				}
				containerId := strings.TrimSpace(string(out))
				if containerId != "" {
					cmd = exec.Command("docker", "stop", containerId)
					if err := cmd.Run(); err != nil {
						return fmt.Errorf("failed to stop registry container: %v", err)
					}
				}
				return nil
			},
		},
		{
			name:          "Embedded MQTT Backend",
			messagingType: "mqtt",
			broker:        "", // Set during setup
			setup: func() error {
				return nil
			},
			cleanup: func() error {
				// MQTT server cleanup is handled by Go runtime
				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create app context
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create test logger
			logger := log.New(os.Stdout, fmt.Sprintf("test-%s: ", tc.name), log.LstdFlags)

			// Create temporary directory for test registry
			testDir, err := os.MkdirTemp("", "registry-test-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(testDir)

			// Create a mock registry check function
			var imageExistsMu sync.Mutex
			imageExists := make(map[string]bool)
			mockImageCheck := func(image string) bool {
				imageExistsMu.Lock()
				exists := imageExists[image]
				if !exists {
					// Simulate image being downloaded
					go func() {
						time.Sleep(100 * time.Millisecond)
						imageExistsMu.Lock()
						imageExists[image] = true
						imageExistsMu.Unlock()
					}()
				}
				imageExistsMu.Unlock()
				return exists
			}

			// Create app context
			app := &AppContext{
				config: Config{
					EnableMirror:       true,
					MessagingType:      tc.messagingType,
					MessagingBroker:    tc.broker,
					MessagingTopic:     "test/images",
					LocalRegistry:      filepath.Join(testDir, "registry"),
					InsecureRegistries: true,
				},
				downloadQueue: make(chan string, 100),
				wg:            &sync.WaitGroup{},
				logger:        logger,
			}

			// Setup test environment
			if tc.messagingType == "registry" {
				// Wait for registry container to start
				var containerId string
				var lastError error
				for i := 0; i < 30; i++ {
					cmd := exec.Command("docker", "ps", "-q", "-f", "ancestor=registry:2")
					if out, err := cmd.CombinedOutput(); err != nil {
						lastError = fmt.Errorf("docker ps failed: %v: %s", err, out)
					} else {
						containerId = strings.TrimSpace(string(out))
						if containerId != "" {
							break
						}
					}
					time.Sleep(1 * time.Second)
				}
				if containerId == "" {
					if lastError != nil {
						t.Fatalf("Registry container not found after waiting: %v", lastError)
					} else {
						t.Fatal("Registry container not found after waiting")
					}
				}

				// Set connection details
				tc.broker = "localhost:5000"

				// Wait for registry to be ready
				var lastCurlError error
				for i := 0; i < 30; i++ {
					cmd := exec.Command("curl", "-f", "http://localhost:5000/v2/")
					if out, err := cmd.CombinedOutput(); err == nil {
						break
					} else {
						lastCurlError = fmt.Errorf("curl failed: %v: %s", err, out)
					}
					time.Sleep(1 * time.Second)
				}
				if lastCurlError != nil {
					t.Fatalf("Registry not ready after waiting: %v", lastCurlError)
				}
			} else if tc.messagingType == "postgres" {
				// Wait for PostgreSQL container to start
				var containerId string
				var lastError error
				for i := 0; i < 30; i++ {
					cmd := exec.Command("docker", "ps", "-q", "-f", "ancestor=postgres:14-alpine")
					if out, err := cmd.CombinedOutput(); err != nil {
						lastError = fmt.Errorf("docker ps failed: %v: %s", err, out)
					} else {
						containerId = strings.TrimSpace(string(out))
						if containerId != "" {
							break
						}
					}
					time.Sleep(1 * time.Second)
				}
				if containerId == "" {
					if lastError != nil {
						t.Fatalf("PostgreSQL container not found after waiting: %v", lastError)
					} else {
						t.Fatal("PostgreSQL container not found after waiting")
					}
				}

				// Wait for port to be available
				var port string
				var lastPortError error
				for i := 0; i < 30; i++ {
					cmd := exec.Command("docker", "port", containerId, "5432")
					if out, err := cmd.CombinedOutput(); err != nil {
						lastPortError = fmt.Errorf("docker port failed: %v: %s", err, out)
					} else {
						port = strings.TrimSpace(strings.Split(string(out), ":")[1])
						if port != "" {
							break
						}
					}
					time.Sleep(1 * time.Second)
				}
				if port == "" {
					if lastPortError != nil {
						t.Fatalf("Failed to get PostgreSQL port after waiting: %v", lastPortError)
					} else {
						t.Fatal("Failed to get PostgreSQL port after waiting")
					}
				}

				// Set connection details
				tc.broker = fmt.Sprintf("localhost:%s", port)
				app.config.MessagingUsername = "test"
				app.config.MessagingPassword = "test"

				// Create test table
				db, err := sql.Open("postgres", fmt.Sprintf("postgres://test:test@%s/test?sslmode=disable", tc.broker))
				if err != nil {
					t.Fatalf("Failed to connect to postgres: %v", err)
				}
				defer db.Close()

				_, err = db.Exec(`
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
			} else if tc.messagingType == "mqtt" {
				// Create MQTT server
				server := mqtt.New(&mqtt.Options{})

				// Allow all connections
				_ = server.AddHook(new(auth.AllowHook), nil)

				// Create MQTT listener with random port
				portChan := make(chan int, 1)
				errChan := make(chan error, 1)

				go func() {
					tcp := listeners.NewTCP(listeners.Config{
						ID:      "t1",
						Address: "127.0.0.1:0",
					})

					if err := server.AddListener(tcp); err != nil {
						errChan <- fmt.Errorf("failed to add MQTT listener: %v", err)
						return
					}

					// Get the assigned port before starting the server
					addr := strings.Split(tcp.Address(), ":")[1]
					port := 0
					fmt.Sscanf(addr, "%d", &port)
					portChan <- port

					if err := server.Serve(); err != nil {
						errChan <- fmt.Errorf("MQTT server error: %v", err)
					}
				}()

				// Wait for server to start
				select {
				case err := <-errChan:
					t.Fatalf("Failed to start MQTT server: %v", err)
				case port := <-portChan:
					tc.broker = fmt.Sprintf("tcp://127.0.0.1:%d", port)
				case <-time.After(5 * time.Second):
					t.Fatal("Timeout waiting for MQTT server to start")
				}
			} else {
				if err := tc.setup(); err != nil {
					t.Fatalf("Failed to setup test: %v", err)
				}
			}
			defer tc.cleanup()

			// Set mock functions
			app.imageExistsInLocalRegistry = mockImageCheck
			app.copyImage = func(srcImage, destImage string) error {
				return nil // Simulate successful image copy
			}

			// Start services
			app.wg.Add(1)
			go app.processDownloadQueue(ctx)

			// For PostgreSQL, wait a bit for the server to be ready
			if tc.messagingType == "postgres" {
				time.Sleep(2 * time.Second)
			}

			if err := app.startMessaging(); err != nil {
				t.Fatalf("Failed to start messaging: %v", err)
			}

			// Test images to mirror (with proper registry format)
			testImages := []string{
				"docker.io/library/nginx:latest",
				"docker.io/library/redis:alpine",
				"docker.io/library/postgres:13",
			}

			// Publish test images
			for _, image := range testImages {
				if err := app.msgClient.Publish(image); err != nil {
					t.Errorf("Failed to publish image %s: %v", image, err)
					continue
				}
				logger.Printf("Published image: %s", image)
			}

			// Wait for images to be processed
			timeout := time.After(30 * time.Second)
			processed := make(map[string]bool)

		checkLoop:
			for len(processed) < len(testImages) {
				select {
				case <-timeout:
					t.Fatalf("Timeout waiting for images. Processed %d out of %d", len(processed), len(testImages))
					break checkLoop
				case <-time.After(1 * time.Second):
					// Check each image
					for _, image := range testImages {
						if processed[image] {
							continue
						}
						if app.checkImageExists(image) {
							processed[image] = true
							logger.Printf("Image mirrored successfully: %s", image)
						}
					}
				}
			}

			// Verify all images were processed
			for _, image := range testImages {
				if !processed[image] {
					t.Errorf("Image not processed: %s", image)
				}
			}

			// Clean shutdown
			cancel()
			app.shutdown()
			app.wg.Wait()
		})
	}
}
