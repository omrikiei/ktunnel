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

// ktunnelSelector is the selector on the Deployments `expose` creates: the two
// labels ktunnel sets on them itself.
func ktunnelSelector(name string) *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchLabels: map[string]string{
			deploymentNameLabel:     name,
			deploymentInstanceLabel: name,
		},
	}
}

// ktunnelDeployment is a tunnel server Deployment as `expose` creates it.
func ktunnelDeployment(name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: ktunnelSelector(name),
		},
	}
}

// appDeployment is an ordinary application Deployment, the kind `inject`
// targets: it selects its pods by whatever labels its author chose, and knows
// nothing of ktunnel's.
func appDeployment(name string, replicas int32, selector map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
		},
	}
}

// appPod is a pod of an appDeployment: it carries that deployment's own
// selector labels, and none of ktunnel's.
func appPod(name string, labels map[string]string, phase v1.PodPhase, age time.Duration) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			Labels:            labels,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Status: v1.PodStatus{Phase: phase},
	}
}

// terminating marks a pod as being deleted, the way the API server does once
// something has asked for it to go away but its grace period has not expired.
func terminating(p *v1.Pod) *v1.Pod {
	deleted := metav1.NewTime(time.Now())
	p.DeletionTimestamp = &deleted
	p.Finalizers = []string{"ktunnel.test/hold"}
	return p
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
	err := svc.getPodNames(context.Background(), newDeploymentWorkload(ktunnelDeployment("proxy", 1)), pods)
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
	if err := svc.getPodNames(context.Background(), newDeploymentWorkload(ktunnelDeployment("proxy", 1)), pods); err != nil {
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
	return ktunnelDeployment(name, 0)
}

// TestPortForward_NoPodsNeverReportsSuccessWithNoPorts pins what PortForward
// hands back for a deployment nobody has any pods for.
//
// With no pods to forward to, every forwarder has already returned before the
// final select is reached, so both of its cases are ready at once and Go picks
// between them uniformly. The forwarder-error case receives the zero value
// from the closed channel and reports it as a nil error: a success with
// nothing in it, on roughly half the runs, which is why this repeats.
//
// It used to return the ports as a *[]string and hand back a nil one here.
// Callers check the list for emptiness, which dereferenced the nil and
// panicked -- inside a retry loop, intermittently, on a deployment someone had
// merely scaled down. The return is a plain slice now, so "none" and "not
// there" are the same value and that panic cannot be written. What still has
// to hold is the part this test asserts: never a port that is not being
// forwarded.
func TestPortForward_NoPodsNeverReportsSuccessWithNoPorts(t *testing.T) {
	name := "scaled-to-zero"
	fake := testclient.NewSimpleClientset(zeroReplicaDeployment(name))

	clientMutex.Lock()
	deploymentsClient = fake.AppsV1().Deployments("default")
	clientMutex.Unlock()

	svc := &KubeService{clients: &Clients{
		Deployments: fake.AppsV1().Deployments("default"),
		Pods:        fake.CoreV1().Pods("default"),
	}}

	// Repeated because the bug is a uniform choice between two ready cases:
	// a single run passes half the time whether or not it is fixed.
	for i := range 200 {
		stopChan := make(chan struct{})
		sourcePorts, _, err := svc.PortForward(context.Background(), KindDeployment, "default", name, "28688", stopChan)
		close(stopChan)

		if err != nil {
			// Reporting "no pods" as an error would be fine too; what must
			// never happen is success with ports that do not exist.
			continue
		}
		if len(sourcePorts) != 0 {
			t.Fatalf("run %d: PortForward returned %d ports for a deployment with no pods; "+
				"the caller would start a tunnel client over a local port nothing is forwarding", i, len(sourcePorts))
		}
	}
}

// oneReplicaDeployment is a tunnel server deployment that wants a single pod.
func oneReplicaDeployment(name string) *appsv1.Deployment {
	return ktunnelDeployment(name, 1)
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
		clients: &Clients{
			Deployments: fake.AppsV1().Deployments("default"),
			Pods:        fake.CoreV1().Pods("default"),
		},
		config: &rest.Config{Host: "http://" + ln.Addr().String()},
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopChan := make(chan struct{})
	t.Cleanup(func() { close(stopChan) })

	done := make(chan error, 1)
	go func() {
		_, _, err := svc.PortForward(ctx, KindDeployment, "default", name, "28688", stopChan)
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

// TestGetPodNames_UsesTheDeploymentsOwnSelector is the regression test for
// `inject` never having worked against a Deployment ktunnel did not create.
//
// Pods used to be resolved by the two labels `expose` puts on its own
// Deployments. An application Deployment is labelled however its author chose,
// so nothing matched: the sidecar injected, the pod reported 2/2 Running, and
// the port-forward retried "found 0 running pod(s)" forever. Half the product,
// broken for years (#171, #115).
func TestGetPodNames_UsesTheDeploymentsOwnSelector(t *testing.T) {
	selector := map[string]string{"app": "web", "tier": "frontend"}
	svc := kubeServiceWithPods(appPod("web-5b65-dd54r", selector, v1.PodRunning, time.Minute))

	pods := make([]string, 1)
	if err := svc.getPodNames(context.Background(), newDeploymentWorkload(appDeployment("web", 1, selector)), pods); err != nil {
		t.Fatalf("failed resolving the pods of a deployment ktunnel did not create: %v; "+
			"this is `ktunnel inject` against any ordinary deployment, and it never forwards to anything", err)
	}
	if pods[0] != "web-5b65-dd54r" {
		t.Errorf("resolved %q, want the deployment's own pod web-5b65-dd54r", pods[0])
	}
}

// TestGetPodNames_RolloutWindowPrefersTheNewPod covers the window a rollout
// opens: a Deployment's selector matches the pods of its old ReplicaSet and
// its new one alike, so for a moment two running pods answer to it.
//
// Injecting the sidecar *is* a rollout, so this window is on the path of every
// `inject` run rather than an edge case. Only the new pod carries the sidecar;
// forwarding to the outgoing one reaches a pod with no tunnel server in it and
// no port to forward to.
func TestGetPodNames_RolloutWindowPrefersTheNewPod(t *testing.T) {
	selector := map[string]string{"app": "web"}
	svc := kubeServiceWithPods(
		appPod("web-old-4k2b", selector, v1.PodRunning, 10*time.Minute),
		appPod("web-new-x2m1", selector, v1.PodRunning, 5*time.Second),
	)

	pods := make([]string, 1)
	if err := svc.getPodNames(context.Background(), newDeploymentWorkload(appDeployment("web", 1, selector)), pods); err != nil {
		t.Fatalf("failed resolving pods mid-rollout: %v", err)
	}
	if pods[0] != "web-new-x2m1" {
		t.Errorf("resolved %q mid-rollout, want the new pod web-new-x2m1: the old one has no sidecar to forward to", pods[0])
	}
}

// TestGetPodNames_SkipsTerminatingPods keeps the other half of the rollout
// window honest. A pod being deleted stays Phase Running for its whole grace
// period, and it is the newest match for as long as its replacement has not
// been created -- so newest-first alone would pick the one pod that is
// guaranteed to be gone shortly, and the forward would die with it.
func TestGetPodNames_SkipsTerminatingPods(t *testing.T) {
	selector := map[string]string{"app": "web"}
	svc := kubeServiceWithPods(
		appPod("web-staying-4k2b", selector, v1.PodRunning, 10*time.Minute),
		terminating(appPod("web-going-x2m1", selector, v1.PodRunning, 5*time.Second)),
	)

	pods := make([]string, 1)
	if err := svc.getPodNames(context.Background(), newDeploymentWorkload(appDeployment("web", 1, selector)), pods); err != nil {
		t.Fatalf("failed resolving pods while one was terminating: %v", err)
	}
	if pods[0] != "web-staying-4k2b" {
		t.Errorf("resolved %q, want web-staying-4k2b: web-going-x2m1 is being deleted and its forward dies with it", pods[0])
	}
}

// TestGetPodNames_RefusesASelectorThatMatchesEverything pins the one case
// where reading the Deployment's selector could be worse than reading
// ktunnel's labels. An absent or empty selector converts to "match every pod",
// which would forward to an arbitrary unrelated pod in the namespace and
// report success. Refusing names the object; matching anything does not.
func TestGetPodNames_RefusesASelectorThatMatchesEverything(t *testing.T) {
	unrelated := map[string]string{"app": "postgres"}
	svc := kubeServiceWithPods(appPod("postgres-0", unrelated, v1.PodRunning, time.Hour))

	replicas := int32(1)
	for _, tc := range []struct {
		name       string
		deployment *appsv1.Deployment
	}{
		{
			name: "no selector",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
				Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			},
		},
		{
			name:       "empty selector",
			deployment: appDeployment("web", 1, map[string]string{}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := make([]string, 1)
			err := svc.getPodNames(context.Background(), newDeploymentWorkload(tc.deployment), pods)
			if err == nil {
				t.Fatalf("resolved %q for a deployment that selects nothing in particular; "+
					"the tunnel would be built to an unrelated pod that happened to be in the namespace", pods[0])
			}
			if !strings.Contains(err.Error(), "web") {
				t.Errorf("the error %q does not name the deployment, so the user cannot tell which object to fix", err)
			}
		})
	}
}

// TestGetPodNames_ResolvesEveryReplica covers the fan-out `inject` relies on
// for a deployment with more than one replica.
//
// The sidecar's listeners are pod-local, so a replica without a forward of its
// own is a replica whose containers get connection refused on the tunnelled
// port. Resolving fewer pods than there are replicas is that outcome.
func TestGetPodNames_ResolvesEveryReplica(t *testing.T) {
	selector := map[string]string{"app": "web"}
	svc := kubeServiceWithPods(
		appPod("web-a", selector, v1.PodRunning, 3*time.Minute),
		appPod("web-b", selector, v1.PodRunning, 2*time.Minute),
		appPod("web-c", selector, v1.PodRunning, time.Minute),
	)

	pods := make([]string, 3)
	if err := svc.getPodNames(context.Background(), newDeploymentWorkload(appDeployment("web", 3, selector)), pods); err != nil {
		t.Fatalf("failed resolving the pods of a three-replica deployment: %v", err)
	}

	resolved := map[string]bool{}
	for _, p := range pods {
		if p == "" {
			t.Fatalf("resolved %v: a replica was left without a pod, so it would get no forward and no tunnel", pods)
		}
		if resolved[p] {
			t.Fatalf("resolved %v: the same pod twice, so one replica is forwarded to twice and another not at all", pods)
		}
		resolved[p] = true
	}
	for _, want := range []string{"web-a", "web-b", "web-c"} {
		if !resolved[want] {
			t.Errorf("resolved %v, which leaves out %s: that replica's containers get connection refused on the tunnelled port", pods, want)
		}
	}
}

// TestGetPodNames_DoesNotMatchADeploymentWithTheSamePrefix is the regression
// test for #123: `ktunnel expose react1` forwarded to pods of `react11`.
//
// Pods used to be found with a selector built by string concatenation from the
// deployment name, and the lookup that consumed it matched on prefix, so every
// deployment whose name extends another one's was a candidate. Resolving the
// pods through the deployment's own spec.selector makes it an exact label
// match, which is the only kind Kubernetes has.
func TestGetPodNames_DoesNotMatchADeploymentWithTheSamePrefix(t *testing.T) {
	svc := exposeFixture(t,
		ktunnelDeployment("react1", 1),
		ktunnelDeployment("react11", 1),
		labelledPod("react1", "react1-abc", v1.PodRunning),
		labelledPod("react11", "react11-xyz", v1.PodRunning),
	)

	pods := make([]string, 1)
	if err := svc.getPodNames(context.Background(), newDeploymentWorkload(ktunnelDeployment("react1", 1)), pods); err != nil {
		t.Fatalf("failed resolving the pods of react1: %v", err)
	}
	if pods[0] != "react1-abc" {
		t.Errorf("forwarding to %q, want react1-abc: the tunnel went to another deployment's pod, and the user is debugging the wrong application", pods[0])
	}
}
