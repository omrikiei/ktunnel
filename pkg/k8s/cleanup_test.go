package k8s

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "k8s.io/client-go/kubernetes/fake"
)

func TestResourceTracker_AddAndRemove(t *testing.T) {
	namespace := "test-namespace"
	fakeClient := testclient.NewSimpleClientset()
	// Initialize the clients
	deploymentsClient = fakeClient.AppsV1().Deployments(namespace)
	svcClient = fakeClient.CoreV1().Services(namespace)

	clients := &Clients{
		Deployments: deploymentsClient,
		Services:    svcClient,
	}
	rt := NewResourceTracker("test-namespace", clients)

	// Test adding resources
	rt.AddDeployment("test-deployment-1")
	rt.AddDeployment("test-deployment-2")
	rt.AddService("test-service-1")
	rt.AddService("test-service-2")

	deployments, services := rt.GetTrackedResources()

	if len(deployments) != 2 {
		t.Errorf("Expected 2 deployments, got %d", len(deployments))
	}
	if len(services) != 2 {
		t.Errorf("Expected 2 services, got %d", len(services))
	}

	// Test removing resources
	rt.RemoveDeployment("test-deployment-1")
	rt.RemoveService("test-service-1")

	deployments, services = rt.GetTrackedResources()

	if len(deployments) != 1 || deployments[0] != "test-deployment-2" {
		t.Errorf("Expected 1 deployment 'test-deployment-2', got %v", deployments)
	}
	if len(services) != 1 || services[0] != "test-service-2" {
		t.Errorf("Expected 1 service 'test-service-2', got %v", services)
	}
}

func TestResourceTracker_Cleanup(t *testing.T) {
	// Create a fake clientset
	namespace := "test-namespace"
	fakeClient := testclient.NewSimpleClientset()

	// Initialize the clients
	deploymentsClient = fakeClient.AppsV1().Deployments(namespace)
	svcClient = fakeClient.CoreV1().Services(namespace)

	clients := &Clients{
		Deployments: deploymentsClient,
		Services:    svcClient,
	}

	// Create a resource tracker
	rt := NewResourceTracker(namespace, clients)
	rt.SetTimeout(5 * time.Second)

	// Create test resources in the fake client
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: namespace,
		},
	}
	_, err := svcClient.Create(context.Background(), svc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test service: %v", err)
	}

	// Add resources to tracker
	rt.AddService("test-service")

	// Test cleanup
	err = rt.Cleanup(context.Background())
	if err != nil {
		t.Errorf("Cleanup failed: %v", err)
	}

	// Verify resources were deleted
	services, err := svcClient.List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list services: %v", err)
	}

	if len(services.Items) != 0 {
		t.Errorf("Expected 0 services after cleanup, got %d", len(services.Items))
	}
}

func TestResourceTracker_CleanupTimeout(t *testing.T) {
	namespace := "test-namespace"
	fakeClient := testclient.NewSimpleClientset()
	// Initialize the clients
	deploymentsClient = fakeClient.AppsV1().Deployments(namespace)
	svcClient = fakeClient.CoreV1().Services(namespace)

	clients := &Clients{
		Deployments: deploymentsClient,
		Services:    svcClient,
	}
	rt := NewResourceTracker("test-namespace", clients)
	rt.SetTimeout(1 * time.Millisecond) // Very short timeout

	// Add some resources
	rt.AddDeployment("test-deployment")
	rt.AddService("test-service")

	// Create a context with an already expired timeout
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	// Cleanup should fail due to timeout
	err := rt.Cleanup(ctx)
	if err == nil {
		t.Error("Expected timeout error, got nil")
	}
}
