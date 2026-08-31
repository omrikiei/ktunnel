package k8s

import (
	"context"
	"testing"

	"github.com/omrikiei/ktunnel/pkg/creds"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	testclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A credentials Secret is one more thing a run leaves in the cluster, so it
// has to leave with the rest. Ctrl+C removing the Deployment and the Service
// but leaving a Secret full of a private key behind is exactly the kind of
// resource surprise v2.3 spent a release removing.
func TestResourceTracker_CleanupDeletesTrackedSecrets(t *testing.T) {
	const namespace = "test-namespace"
	fakeClient := testclient.NewSimpleClientset()
	clients := &Clients{
		Deployments: fakeClient.AppsV1().Deployments(namespace),
		Services:    fakeClient.CoreV1().Services(namespace),
		Secrets:     fakeClient.CoreV1().Secrets(namespace),
	}

	if _, err := clients.Secrets.Create(context.Background(),
		newSecret(namespace, "myapp", testBundle(t)), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the secret: %v", err)
	}

	rt := NewResourceTracker(namespace, clients)
	rt.AddSecret("myapp")

	if err := rt.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, err := clients.Secrets.Get(context.Background(), "myapp", metav1.GetOptions{}); err == nil {
		t.Error("the credentials secret survived cleanup")
	}
}

// The Secret goes last. Deleting it while the pod that mounts it is still
// running turns a clean Ctrl+C into a crash-looping pod on the way out.
func TestResourceTracker_CleanupDeletesTheSecretAfterTheDeployment(t *testing.T) {
	const namespace = "test-namespace"
	fakeClient := testclient.NewSimpleClientset()

	var order []string
	fakeClient.PrependReactor("delete", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, action.GetResource().Resource)
		return false, nil, nil
	})

	clients := &Clients{
		Deployments: fakeClient.AppsV1().Deployments(namespace),
		Services:    fakeClient.CoreV1().Services(namespace),
		Secrets:     fakeClient.CoreV1().Secrets(namespace),
	}
	if _, err := clients.Deployments.Create(context.Background(),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "myapp", Namespace: namespace}},
		metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the deployment: %v", err)
	}
	if _, err := clients.Secrets.Create(context.Background(),
		newSecret(namespace, "myapp", testBundle(t)), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seeding the secret: %v", err)
	}

	rt := NewResourceTracker(namespace, clients)
	rt.AddDeployment("myapp")
	rt.AddSecret("myapp")

	if err := rt.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	deploymentAt, secretAt := -1, -1
	for i, r := range order {
		if r == "deployments" {
			deploymentAt = i
		}
		if r == "secrets" {
			secretAt = i
		}
	}
	if deploymentAt == -1 || secretAt == -1 {
		t.Fatalf("cleanup did not delete both objects, saw %v", order)
	}
	if secretAt < deploymentAt {
		t.Errorf("the secret was deleted before the deployment that mounts it: %v", order)
	}
}

func testBundle(t *testing.T) *creds.Bundle {
	t.Helper()
	b, err := creds.Generate("myapp", "test-namespace")
	if err != nil {
		t.Fatalf("generating a test bundle: %v", err)
	}
	return b
}
