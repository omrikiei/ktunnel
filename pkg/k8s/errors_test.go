package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	testclient "k8s.io/client-go/kubernetes/fake"
)

var deploymentResource = schema.GroupResource{Group: "apps", Resource: "deployments"}

// TestAPIError covers the four failures every call to the API server can
// return, and what each one is actually about.
//
// The raw client-go error names the object in its own vocabulary and stops
// there: `deployments.apps "api" not found` does not say which namespace was
// looked in, and that is the answer most of the time -- ktunnel used the
// wrong one, or the user meant another context (#134).
func TestAPIError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "not found says where it looked and how to change it",
			err:  apierrors.NewNotFound(deploymentResource, "api"),
			want: []string{"deployment", "team-a/api", "--namespace", "--context"},
		},
		{
			name: "forbidden points at permissions, not at the object",
			err:  apierrors.NewForbidden(deploymentResource, "api", errors.New("nope")),
			want: []string{"team-a/api", "not allowed", "docs/security.md"},
		},
		{
			name: "unauthorized points at the credentials",
			err:  apierrors.NewUnauthorized("token expired"),
			want: []string{"team-a/api", "credentials"},
		},
		{
			name: "a conflict says the object changed underneath",
			err:  apierrors.NewConflict(deploymentResource, "api", errors.New("object modified")),
			want: []string{"team-a/api", "changed"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := apiError("read", "deployment", "team-a", "api", tc.err)
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%q does not mention %q", err, want)
				}
			}
			if !errors.Is(err, tc.err) {
				t.Error("the underlying API error is not wrapped, so nothing can inspect it and the detail is lost")
			}
		})
	}
}

// TestAPIError_UnknownCauseIsStillNamed: an error class with no advice
// attached still names the object and the verb, rather than passing the
// client-go string through on its own.
func TestAPIError_UnknownCauseIsStillNamed(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.1:443: i/o timeout")

	err := apiError("read", "deployment", "team-a", "api", cause)

	for _, want := range []string{"read", "deployment", "team-a/api", "i/o timeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not mention %q", err, want)
		}
	}
}

// Test_InjectSidecar_MissingDeploymentNamesTheFix is the first error most
// people meet on this command: a name that is right in the namespace they had
// in mind and absent from the one ktunnel used.
func Test_InjectSidecar_MissingDeploymentNamesTheFix(t *testing.T) {
	namespace, name := "team-a", "api"
	fake := testclient.NewSimpleClientset()
	useFakeClients(fake, namespace)
	svc := &KubeService{clients: &Clients{
		Deployments: fake.AppsV1().Deployments(namespace),
		Pods:        fake.CoreV1().Pods(namespace),
	}}

	port := 28688
	readyChan := make(chan bool, 1)
	_, err := svc.InjectSidecar(&namespace, &name, KindDeployment, &port, "test-image:latest", PodCredentials{}, readyChan, nil)
	if err == nil {
		t.Fatal("injecting into a deployment that does not exist reported success")
	}
	for _, want := range []string{"team-a/api", "--namespace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not mention %q, so it does not say where ktunnel looked or how to look elsewhere", err, want)
		}
	}
}

// Test_RemoveSidecar_NothingToEject: ejecting from a deployment that does not
// carry the sidecar is a no-op, not a failure.
//
// It used to come back as `test-image:latest is not present on spec`, logged
// as `Failed removing tunnel sidecar` -- an error message, naming an image
// rather than the deployment, for the case where the cluster is already in the
// state the user asked for. That happens on every second Ctrl+C of a run whose
// rollout never finished, and after someone has removed the container by hand.
func Test_RemoveSidecar_NothingToEject(t *testing.T) {
	namespace, name := "team-a", "api"
	fake := testclient.NewSimpleClientset()
	useFakeClients(fake, namespace)
	svc := &KubeService{clients: &Clients{
		Deployments: fake.AppsV1().Deployments(namespace),
		Pods:        fake.CoreV1().Pods(namespace),
	}}

	containers := podContainers("app", "app:latest")
	if err := createDeployment(svc.clients.Deployments, name, 1, &containers); err != nil {
		t.Fatalf("failed creating the deployment under test: %v", err)
	}

	readyChan := make(chan bool, 1)
	ok, err := svc.RemoveSidecar(&namespace, &name, KindDeployment, "test-image:latest", readyChan, nil)
	if err != nil {
		t.Errorf("ejecting a sidecar that is not there failed with %v; the deployment is already in the state that was asked for", err)
	}
	if !ok {
		t.Error("ejecting a sidecar that is not there reported failure")
	}
	// The caller blocks on this channel before exiting, so a path that skips
	// the rollout still has to report on it or the command hangs on Ctrl+C.
	select {
	case <-readyChan:
	default:
		t.Error("nothing was sent on readyChan, so `inject` would hang waiting for a rollout that is not coming")
	}
}

// TestGetPodNames_TooFewPodsSaysWhereToLook: the message a user sees while a
// rollout is still coming up, or has failed, is the one that has to say which
// deployment in which namespace, and what to go and look at.
func TestGetPodNames_TooFewPodsSaysWhereToLook(t *testing.T) {
	selector := map[string]string{"app": "web"}
	svc := exposeFixture(t,
		appDeployment("web", 3, selector),
		appPod("web-1", selector, v1.PodRunning, time.Minute),
	)

	pods := make([]string, 3)
	err := svc.getPodNames(context.Background(), newDeploymentWorkload(appDeployment("web", 3, selector)), pods)
	if err == nil {
		t.Fatal("resolving three pods from one running pod reported success")
	}
	for _, want := range []string{"default/web", "app=web", "kubectl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not mention %q, so it does not say which pods ktunnel was looking for or how to see them", err, want)
		}
	}
}

// TestForwardError_BindFailureNamesTheLocalPort: "unable to listen on any of
// the requested ports" is the most common failure on the forwarding path and
// the one that has nothing to do with the cluster. The port is local, the
// process holding it is local, and the fix is a local flag.
func TestForwardError_BindFailureNamesTheLocalPort(t *testing.T) {
	cause := errors.New("unable to listen on any of the requested ports: [{28688 28688}]")

	err := forwardError("team-a", "api-7d8f", "28688", cause)

	for _, want := range []string{"28688", "team-a/api-7d8f", "--port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not mention %q", err, want)
		}
	}
}

// TestForwardError_OtherFailuresStillNameBothEnds: anything else keeps the
// cause and still says which local port and which pod it was between.
func TestForwardError_OtherFailuresStillNameBothEnds(t *testing.T) {
	cause := errors.New("lost connection to pod")

	err := forwardError("team-a", "api-7d8f", "28688", cause)

	for _, want := range []string{"28688", "team-a/api-7d8f", "lost connection to pod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "--port") {
		t.Error("a failure that is not a bind failure points at --port, which will not fix it")
	}
}
