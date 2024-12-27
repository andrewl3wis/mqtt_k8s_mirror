package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	MQTT "github.com/eclipse/paho.mqtt.golang"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// Configuration
var (
	enableWebhook = flag.Bool("webhook", false, "Enable Kubernetes webhook monitoring")
	enableMirror  = flag.Bool("mirror", false, "Enable MQTT-based image mirroring")
	mqttBroker    = flag.String("mqtt-broker", "tcp://mqtt.broker.address:1883", "MQTT broker address")
	mqttTopic     = flag.String("mqtt-topic", "image/download", "MQTT topic for image download requests")
	localRegistry = flag.String("local-registry", "localhost:5000", "Local Docker registry address")
	webhookPort   = flag.String("webhook-port", "8443", "Port to listen for webhook calls")
)

// MQTT message payload structure
type ImageRequest struct {
	Image string `json:"image"`
}

// Queue for background image downloads
var (
	downloadQueue = make(chan string, 100) // Buffered channel to queue downloads
	wg            sync.WaitGroup           // WaitGroup to track background goroutines
)

func main() {
	flag.Parse()

	if !*enableWebhook && !*enableMirror {
		log.Fatal("At least one of --webhook or --mirror must be enabled")
	}

	// Start background worker to process download queue
	wg.Add(1)
	go processDownloadQueue()

	// Start MQTT-based image mirroring
	if *enableMirror {
		wg.Add(1)
		go func() {
			defer wg.Done()
			startMQTTMirroring()
		}()
	}

	// Start webhook server
	if *enableWebhook {
		wg.Add(1)
		go func() {
			defer wg.Done()
			startWebhookServer()
		}()
	}

	// Wait for all goroutines to finish
	wg.Wait()
}

// Start MQTT-based image mirroring
func startMQTTMirroring() {
	// Set up MQTT client options
	opts := MQTT.NewClientOptions().AddBroker(*mqttBroker)
	client := MQTT.NewClient(opts)

	// Connect to the MQTT broker
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		log.Fatalf("Failed to connect to MQTT broker: %v", token.Error())
	}
	log.Println("Connected to MQTT broker")

	// Subscribe to the MQTT topic
	if token := client.Subscribe(*mqttTopic, 0, onMessage); token.Wait() && token.Error() != nil {
		log.Fatalf("Failed to subscribe to topic: %v", token.Error())
	}
	log.Printf("Subscribed to topic: %s\n", *mqttTopic)

	// Keep the MQTT client running
	select {}
}

// MQTT message handler
func onMessage(client MQTT.Client, msg MQTT.Message) {
	var request ImageRequest
	if err := json.Unmarshal(msg.Payload(), &request); err != nil {
		log.Printf("Failed to parse MQTT message: %v", err)
		return
	}

	if request.Image == "" {
		log.Println("Invalid payload: 'image' key not found")
		return
	}

	// Check if the image exists in the local registry
	if imageExistsInLocalRegistry(request.Image) {
		log.Printf("Image found in local registry: %s\n", request.Image)
		return
	}

	// Queue the image for background download
	log.Printf("Queueing image for download: %s\n", request.Image)
	downloadQueue <- request.Image
}

// Process the download queue
func processDownloadQueue() {
	defer wg.Done()
	for image := range downloadQueue {
		log.Printf("Downloading image: %s\n", image)
		if err := copyImage(image, fmt.Sprintf("%s/%s", *localRegistry, image)); err != nil {
			log.Printf("Failed to download image: %v\n", err)
		} else {
			log.Printf("Successfully downloaded image: %s\n", image)
		}
	}
}

// Check if an image exists in the local registry
func imageExistsInLocalRegistry(image string) bool {
	// Create a new context
	ctx := context.Background()

	// Parse the image reference
	ref, err := alltransports.ParseImageName(fmt.Sprintf("docker://%s/%s", *localRegistry, image))
	if err != nil {
		log.Printf("Failed to parse image reference: %v\n", err)
		return false
	}

	// Check if the image exists
	_, err = ref.NewImageSource(ctx, &types.SystemContext{})
	return err == nil
}

