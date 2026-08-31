package k8s

import (
	"context"
	"testing"

	"github.com/omrikiei/ktunnel/pkg/creds"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	testclient "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// exposeWithBundle is `ktunnel expose NAME 8080` for a run that generated
// credentials, which after v2.4 is every run that did not pass --insecure.
func exposeWithBundle(t *testing.T, svc *KubeService, name string, bundle *creds.Bundle) (*ResourceTracker, error) {
	t.Helper()
	readyChan := make(chan bool, 1)
	return svc.ExposeAsService(
		"default", name, 28688, "tcp", []string{"8080"}, "",
		Image, false, false, readyChan,
		map[string]string{}, map[string]string{}, map[string]string{}, nil,
		bundle, "ClusterIP",
		100, 500, 100, 1000,
	)
}

func TestExposeAsService_CreatesAndTracksTheCredentialsSecret(t *testing.T) {
	fake := testclient.NewSimpleClientset()
	svc := fakeKubeServiceWithSecrets(fake)
	bundle, err := creds.Generate("myapp", "default")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	tracker, err := exposeWithBundle(t, svc, "myapp", bundle)
	if err != nil {
		t.Fatalf("ExposeAsService: %v", err)
	}

	secret, err := svc.clients.Secrets.Get(context.Background(), "myapp", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no credentials secret was created: %v", err)
	}
	if string(secret.Data["token"]) != bundle.Token {
		t.Error("the secret does not hold this run's token")
	}

	if len(tracker.secrets) != 1 || tracker.secrets[0] != "myapp" {
		t.Errorf("the secret is not tracked for cleanup: %v; a Ctrl+C would leave a private key in the cluster", tracker.secrets)
	}

	// And the deployment has to actually use it, or the secret is decoration.
	deployment, err := svc.clients.Deployments.Get(context.Background(), "myapp", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting the deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if len(container.VolumeMounts) == 0 {
		t.Error("the deployment mounts no credentials")
	}
	if !argsContain(container.Args, "--tls") {
		t.Errorf("the deployment does not enable TLS despite a mounted certificate: %q", container.Args)
	}
}

// The namespace forbids `secrets: create`. The run must not die -- ktunnel's
// pitch is that it needs no special permissions -- but it must not pretend
// either: it keeps the token and gives up encryption.
func TestExposeAsService_FallsBackToAnInlineTokenWhenSecretsAreForbidden(t *testing.T) {
	fake := testclient.NewSimpleClientset()
	fake.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewForbidden(
			schema.GroupResource{Resource: "secrets"}, "myapp", nil)
	})
	svc := fakeKubeServiceWithSecrets(fake)
	bundle, err := creds.Generate("myapp", "default")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	tracker, err := exposeWithBundle(t, svc, "myapp", bundle)
	if err != nil {
		t.Fatalf("a forbidden secret ended the run: %v; ktunnel is meant to work without special permissions", err)
	}
	if len(tracker.secrets) != 0 {
		t.Errorf("tracking a secret that was never created: %v", tracker.secrets)
	}

	deployment, err := svc.clients.Deployments.Get(context.Background(), "myapp", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting the deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]

	if argsContain(container.Args, "--tls") {
		t.Errorf("the fallback claims TLS with no certificate to serve: %q", container.Args)
	}
	if len(container.VolumeMounts) != 0 {
		t.Errorf("the fallback mounts a secret that does not exist: %+v", container.VolumeMounts)
	}

	var token *apiv1.EnvVar
	for i := range container.Env {
		if container.Env[i].Name == creds.TokenEnvVar {
			token = &container.Env[i]
		}
	}
	if token == nil || token.Value != bundle.Token {
		t.Fatalf("the fallback dropped authentication as well as encryption: %+v", container.Env)
	}
}

// --insecure: no bundle, and therefore nothing new in the cluster at all.
func TestExposeAsService_InsecureCreatesNoSecret(t *testing.T) {
	fake := testclient.NewSimpleClientset()
	svc := fakeKubeServiceWithSecrets(fake)

	tracker, err := exposeWithBundle(t, svc, "myapp", nil)
	if err != nil {
		t.Fatalf("ExposeAsService: %v", err)
	}
	if len(tracker.secrets) != 0 {
		t.Errorf("--insecure tracked a secret: %v", tracker.secrets)
	}

	secrets, err := svc.clients.Secrets.List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing secrets: %v", err)
	}
	if len(secrets.Items) != 0 {
		t.Errorf("--insecure created %d secrets", len(secrets.Items))
	}
}

func fakeKubeServiceWithSecrets(fake *testclient.Clientset) *KubeService {
	clientMutex.Lock()
	deploymentsClient = fake.AppsV1().Deployments("default")
	podsClient = fake.CoreV1().Pods("default")
	svcClient = fake.CoreV1().Services("default")
	clientMutex.Unlock()

	return &KubeService{clients: &Clients{
		Deployments: fake.AppsV1().Deployments("default"),
		Pods:        fake.CoreV1().Pods("default"),
		Services:    fake.CoreV1().Services("default"),
		Secrets:     fake.CoreV1().Secrets("default"),
	}}
}
