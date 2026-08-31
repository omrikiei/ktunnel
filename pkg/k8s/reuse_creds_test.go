package k8s

import (
	"context"
	"testing"

	"github.com/omrikiei/ktunnel/pkg/creds"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "k8s.io/client-go/kubernetes/fake"
)

// --reuse adopts a deployment exactly as it stands: it does not mount
// ktunnel's Secret and its server never reads ktunnel's token. Provisioning
// credentials for it creates a Secret nothing consumes -- demanding a
// permission the #120 and #94 users may not have -- and leaves the client
// expecting a TLS handshake the adopted server cannot complete.
//
// So an adopted deployment gets no credentials at all, and the caller is told
// so, rather than finding out through a failed connection attempt.
func TestExposeAsService_ReuseAdoptsWithoutProvisioningCredentials(t *testing.T) {
	fake := testclient.NewSimpleClientset(handWrittenDeployment("myapp"), handWrittenService("myapp"))
	svc := fakeKubeServiceWithSecrets(fake)
	bundle, err := creds.Generate("myapp", "default")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	readyChan := make(chan bool, 1)
	tracker, podCreds, err := svc.ExposeAsService(
		"default", "myapp", 28688, "tcp", []string{"8080"}, "",
		Image, true, false, readyChan,
		map[string]string{}, map[string]string{}, map[string]string{}, nil,
		bundle, "ClusterIP",
		100, 500, 100, 1000,
	)
	if err != nil {
		t.Fatalf("ExposeAsService: %v", err)
	}

	secrets, err := svc.clients.Secrets.List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Errorf("--reuse created %d secrets for a deployment that cannot mount them", len(secrets.Items))
	}
	if len(tracker.secrets) != 0 {
		t.Errorf("--reuse tracked a secret: %v", tracker.secrets)
	}

	if podCreds != (PodCredentials{}) {
		t.Errorf("ExposeAsService reported %+v for an adopted deployment; "+
			"the client will attempt TLS against a server that serves plaintext", podCreds)
	}

	// And the adopted deployment is still untouched, which is the v2.3
	// contract this must not break.
	deployment, err := svc.clients.Deployments.Get(context.Background(), "myapp", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting the deployment: %v", err)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != privateRegistryImage {
		t.Errorf("the adopted deployment's image is now %q, want %q", got, privateRegistryImage)
	}
	if len(deployment.Spec.Template.Spec.Volumes) != 0 {
		t.Errorf("a volume was patched into the adopted deployment: %+v", deployment.Spec.Template.Spec.Volumes)
	}
}

// --reuse with nothing there creates the deployment from ktunnel's own
// template, so that run is secured like any other. Skipping credentials for
// every --reuse run would silently downgrade this one.
func TestExposeAsService_ReuseCreatingFromScratchIsStillSecured(t *testing.T) {
	fake := testclient.NewSimpleClientset()
	svc := fakeKubeServiceWithSecrets(fake)
	bundle, err := creds.Generate("myapp", "default")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	readyChan := make(chan bool, 1)
	tracker, podCreds, err := svc.ExposeAsService(
		"default", "myapp", 28688, "tcp", []string{"8080"}, "",
		Image, true, false, readyChan,
		map[string]string{}, map[string]string{}, map[string]string{}, nil,
		bundle, "ClusterIP",
		100, 500, 100, 1000,
	)
	if err != nil {
		t.Fatalf("ExposeAsService: %v", err)
	}

	if podCreds.SecretName != "myapp" {
		t.Errorf("a deployment ktunnel created itself was not secured: %+v", podCreds)
	}
	if len(tracker.secrets) != 1 {
		t.Errorf("the secret is not tracked for cleanup: %v", tracker.secrets)
	}
}
