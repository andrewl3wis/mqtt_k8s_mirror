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

			// Generate test certificates (even if not used)
			certPath := filepath.Join(testDir, "server.crt")
			keyPath := filepath.Join(testDir, "server.key")
			if err := generateTestCerts(certPath, keyPath); err != nil {
				t.Fatalf("Failed to generate test certificates: %v", err)
			}

			// Create test logger
			logger := log.New(os.Stdout, fmt.Sprintf("[%s] ", tc.name), log.LstdFlags)

			// Create app context with short timeout
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Start services
			app := &AppContext{
				config: Config{
					EnableWebhook:      true,
					EnableMirror:       true,
					MessagingType:      tc.messagingType,
					MessagingBroker:    tc.broker,
					MessagingTopic:     "test/images",
					LocalRegistry:      "mock://localhost:5000",
					WebhookPort:        "0", // Use random port
					InsecureRegistries: true,
					TLSCertPath:        tc.tlsCert,
					TLSKeyPath:         tc.tlsKey,
				},
				downloadQueue: make(chan string, 100),
				wg:            &sync.WaitGroup{},
				logger:        logger,
			}

			// Mock image operations
			app.imageExistsInLocalRegistry = func(image string) bool {
				return true // Always return true to speed up tests
			}
			app.copyImage = func(srcImage, destImage string) error {
				return nil // No-op for tests
			}

			// Start services with context
			errChan := make(chan error, 1)
			go func() {
				errChan <- app.startServices(ctx)
			}()

			// Wait for server to start or error
			select {
			case err := <-errChan:
				if err != nil {
					t.Fatalf("Failed to start services: %v", err)
				}
			case <-time.After(time.Second):
				// Server started successfully
			}

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

			// Create admission review
			review := &admissionv1.AdmissionReview{
				Request: &admissionv1.AdmissionRequest{
					UID: "test",
					Object: runtime.RawExtension{
						Raw: mustMarshal(t, pod),
					},
				},
			}

			// Send webhook request
			resp := app.processAdmissionReview(review)
			if !resp.Response.Allowed {
				t.Errorf("Pod not allowed: %s", resp.Response.Result.Message)
			}

			// Clean shutdown
			cancel()

			// Stop webhook server
			if app.server != nil {
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
				defer shutdownCancel()
				if err := app.server.Shutdown(shutdownCtx); err != nil {
					logger.Printf("Failed to shutdown webhook server: %v", err)
				}
			}

			// Stop messaging client
			if app.msgClient != nil {
				app.msgClient.Disconnect()
			}

			// Close download queue
			close(app.downloadQueue)

			// Wait for all goroutines with timeout
			done := make(chan struct{})
			go func() {
				app.wg.Wait()
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