// Copy an image from source to destination using containers/image
func copyImage(srcImage, destImage string) error {
	// Create a new context
	ctx := context.Background()

	// Parse the source and destination image references
	srcRef, err := alltransports.ParseImageName(fmt.Sprintf("docker://%s", srcImage))
	if err != nil {
		return fmt.Errorf("failed to parse source image: %v", err)
	}

	destRef, err := alltransports.ParseImageName(fmt.Sprintf("docker://%s", destImage))
	if err != nil {
		return fmt.Errorf("failed to parse destination image: %v", err)
	}

	// Create a policy context to allow all images
	policyContext, err := signature.NewPolicyContext(&signature.Policy{
		Default: []signature.PolicyRequirement{signature.NewPRInsecureAcceptAnything()},
	})
	if err != nil {
		return fmt.Errorf("failed to create policy context: %v", err)
	}

	// Fetch the source image's manifest
	srcImageSource, err := srcRef.NewImageSource(ctx, &types.SystemContext{})
	if err != nil {
		return fmt.Errorf("failed to create source image source: %v", err)
	}
	defer srcImageSource.Close()

	manifestBytes, manifestType, err := srcImageSource.GetManifest(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to fetch manifest: %v", err)
	}

	// Check if the manifest is a multi-arch manifest list
	if manifest.MIMETypeIsMultiArch(manifestType) {
		// Parse the manifest list
		manifestList, err := manifest.ListFromBlob(manifestBytes, manifestType)
		if err != nil {
			return fmt.Errorf("failed to parse manifest list: %v", err)
		}

		// Iterate through each manifest in the list
		for _, instance := range manifestList.Instances() {
			// Fetch the manifest for the specific architecture
			instanceManifestBytes, _, err := srcImageSource.GetManifest(ctx, &instance)
			if err != nil {
				log.Printf("Failed to fetch manifest for instance %s: %v\n", instance, err)
				continue
			}

			// Copy the image for the specific architecture
			if err := copyImageInstance(ctx, policyContext, srcRef, destRef, instanceManifestBytes); err != nil {
				log.Printf("Failed to copy instance %s: %v\n", instance, err)
			} else {
				log.Printf("Successfully copied instance %s\n", instance)
			}
		}
	} else {
		// Copy the single-arch image
		if err := copyImageInstance(ctx, policyContext, srcRef, destRef, manifestBytes); err != nil {
			return fmt.Errorf("failed to copy image: %v", err)
		}
	}

	return nil
}

// Copy a single instance of an image
func copyImageInstance(ctx context.Context, policyContext *signature.PolicyContext, srcRef, destRef types.ImageReference, manifestBytes []byte) error {
	// Create a new image from the manifest
	srcImage, err := image.FromManifest(ctx, srcRef, manifestBytes)
	if err != nil {
		return fmt.Errorf("failed to create source image: %v", err)
	}

	// Copy the image
	_, err = copy.Image(ctx, policyContext, destRef, srcImage, &copy.Options{
		ReportWriter: log.Writer(),
	})
	if err != nil {
		return fmt.Errorf("failed to copy image: %v", err)
	}

	return nil
}

// Start webhook server
func startWebhookServer() {
	http.HandleFunc("/webhook", handleWebhook)
	log.Printf("Webhook server listening on port %s\n", *webhookPort)
	if err := http.ListenAndServeTLS(fmt.Sprintf(":%s", *webhookPort), "server.crt", "server.key", nil); err != nil {
		log.Fatalf("Failed to start webhook server: %v", err)
	}
}

// Handle incoming webhook requests
func handleWebhook(w http.ResponseWriter, r *http.Request) {
	var admissionReview admissionv1.AdmissionReview
	decoder := serializer.NewCodecFactory(runtime.NewScheme()).UniversalDeserializer()
	body, err := readRequestBody(r)
	if err != nil {
		log.Printf("Failed to read request body: %v\n", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	if _, _, err := decoder.Decode(body, nil, &admissionReview); err != nil {
		log.Printf("Failed to decode admission review: %v\n", err)
		http.Error(w, "Failed to decode admission review", http.StatusBadRequest)
		return
	}

	// Process the admission review
	response := processAdmissionReview(&admissionReview)
	responseBytes, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to encode admission response: %v\n", err)
		http.Error(w, "Failed to encode admission response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(responseBytes)
}

// Read the request body
func readRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, fmt.Errorf("request body is empty")
	}
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

// Process the admission review
func processAdmissionReview(admissionReview *admissionv1.AdmissionReview) *admissionv1.AdmissionReview {
	response := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: admissionReview.APIVersion,
			Kind:       admissionReview.Kind,
		},
		Response: &admissionv1.AdmissionResponse{
			UID: admissionReview.Request.UID,
		},
	}

	// Extract the pod object
	var pod corev1.Pod
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &pod); err != nil {
		log.Printf("Failed to unmarshal pod object: %v\n", err)
		response.Response.Allowed = false
		response.Response.Result = &metav1.Status{
			Message: fmt.Sprintf("Failed to unmarshal pod object: %v", err),
		}
		return response
	}

	// Check if the pod has a webhook annotation
	if pod.Annotations["webhook"] == "true" {
		for _, container := range pod.Spec.Containers {
			image := container.Image
			log.Printf("Detected webhook pod with image: %s\n", image)

			// Publish the image to the MQTT topic for mirroring
			payload, _ := json.Marshal(ImageRequest{Image: image})
			token := mqttClient.Publish(*mqttTopic, 0, false, payload)
			token.Wait()
			log.Printf("Published image to MQTT topic: %s\n", image)
		}
	}

	response.Response.Allowed = true
	return response
}
