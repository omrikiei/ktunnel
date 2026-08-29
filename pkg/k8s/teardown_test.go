package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "k8s.io/client-go/kubernetes/fake"
)

// kubeServiceWithExposed returns a KubeService backed by a fake API server
// already holding the deployment and service that `expose` would have created.
func kubeServiceWithExposed(name string) *KubeService {
	fake := testclient.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		},
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		},
	)
	return &KubeService{clients: &Clients{
		Deployments: fake.AppsV1().Deployments("default"),
		Services:    fake.CoreV1().Services("default"),
	}}
}

// TestTeardownExposedService_IsIdempotent is the regression test for a red
// ERROR line printed on every clean Ctrl+C of `ktunnel expose`.
//
// Two independent signal handlers used to race to delete the same deployment
// and service: the one ResourceTracker installed, and the one the tunnel
// session installs. Whichever lost reported `Failed deleting k8s objects:
// services "..." not found` -- an error for work that had in fact succeeded,
// which is precisely the "vague errors" complaint in #134.
//
// The duplicate handler is gone, and teardown is idempotent besides, so that
// anything else removing the resources first -- a `kubectl delete` from
// another terminal -- is not reported as a failure either.
func TestTeardownExposedService_IsIdempotent(t *testing.T) {
	svc := kubeServiceWithExposed("ktunnel-test")

	if err := svc.TeardownExposedService("ktunnel-test", false); err != nil {
		t.Fatalf("first teardown: unexpected error: %v", err)
	}

	// Everything is gone now, so a second teardown finds nothing. That has to
	// be a success, not an error.
	if err := svc.TeardownExposedService("ktunnel-test", false); err != nil {
		t.Fatalf("second teardown reported failure for resources already gone: %v", err)
	}

	// And the resources really are gone -- an idempotent teardown must not be
	// idempotent by way of never deleting anything.
	if _, err := svc.clients.Deployments.Get(context.Background(), "ktunnel-test", metav1.GetOptions{}); err == nil {
		t.Error("deployment still exists after teardown")
	}
	if _, err := svc.clients.Services.Get(context.Background(), "ktunnel-test", metav1.GetOptions{}); err == nil {
		t.Error("service still exists after teardown")
	}
}

// TestTeardownExposedService_DeploymentOnlyLeavesService covers the
// --deployment-only path: the service belongs to the user, not to ktunnel, so
// teardown must not touch it.
func TestTeardownExposedService_DeploymentOnlyLeavesService(t *testing.T) {
	svc := kubeServiceWithExposed("ktunnel-test")

	if err := svc.TeardownExposedService("ktunnel-test", true); err != nil {
		t.Fatalf("teardown: unexpected error: %v", err)
	}

	if _, err := svc.clients.Deployments.Get(context.Background(), "ktunnel-test", metav1.GetOptions{}); err == nil {
		t.Error("deployment still exists after teardown")
	}
	if _, err := svc.clients.Services.Get(context.Background(), "ktunnel-test", metav1.GetOptions{}); err != nil {
		t.Errorf("DeploymentOnly teardown deleted a service it does not own: %v", err)
	}
}
