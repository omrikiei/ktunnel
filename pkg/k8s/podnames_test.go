package k8s

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	testclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func labelledPod(deployment, name string, phase v1.PodPhase) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				deploymentNameLabel:     deployment,
				deploymentInstanceLabel: deployment,
			},
		},
		Status: v1.PodStatus{Phase: phase},
	}
}

// kubeServiceWithPods returns a KubeService backed by a fake API server
// holding pods.
func kubeServiceWithPods(pods ...*v1.Pod) *KubeService {
	objects := make([]runtime.Object, 0, len(pods))
	for _, p := range pods {
		objects = append(objects, p)
	}
	fake := testclient.NewSimpleClientset(objects...)
	return &KubeService{clients: &Clients{Pods: fake.CoreV1().Pods("default")}}
}

// TestGetPodNames_FewerRunningPodsThanReplicas is the regression test for a
// crash on the exact path this feature exists to survive.
//
// Between a tunnel server pod being deleted and its replacement reaching
// Running, the deployment still says it wants one replica and there is no
// running pod to satisfy it. Indexing past the end of the matches used to
// panic, taking the whole process down -- so `kubectl delete pod` killed
// ktunnel instead of being reconnected through.
func TestGetPodNames_FewerRunningPodsThanReplicas(t *testing.T) {
	svc := kubeServiceWithPods(labelledPod("proxy", "proxy-7d9f-x2m1", v1.PodPending))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("resolving pods panicked (%v) when the replacement pod was not running yet; "+
				"deleting the tunnel server pod kills ktunnel outright instead of being reconnected through", r)
		}
	}()

	pods := make([]string, 1)
	err := svc.getPodNames(context.Background(), "proxy", pods)
	if err == nil {
		t.Fatal("resolving pods reported success with no running pod to forward to; the attempt would build a forward to the empty pod name")
	}
	if !strings.Contains(err.Error(), "proxy") {
		t.Errorf("the error %q does not name the deployment, so a user watching the reconnect log cannot tell what is missing", err)
	}
}

// TestGetPodNames_ResolvesRunningPods keeps the ordinary case: pod names are
// re-resolved on every attempt, and a forward is bound to the name it gets.
func TestGetPodNames_ResolvesRunningPods(t *testing.T) {
	svc := kubeServiceWithPods(
		labelledPod("proxy", "proxy-7d9f-x2m1", v1.PodRunning),
		labelledPod("proxy", "proxy-7d9f-b4k2", v1.PodPending),
	)

	pods := make([]string, 1)
	if err := svc.getPodNames(context.Background(), "proxy", pods); err != nil {
		t.Fatalf("failed resolving a running pod: %v", err)
	}
	if pods[0] != "proxy-7d9f-x2m1" {
		t.Errorf("resolved %q, want the running pod proxy-7d9f-x2m1", pods[0])
	}
}

// zeroReplicaDeployment is a tunnel server deployment that has been scaled to
// zero -- `expose -r` against a scaled-down deployment, or someone scaling it
// down mid-session, which is a reconnect scenario by definition.
func zeroReplicaDeployment(name string) *appsv1.Deployment {
	replicas := int32(0)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

// TestPortForward_NoPodsNeverReportsSuccessWithNoPorts is the regression test
// for a nil dereference in the caller.
//
// With no pods to forward to, every forwarder has already returned before the
// final select is reached, so both of its cases are ready at once and Go picks
// between them uniformly. The forwarder-error case receives the zero value
// from the closed channel and returned it as (nil ports, nil error): a success
// with nothing in it. Callers check the returned slice for emptiness, which
// dereferences the nil pointer and panics -- inside a retry loop, on roughly
// half of the attempts, which reads as intermittent and is miserable to
// diagnose.
func TestPortForward_NoPodsNeverReportsSuccessWithNoPorts(t *testing.T) {
	name := "scaled-to-zero"
	fake := testclient.NewSimpleClientset(zeroReplicaDeployment(name))

	clientMutex.Lock()
	deploymentsClient = fake.AppsV1().Deployments("default")
	clientMutex.Unlock()

	svc := &KubeService{clients: &Clients{Pods: fake.CoreV1().Pods("default")}}

	// Repeated because the bug is a uniform choice between two ready cases:
	// a single run passes half the time whether or not it is fixed.
	for i := range 200 {
		stopChan := make(chan struct{})
		sourcePorts, _, err := svc.PortForward(context.Background(), "default", name, "28688", stopChan)
		close(stopChan)

		if err != nil {
			// Reporting "no pods" as an error would be fine too; what must
			// never happen is success with nothing to hand back.
			continue
		}
		if sourcePorts == nil {
			t.Fatalf("run %d: PortForward reported success with a nil port list; "+
				"the caller checks that list for emptiness, which dereferences the nil and panics the reconnect loop", i)
		}
		if len(*sourcePorts) != 0 {
			t.Fatalf("run %d: PortForward returned %d ports for a deployment with no pods", i, len(*sourcePorts))
		}
	}
}

// oneReplicaDeployment is a tunnel server deployment that wants a single pod.
func oneReplicaDeployment(name string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
}

// TestPortForward_UnresponsiveAPIServerIsCancellable is the regression test for
// a hang that no timeout covered.
//
// A forward reports itself ready only after its SPDY dial completes, and that
// dial is not cancellable by any means client-go exposes. So against an API
// server that accepts connections and then says nothing, neither of the other
// two cases of the startup select can ever arrive: no forwarder has failed,
// and none has become ready. PortForward parked there forever -- and it parks
// inside the caller, before the caller has registered the release timeout
// built for exactly this scenario. Supervisor.Run waits for the attempt, so
// the whole command hung with only a second Ctrl+C to escape: the precise
// failure this feature exists to remove.
func TestPortForward_UnresponsiveAPIServerIsCancellable(t *testing.T) {
	// Accepts, then never answers. The TCP connect completes from the
	// backlog, so the dial gets far enough to block rather than to fail.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed starting the stand-in API server: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()

	name := "unresponsive"
	fake := testclient.NewSimpleClientset(
		oneReplicaDeployment(name),
		labelledPod(name, name+"-7d9f-x2m1", v1.PodRunning),
	)

	clientMutex.Lock()
	deploymentsClient = fake.AppsV1().Deployments("default")
	clientMutex.Unlock()

	svc := &KubeService{
		clients: &Clients{Pods: fake.CoreV1().Pods("default")},
		config:  &rest.Config{Host: "http://" + ln.Addr().String()},
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopChan := make(chan struct{})
	t.Cleanup(func() { close(stopChan) })

	done := make(chan error, 1)
	go func() {
		_, _, err := svc.PortForward(ctx, "default", name, "28688", stopChan)
		done <- err
	}()

	// Long enough for the forwarder to be inside its dial rather than still
	// being set up, so the cancellation is landing on the case that hangs.
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("PortForward returned %v on its own against an API server that never answers; "+
			"this test is asserting nothing about cancellation", err)
	default:
	}

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("PortForward reported success after its context was cancelled; the caller would go on to build a tunnel over a forward that never came up")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("PortForward never returned after its context was cancelled; a forwarder stuck dialling an unresponsive API server parks the attempt " +
			"before its release timeout is even registered, and the command hangs with only a second Ctrl+C to escape")
	}
}
