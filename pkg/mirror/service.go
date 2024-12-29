package mirror

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"kube-mqtt-mirror/pkg/image"
	"kube-mqtt-mirror/pkg/messaging"
)

// Service handles image mirroring operations
type Service struct {
	logger        *log.Logger
	msgClient     messaging.Client
	imgOps        *image.Operations
	downloadQueue chan string
	wg            *sync.WaitGroup
	localRegistry string
}

// NewService creates a new mirror service
func NewService(logger *log.Logger, msgClient messaging.Client, imgOps *image.Operations, localRegistry string) *Service {
	return &Service{
		logger:        logger,
		msgClient:     msgClient,
		imgOps:        imgOps,
		downloadQueue: make(chan string, 100),
		wg:            &sync.WaitGroup{},
		localRegistry: localRegistry,
	}
}

// Start starts the mirror service
func (s *Service) Start(ctx context.Context) error {
	// Start background worker to process download queue
	s.wg.Add(1)
	go s.processDownloadQueue(ctx)

	// Start subscription in a goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- s.msgClient.Subscribe(func(req messaging.ImageRequest) error {
			select {
			case <-ctx.Done():
				return nil
			default:
				if req.Image == "" {
					s.logger.Println("Invalid payload: 'image' key not found")
					return nil
				}

				if s.imgOps.ImageExists(req.Image) {
					s.logger.Printf("Image found in local registry: %s", req.Image)
					return nil
				}

				s.logger.Printf("Queueing image for download: %s", req.Image)
				select {
				case s.downloadQueue <- req.Image:
					return nil
				case <-ctx.Done():
					return nil
				}
			}
		})
	}()

	// Wait for subscription to be established or context to be cancelled
	select {
	case err := <-errChan:
		if err != nil {
			return fmt.Errorf("failed to subscribe: %v", err)
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	s.logger.Printf("Mirror service started")
	return nil
}

// Shutdown gracefully shuts down the service
func (s *Service) Shutdown() {
	// Close download queue
	close(s.downloadQueue)

	// Wait for all goroutines to finish
	s.wg.Wait()
}

func (s *Service) processDownloadQueue(ctx context.Context) {
	defer s.wg.Done()

	// Track in-progress downloads to prevent duplicates
	inProgress := make(map[string]bool)
	var mu sync.Mutex

	// Rate limiter for concurrent downloads
	maxConcurrent := make(chan struct{}, 3) // Allow 3 concurrent downloads

	for {
		select {
		case <-ctx.Done():
			return
		case image, ok := <-s.downloadQueue:
			if !ok {
				return
			}

			// Check if already downloading
			mu.Lock()
			if inProgress[image] {
				s.logger.Printf("Image already being downloaded: %s", image)
				mu.Unlock()
				continue
			}
			inProgress[image] = true
			mu.Unlock()

			s.wg.Add(1)
			go func(img string) {
				defer s.wg.Done()
				defer func() {
					mu.Lock()
					delete(inProgress, img)
					mu.Unlock()
				}()

				// Rate limit concurrent downloads
				maxConcurrent <- struct{}{}
				defer func() { <-maxConcurrent }()

				// Try up to 3 times with exponential backoff
				backoff := time.Second
				for attempt := 1; attempt <= 3; attempt++ {
					destImage := fmt.Sprintf("%s/%s", s.localRegistry, img)
					if err := s.imgOps.CopyImage(img, destImage); err != nil {
						if attempt == 3 {
							s.logger.Printf("Failed to download image after %d attempts: %v", attempt, err)
							return
						}
						s.logger.Printf("Download attempt %d failed: %v, retrying in %v", attempt, err, backoff)
						time.Sleep(backoff)
						backoff *= 2
						continue
					}
					s.logger.Printf("Successfully downloaded image: %s", img)
					return
				}
			}(image)
		}
	}
}
