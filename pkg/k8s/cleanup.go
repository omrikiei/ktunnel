// Package k8s provides Kubernetes integration functionality for ktunnel
package k8s

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceTracker keeps track of resources created by ktunnel for cleanup
type ResourceTracker struct {
	clients     *Clients
	namespace   string
	deployments []string
	services    []string
	mu          sync.Mutex
	timeout     time.Duration
}

// NewResourceTracker creates a new ResourceTracker for the given namespace
func NewResourceTracker(namespace string, clients *Clients) *ResourceTracker {
	return &ResourceTracker{
		clients:     clients,
		namespace:   namespace,
		deployments: make([]string, 0),
		services:    make([]string, 0),
		timeout:     30 * time.Second, // Default timeout for cleanup operations
	}
}

// SetTimeout sets the timeout duration for cleanup operations
func (rt *ResourceTracker) SetTimeout(timeout time.Duration) {
	rt.timeout = timeout
}

// AddDeployment adds a deployment to be tracked for cleanup
func (rt *ResourceTracker) AddDeployment(name string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.deployments = append(rt.deployments, name)
	log.Debugf("Added deployment %s to cleanup tracker", name)
}

// AddService adds a service to be tracked for cleanup
func (rt *ResourceTracker) AddService(name string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.services = append(rt.services, name)
	log.Debugf("Added service %s to cleanup tracker", name)
}

// RemoveDeployment removes a deployment from tracking
func (rt *ResourceTracker) RemoveDeployment(name string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i, d := range rt.deployments {
		if d == name {
			rt.deployments = append(rt.deployments[:i], rt.deployments[i+1:]...)
			log.Debugf("Removed deployment %s from cleanup tracker", name)
			return
		}
	}
}

// RemoveService removes a service from tracking
func (rt *ResourceTracker) RemoveService(name string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i, s := range rt.services {
		if s == name {
			rt.services = append(rt.services[:i], rt.services[i+1:]...)
			log.Debugf("Removed service %s from cleanup tracker", name)
			return
		}
	}
}

// Cleanup removes all tracked resources
func (rt *ResourceTracker) Cleanup(ctx context.Context) error {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(ctx, rt.timeout)
	defer cancel()

	var wg sync.WaitGroup
	errChan := make(chan error, 100) // Buffer for potential errors

	// Get a snapshot of resources to clean up
	rt.mu.Lock()
	deployments := make([]string, len(rt.deployments))
	services := make([]string, len(rt.services))
	copy(deployments, rt.deployments)
	copy(services, rt.services)
	rt.mu.Unlock()

	// Clean up deployments
	for _, deployment := range deployments {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			err := rt.clients.Deployments.Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil {
				log.Warnf("Failed to delete deployment %s: %v", name, err)
				select {
				case errChan <- err:
				default:
				}
			} else {
				log.Infof("Deleted deployment %s", name)
			}
		}(deployment)
	}

	// Clean up services
	for _, service := range services {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			err := rt.clients.Services.Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil {
				log.Warnf("Failed to delete service %s: %v", name, err)
				select {
				case errChan <- err:
				default:
				}
			} else {
				log.Infof("Deleted service %s", name)
			}
		}(service)
	}

	// Wait for all deletions to complete or context to timeout
	doneChan := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneChan)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-doneChan:
		// Check if there were any errors
		close(errChan)
		var errs []error
		for err := range errChan {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			log.Errorf("Encountered %d errors during cleanup", len(errs))
			return errs[0] // Return the first error for now
		}
		return nil
	}
}

// StartCleanupOnSignal starts a goroutine that will clean up resources when a shutdown signal is received
func (rt *ResourceTracker) StartCleanupOnSignal() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Infof("Received %v signal, cleaning up resources...", sig)

		ctx := context.Background()
		if err := rt.Cleanup(ctx); err != nil {
			if err == context.DeadlineExceeded {
				log.Errorf("Cleanup timed out after %v", rt.timeout)
			} else {
				log.Errorf("Failed to clean up resources: %v", err)
			}
			os.Exit(1)
		}

		log.Info("Cleanup completed successfully")
		os.Exit(0)
	}()
}

// GetTrackedResources returns the currently tracked resources (useful for testing)
func (rt *ResourceTracker) GetTrackedResources() ([]string, []string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	deployments := make([]string, len(rt.deployments))
	services := make([]string, len(rt.services))
	copy(deployments, rt.deployments)
	copy(services, rt.services)
	return deployments, services
}
