package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kube-mqtt-mirror/pkg/image"
	"kube-mqtt-mirror/pkg/mirror"
	"kube-mqtt-mirror/pkg/webhook"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestImageMirroringWithAllBackends(t *testing.T) {
	testCases := []struct {
		name          string
		messagingType string
		broker        string
		tlsCert       string
		tlsKey        string
	}{
		{
			name:          "Queue Backend with TLS",
			messagingType: "queue",
			broker:        "", // In-memory only
			tlsCert:       "server.crt",
			tlsKey:        "server.key",
		},
		{
			name:          "Queue Backend without TLS",
			messagingType: "queue",
			broker:        "", // In-memory only
			tlsCert:       "", // No TLS paths = no TLS
			tlsKey:        "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create test directory
			testDir, err := os.MkdirTemp("", "mirror-test-*")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(testDir)

			// Create test logger
			logger := log.New(os.Stdout, fmt.Sprintf("[%s] ", tc.name), log.LstdFlags)

			// Generate test certificates (even if not used)
			var certPath, keyPath string
			if tc.tlsCert != "" && tc.tlsKey != "" {
				certPath = filepath.Join(testDir, tc.tlsCert)
				keyPath = filepath.Join(testDir, tc.tlsKey)
				if err := generateTestCerts(certPath, keyPath); err != nil {
					t.Fatalf("Failed to generate test certificates: %v", err)
				}
			}

			// Create app context with short timeout
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Create configuration
			config := Config{
				EnableWebhook:      true,
				EnableMirror:       true,
				MessagingType:      tc.messagingType,
				MessagingBroker:    tc.broker,
				MessagingTopic:     "test/images",
				LocalRegistry:      "mock://localhost:5000",
				WebhookPort:        "0", // Use random port
				InsecureRegistries: true,
				TLSCertPath:        certPath,
				TLSKeyPath:         keyPath,
			}

			// Create messaging client
			msgClient, err := createMessagingClient(config, logger)
			if err != nil {
				t.Fatalf("Failed to create messaging client: %v", err)
			}

			// Connect to messaging system
			if err := msgClient.Connect(ctx); err != nil {
				t.Fatalf("Failed to connect to messaging system: %v", err)
			}
			defer msgClient.Disconnect()

			// Create image operations with mocks
			imgOps := image.NewOperations(logger, config.LocalRegistry, config.InsecureRegistries)

			// Create mirror service
			mirrorService := mirror.NewService(logger, msgClient, imgOps, config.LocalRegistry)
			if err := mirrorService.Start(ctx); err != nil {
				t.Fatalf("Failed to start mirror service: %v", err)
			}
			defer mirrorService.Shutdown()

			// Create webhook server
			webhookServer := webhook.NewServer(logger, msgClient, config.WebhookPort, config.TLSCertPath, config.TLSKeyPath)

			// Start webhook server
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := webhookServer.Start(ctx); err != nil {
					logger.Printf("Webhook server error: %v", err)
				}
			}()

			// Test webhook with single pod
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "web", Image: "nginx:alpine"},
					},
				},
			}

			// Create admission review and test webhook
			review := &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					UID: "test",
					Object: runtime.RawExtension{
						Raw: mustMarshal(t, pod),
					},
				},
			}

			// Give webhook server time to start
			time.Sleep(100 * time.Millisecond)

			// Process review
			response := webhookServer.ProcessAdmissionReview(review)
			if !response.Response.Allowed {
				t.Errorf("Pod not allowed: %s", response.Response.Result.Message)
			}

			// Clean shutdown
			cancel()

			// Stop webhook server
			if err := webhookServer.Shutdown(ctx); err != nil {
				logger.Printf("Failed to shutdown webhook server: %v", err)
			}

			// Wait for all goroutines with timeout
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				logger.Printf("Clean shutdown completed")
			case <-time.After(time.Second):
				t.Fatal("Timeout waiting for shutdown")
			}
		})
	}
}

func generateTestCerts(certPath, keyPath string) error {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %v", err)
	}

	certOut, err := os.Create(certPath)
	if err != nil {
		return fmt.Errorf("failed to create cert file: %v", err)
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("failed to encode certificate: %v", err)
	}

	keyOut, err := os.Create(keyPath)
	if err != nil {
		return fmt.Errorf("failed to create key file: %v", err)
	}
	defer keyOut.Close()

	if err := pem.Encode(keyOut, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}); err != nil {
		return fmt.Errorf("failed to encode private key: %v", err)
	}

	return nil
}

func mustMarshal(t *testing.T, v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}
	return data
}
