package webhook

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"kube-mqtt-mirror/pkg/messaging"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// Server represents a webhook server
type Server struct {
	server    *http.Server
	logger    *log.Logger
	msgClient messaging.Client
	certFile  string
	keyFile   string
	port      string
	enableTLS bool
}

// NewServer creates a new webhook server
func NewServer(logger *log.Logger, msgClient messaging.Client, port string, certFile, keyFile string) *Server {
	return &Server{
		logger:    logger,
		msgClient: msgClient,
		port:      port,
		certFile:  certFile,
		keyFile:   keyFile,
		enableTLS: certFile != "" && keyFile != "",
	}
}

// Start starts the webhook server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.handleWebhook)

	// Create listener first to get the actual port
	var listener net.Listener
	var err error

	if s.port == "0" {
		// Random port
		listener, err = net.Listen("tcp", "0.0.0.0:0")
	} else {
		listener, err = net.Listen("tcp", "0.0.0.0:"+s.port)
	}
	if err != nil {
		return fmt.Errorf("failed to create listener: %v", err)
	}

	s.server = &http.Server{
		Handler: mux,
	}

	// Get the actual port
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	if !s.enableTLS {
		s.logger.Printf("Starting webhook server without TLS on 0.0.0.0:%s", port)
		return s.server.Serve(listener)
	}

	// Load TLS certificate
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return fmt.Errorf("failed to load TLS certificate: %v", err)
	}

	s.logger.Printf("Starting webhook server with TLS on 0.0.0.0:%s", port)

	// Wrap listener with TLS
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{cert},
	})

	return s.server.Serve(tlsListener)
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	var admissionReview admissionv1.AdmissionReview
	decoder := serializer.NewCodecFactory(runtime.NewScheme()).UniversalDeserializer()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Printf("Failed to read request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if _, _, err := decoder.Decode(body, nil, &admissionReview); err != nil {
		s.logger.Printf("Failed to decode admission review: %v", err)
		http.Error(w, "Failed to decode admission review", http.StatusBadRequest)
		return
	}

	response := s.ProcessAdmissionReview(&admissionReview)
	responseBytes, err := json.Marshal(response)
	if err != nil {
		s.logger.Printf("Failed to encode admission response: %v", err)
		http.Error(w, "Failed to encode admission response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(responseBytes)
}

// ProcessAdmissionReview processes an admission review request
func (s *Server) ProcessAdmissionReview(admissionReview *admissionv1.AdmissionReview) *admissionv1.AdmissionReview {
	response := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionReview.APIVersion,
			Kind:       admissionReview.Kind,
		},
		Response: &admissionv1.AdmissionResponse{
			UID: admissionReview.Request.UID,
		},
	}

	var pod corev1.Pod
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &pod); err != nil {
		s.logger.Printf("Failed to unmarshal pod object: %v", err)
		response.Response.Allowed = false
		response.Response.Result = &metav1.Status{
			Message: fmt.Sprintf("Failed to unmarshal pod object: %v", err),
		}
		return response
	}

	// Process all containers in the pod
	for _, container := range pod.Spec.Containers {
		image := container.Image
		s.logger.Printf("Detected webhook pod with image: %s", image)

		if err := s.msgClient.Publish(image); err != nil {
			s.logger.Printf("Failed to publish image: %v", err)
		} else {
			s.logger.Printf("Published image: %s", image)
		}
	}

	response.Response.Allowed = true
	return response
}
