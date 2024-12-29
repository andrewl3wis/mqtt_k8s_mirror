package image

import (
	"fmt"
	"log"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Operations handles container image operations
type Operations struct {
	logger           *log.Logger
	localRegistry    string
	insecureRegistry bool
	registryOptions  []name.Option
	transportOptions []remote.Option
}

// NewOperations creates a new image operations handler
func NewOperations(logger *log.Logger, localRegistry string, insecureRegistry bool) *Operations {
	var registryOptions []name.Option
	if insecureRegistry {
		registryOptions = append(registryOptions, name.Insecure)
	}

	return &Operations{
		logger:           logger,
		localRegistry:    localRegistry,
		insecureRegistry: insecureRegistry,
		registryOptions:  registryOptions,
		transportOptions: []remote.Option{
			remote.WithAuthFromKeychain(authn.DefaultKeychain),
		},
	}
}

// CopyImage copies an image from source to destination registry
func (o *Operations) CopyImage(srcImage, destImage string) error {
	// Parse source reference
	srcRef, err := name.ParseReference(srcImage, o.registryOptions...)
	if err != nil {
		return fmt.Errorf("failed to parse source image: %v", err)
	}

	// Parse destination reference
	destRef, err := name.ParseReference(destImage, o.registryOptions...)
	if err != nil {
		return fmt.Errorf("failed to parse destination image: %v", err)
	}

	o.logger.Printf("Starting image copy from %s to %s", srcImage, destImage)

	// Try to get the source as an index first (multi-arch)
	srcIdx, err := remote.Index(srcRef, o.transportOptions...)
	if err == nil {
		o.logger.Printf("Source is a multi-arch image")
		// Copy the entire index with all architectures
		if err := remote.WriteIndex(destRef, srcIdx, o.transportOptions...); err != nil {
			return fmt.Errorf("failed to push multi-arch image: %v", err)
		}
		o.logger.Printf("Multi-arch image copy completed successfully: %s -> %s", srcImage, destImage)
		return nil
	}

	// If not an index, try as a single image
	srcImg, err := remote.Image(srcRef, o.transportOptions...)
	if err != nil {
		return fmt.Errorf("failed to get source image: %v", err)
	}

	// Get image config to check platform
	config, err := srcImg.ConfigFile()
	if err != nil {
		return fmt.Errorf("failed to get image config: %v", err)
	}
	o.logger.Printf("Source is a single-arch image for %s/%s", config.OS, config.Architecture)

	// Push the single image
	if err := remote.Write(destRef, srcImg, o.transportOptions...); err != nil {
		return fmt.Errorf("failed to push image: %v", err)
	}

	o.logger.Printf("Single-arch image copy completed successfully: %s -> %s", srcImage, destImage)
	return nil
}

// ImageExists checks if an image exists in the local registry
func (o *Operations) ImageExists(image string) bool {
	ref, err := name.ParseReference(fmt.Sprintf("%s/%s", o.localRegistry, image), o.registryOptions...)
	if err != nil {
		o.logger.Printf("Failed to parse image reference: %v", err)
		return false
	}

	// Try as index first
	if _, err := remote.Index(ref, o.transportOptions...); err == nil {
		return true
	}

	// Try as single image
	if _, err := remote.Head(ref, o.transportOptions...); err == nil {
		return true
	}

	return false
}
