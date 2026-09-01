package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	testclient "k8s.io/client-go/kubernetes/fake"
)

// appStatefulSet is an ordinary application StatefulSet, the kind #91 is
// about: a PHP application declared as a StatefulSet with no Deployment
// anywhere, selecting its pods by whatever labels its author chose.
func appStatefulSet(name string, replicas int32, selector map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: selector},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selector},
				Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "app", Image: "app:latest"}}},
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1},
	}
}

// statefulSetFixture is exposeFixture for the statefulset path: a fake API
// server holding the given objects, with the package-level clients pointed at
// it too, since the rollout wait polls those.
func statefulSetFixture(t *testing.T, objects ...runtime.Object) *KubeService {
	t.Helper()
	fake := testclient.NewSimpleClientset(objects...)

	clientMutex.Lock()
	deploymentsClient = fake.AppsV1().Deployments("default")
	statefulSetsClient = fake.AppsV1().StatefulSets("default")
	podsClient = fake.CoreV1().Pods("default")
	svcClient = fake.CoreV1().Services("default")
	clientMutex.Unlock()

	return &KubeService{clients: &Clients{
		Deployments:  fake.AppsV1().Deployments("default"),
		StatefulSets: fake.AppsV1().StatefulSets("default"),
		Pods:         fake.CoreV1().Pods("default"),
		Services:     fake.CoreV1().Services("default"),
	}}
}

// TestInjectSidecar_StatefulSet is #91: an application that exists only as a
// StatefulSet could not be injected into at all. `ktunnel inject deployment
// owl-app` answered `deployments.apps "owl-app" not found`, which is true and
// useless -- there was no other subcommand to reach for.
//
// Every replica is injected, for the same reason as a Deployment and more so:
// the sidecar's listeners are pod-local, so a pod without one has the tunnelled
// port closed, and a StatefulSet's pods are deliberately not interchangeable.
func TestInjectSidecar_StatefulSet(t *testing.T) {
	namespace, name, image := "default", "owl-app", "test-image:latest"
	port := 28688
	svc := statefulSetFixture(t, appStatefulSet(name, 3, map[string]string{"app": name}))

	readyChan := make(chan bool, 1)
	ok, err := svc.InjectSidecar(&namespace, &name, KindStatefulSet, &port, image, PodCredentials{}, readyChan, nil)
	if err != nil {
		t.Fatalf("failed injecting into a statefulset: %v; this is the whole of #91", err)
	}
	if !ok {
		t.Fatal("InjectSidecar reported failure without an error for a statefulset")
	}

	sts, err := svc.clients.StatefulSets.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed reading the statefulset back: %v", err)
	}
	if !hasSidecar(sts.Spec.Template.Spec, image) {
		t.Error("the sidecar is not in the statefulset's pod template, so no pod would come back carrying it")
	}
	if statefulSetReplicaCount(sts) != 3 {
		t.Errorf("the statefulset now wants %d replicas; injecting must not scale the user's workload to make its own life easier",
			statefulSetReplicaCount(sts))
	}
}

// TestInjectSidecar_StatefulSetNotFoundNamesTheObject: the failure that sent
// #91's reporter here in the first place was an error naming a resource kind
// they had not asked for. Whatever ktunnel cannot find, it says which object
// in which namespace, and which flags decide both.
func TestInjectSidecar_StatefulSetNotFoundNamesTheObject(t *testing.T) {
	namespace, name := "default", "owl-app"
	port := 28688
	svc := statefulSetFixture(t)

	readyChan := make(chan bool, 1)
	_, err := svc.InjectSidecar(&namespace, &name, KindStatefulSet, &port, "img:latest", PodCredentials{}, readyChan, nil)
	if err == nil {
		t.Fatal("injecting into a statefulset that does not exist reported success")
	}
	for _, want := range []string{"statefulset default/owl-app", "--namespace", "--context"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not mention %q, so the user cannot tell which object was looked for, or with which flags", err, want)
		}
	}
}

// TestRemoveSidecar_StatefulSet is the other half of the same promise: eject
// has to be a clean reverse on this path too, or `inject` leaves a container
// behind in a workload ktunnel does not own.
func TestRemoveSidecar_StatefulSet(t *testing.T) {
	namespace, name, image := "default", "owl-app", "test-image:latest"
	port := 28688
	svc := statefulSetFixture(t, appStatefulSet(name, 1, map[string]string{"app": name}))

	readyChan := make(chan bool, 1)
	if _, err := svc.InjectSidecar(&namespace, &name, KindStatefulSet, &port, image, PodCredentials{}, readyChan, nil); err != nil {
		t.Fatalf("failed injecting: %v", err)
	}

	ejectReady := make(chan bool, 1)
	ok, err := svc.RemoveSidecar(&namespace, &name, KindStatefulSet, image, ejectReady, nil)
	if err != nil {
		t.Fatalf("failed ejecting from a statefulset: %v", err)
	}
	if !ok {
		t.Fatal("RemoveSidecar reported failure without an error")
	}

	sts, err := svc.clients.StatefulSets.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed reading the statefulset back: %v", err)
	}
	if hasSidecar(sts.Spec.Template.Spec, image) {
		t.Error("the ktunnel container is still in the statefulset after eject; ktunnel has left debris in a workload it does not own")
	}
}

// TestRemoveSidecar_StatefulSetWithNothingToEject: the run whose rollout never
// finished still reaches teardown, and there is no sidecar to take out. That
// is the state that was asked for, not a failure -- and the caller blocks on
// readyChan before exiting, so something has to be sent on it either way.
func TestRemoveSidecar_StatefulSetWithNothingToEject(t *testing.T) {
	namespace, name := "default", "owl-app"
	svc := statefulSetFixture(t, appStatefulSet(name, 1, map[string]string{"app": name}))

	readyChan := make(chan bool, 1)
	ok, err := svc.RemoveSidecar(&namespace, &name, KindStatefulSet, "test-image:latest", readyChan, nil)
	if err != nil {
		t.Errorf("ejecting a sidecar that is not there failed with %v", err)
	}
	if !ok {
		t.Error("ejecting a sidecar that is not there reported failure")
	}
	select {
	case <-readyChan:
	default:
		t.Error("nothing was sent on readyChan, so `inject statefulset` would hang waiting for a rollout that is not coming")
	}
}

// TestGetPodNames_StatefulSetUsesItsOwnSelector is #171/#115 on the new path.
// Pods are resolved through the workload's own spec.selector, never through
// the two labels `expose` puts on the Deployments it creates -- an application
// StatefulSet carries neither, and matching on them found nothing at all.
func TestGetPodNames_StatefulSetUsesItsOwnSelector(t *testing.T) {
	selector := map[string]string{"app": "owl-app", "tier": "php"}
	svc := statefulSetFixture(t, appPod("owl-app-0", selector, v1.PodRunning, time.Minute))

	pods := make([]string, 1)
	if err := svc.getPodNames(context.Background(), newStatefulSetWorkload(appStatefulSet("owl-app", 1, selector)), pods); err != nil {
		t.Fatalf("failed resolving the pods of a statefulset: %v", err)
	}
	if pods[0] != "owl-app-0" {
		t.Errorf("resolved %q, want the statefulset's own pod owl-app-0", pods[0])
	}
}

// TestGetPodNames_StatefulSetRefusesASelectorThatMatchesEverything: an absent
// or empty selector converts to "match every pod in the namespace", and
// forwarding into an unrelated pod while reporting success is worse than
// refusing by name.
func TestGetPodNames_StatefulSetRefusesASelectorThatMatchesEverything(t *testing.T) {
	svc := statefulSetFixture(t, appPod("postgres-0", map[string]string{"app": "postgres"}, v1.PodRunning, time.Hour))

	replicas := int32(1)
	for _, tc := range []struct {
		name string
		sts  *appsv1.StatefulSet
	}{
		{
			name: "no selector",
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "owl-app", Namespace: "default"},
				Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
			},
		},
		{
			name: "empty selector",
			sts:  appStatefulSet("owl-app", 1, map[string]string{}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := make([]string, 1)
			err := svc.getPodNames(context.Background(), newStatefulSetWorkload(tc.sts), pods)
			if err == nil {
				t.Fatalf("resolved %q for a statefulset that selects nothing in particular; the tunnel would go to an unrelated pod", pods[0])
			}
			if !strings.Contains(err.Error(), "statefulset default/owl-app") {
				t.Errorf("the error %q does not name the statefulset, so the user cannot tell which object to fix", err)
			}
		})
	}
}

// TestGetPodNames_StatefulSetOrdersPodsByOrdinal is the one place a
// StatefulSet must not behave like a Deployment.
//
// A Deployment's pods are interchangeable, so they are ordered newest-first to
// survive the rollout window. A StatefulSet's pods are the opposite by
// definition: owl-app-0 is a different thing from owl-app-1, and the local
// port each is reached on has to mean the same pod on every reconnect -- a
// debugger attached for owl-app-0 must not quietly become owl-app-1 because a
// pod was rescheduled and is now the newest.
//
// There is no rollout window to survive here anyway: a StatefulSet deletes a
// pod before creating its replacement, so two pods never share an ordinal.
func TestGetPodNames_StatefulSetOrdersPodsByOrdinal(t *testing.T) {
	selector := map[string]string{"app": "owl-app"}
	// The ages are what a StatefulSet that was created and left alone looks
	// like: it starts its pods in ordinal order and waits for each, so
	// owl-app-0 is the oldest pod in the set. Newest-first is then exactly
	// backwards -- it would hand back 2, 1, 0 -- and any single rescheduled
	// pod reshuffles it again.
	svc := statefulSetFixture(t,
		appPod("owl-app-0", selector, v1.PodRunning, 3*time.Minute),
		appPod("owl-app-1", selector, v1.PodRunning, 2*time.Minute),
		appPod("owl-app-2", selector, v1.PodRunning, 1*time.Minute),
	)

	pods := make([]string, 3)
	if err := svc.getPodNames(context.Background(), newStatefulSetWorkload(appStatefulSet("owl-app", 3, selector)), pods); err != nil {
		t.Fatalf("failed resolving the pods of a three-replica statefulset: %v", err)
	}
	for i, want := range []string{"owl-app-0", "owl-app-1", "owl-app-2"} {
		if pods[i] != want {
			t.Errorf("local port %d resolves to %q, want %s: a statefulset's pods are not interchangeable, and the port a pod is reached on has to stay that pod's",
				28688+i, pods[i], want)
		}
	}
}

// TestPlanInject_StatefulSetSaysWhatItWillPatch: injecting rolls every pod of
// a workload the user owns, which is the last thing to find out afterwards --
// and the object it names has to be the one they typed.
func TestPlanInject_StatefulSetSaysWhatItWillPatch(t *testing.T) {
	namespace, name := "default", "owl-app"
	svc := statefulSetFixture(t, appStatefulSet(name, 3, map[string]string{"app": name}))

	plan, err := svc.PlanInject(namespace, name, KindStatefulSet, "test-image:latest", 28688)
	if err != nil {
		t.Fatalf("PlanInject: %v", err)
	}
	lines := plan.Describe(true)
	for _, want := range []string{"statefulset", name, "test-image:latest", "3", "28688-28690", "remove"} {
		if !contains(lines, want) {
			t.Errorf("%q does not mention %q", lines, want)
		}
	}
	if contains(lines, "deployment") {
		t.Errorf("%q calls a statefulset a deployment, which is the error message #91 was reported against", lines)
	}
}

// TestPlanInject_StatefulSetOnDeleteSaysPodsWillNotRestart is the one thing a
// StatefulSet can do that a Deployment cannot.
//
// With updateStrategy OnDelete, writing a new pod template restarts nothing:
// the controller waits for someone to delete each pod by hand. ktunnel would
// otherwise sit on "waiting for the rollout" until its deadline, having
// already patched the object, with nothing on screen to say why.
func TestPlanInject_StatefulSetOnDeleteSaysPodsWillNotRestart(t *testing.T) {
	namespace, name := "default", "owl-app"
	sts := appStatefulSet(name, 2, map[string]string{"app": name})
	sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
	svc := statefulSetFixture(t, sts)

	plan, err := svc.PlanInject(namespace, name, KindStatefulSet, "test-image:latest", 28688)
	if err != nil {
		t.Fatalf("PlanInject: %v", err)
	}
	lines := plan.Describe(true)
	if !contains(lines, "OnDelete") {
		t.Errorf("%q does not name the OnDelete update strategy, which is why nothing is going to happen", lines)
	}
	if !contains(lines, "kubectl delete pod") {
		t.Errorf("%q does not say what the user has to do to make the sidecar appear", lines)
	}
}

// TestPlanInject_StatefulSetPartitionSaysSomePodsAreLeftBehind: a rolling
// update with spec.updateStrategy.rollingUpdate.partition set only updates
// ordinals at or above the partition. The pods below it keep the old template,
// never get a sidecar, and the rollout never converges -- so ktunnel waits out
// its deadline for a reason that is visible in the spec beforehand.
func TestPlanInject_StatefulSetPartitionSaysSomePodsAreLeftBehind(t *testing.T) {
	namespace, name := "default", "owl-app"
	partition := int32(2)
	sts := appStatefulSet(name, 3, map[string]string{"app": name})
	sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type:          appsv1.RollingUpdateStatefulSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: &partition},
	}
	svc := statefulSetFixture(t, sts)

	plan, err := svc.PlanInject(namespace, name, KindStatefulSet, "test-image:latest", 28688)
	if err != nil {
		t.Fatalf("PlanInject: %v", err)
	}
	if !contains(plan.Describe(true), "partition") {
		t.Errorf("%q does not mention the partition, so the pods it leaves on the old template are a surprise", plan.Describe(true))
	}
}

// TestPlanInject_StatefulSetSaysWhenNothingChanges: a workload that already
// carries the sidecar is not patched and does not roll, and the plan must not
// promise a rollout that will not happen.
func TestPlanInject_StatefulSetSaysWhenNothingChanges(t *testing.T) {
	namespace, name, image := "default", "owl-app", "test-image:latest"
	sts := appStatefulSet(name, 1, map[string]string{"app": name})
	sts.Spec.Template.Spec.Containers = append(sts.Spec.Template.Spec.Containers, v1.Container{Name: "ktunnel", Image: image})
	svc := statefulSetFixture(t, sts)

	plan, err := svc.PlanInject(namespace, name, KindStatefulSet, image, 28688)
	if err != nil {
		t.Fatalf("PlanInject: %v", err)
	}
	if !plan.AlreadyInjected {
		t.Fatal("a statefulset that already has the sidecar is planned as an injection, so the plan promises a rollout that will not happen")
	}
}

// readyStatefulSet is a StatefulSet whose status reports a finished rollout:
// every pod ready, and the revision it is being updated to is the revision it
// is on.
func readyStatefulSet(name string) *appsv1.StatefulSet {
	sts := appStatefulSet(name, 1, map[string]string{"app": name})
	sts.Status = appsv1.StatefulSetStatus{
		ObservedGeneration: 1,
		Replicas:           1,
		ReadyReplicas:      1,
		UpdatedReplicas:    1,
		CurrentRevision:    "rev-2",
		UpdateRevision:     "rev-2",
	}
	return sts
}

// TestWatchWorkloadReady_StatefulSetAlreadyReady: the rollout that finished
// before ktunnel started watching is the case that hung `expose` for years. It
// polls rather than watches for exactly this reason, and the statefulset path
// has to inherit that rather than reinvent a watch.
func TestWatchWorkloadReady_StatefulSetAlreadyReady(t *testing.T) {
	name := "already-ready-sts"
	statefulSetFixture(t, readyStatefulSet(name))

	readyChan := make(chan bool, 1)
	watchWorkloadReady(newStatefulSetWorkload(readyStatefulSet(name)), readyChan)

	select {
	case ready := <-readyChan:
		if !ready {
			t.Fatal("a statefulset whose rollout is complete was reported not ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out; the rollout completed before ktunnel started observing it and was never noticed")
	}
}

// TestWatchWorkloadReady_StatefulSetMidRollout: pods that are ready are not
// the same thing as pods that carry the new template. Between the two, the
// statefulset controller has the old revision still current -- and forwarding
// then reaches pods with no tunnel server in them.
func TestWatchWorkloadReady_StatefulSetMidRollout(t *testing.T) {
	name := "rolling-sts"
	sts := readyStatefulSet(name)
	sts.Status.CurrentRevision = "rev-1"
	sts.Status.UpdateRevision = "rev-2"
	sts.Status.UpdatedReplicas = 0
	statefulSetFixture(t, sts)

	readyChan := make(chan bool, 1)
	watchWorkloadReady(newStatefulSetWorkload(sts), readyChan)

	select {
	case ready := <-readyChan:
		t.Fatalf("a statefulset still on its old revision was reported ready=%v; ktunnel would forward to pods that do not have the sidecar yet", ready)
	case <-time.After(2 * time.Second):
	}
}

// TestWatchWorkloadReady_StatefulSetGone: a workload that disappears out from
// under the wait reports not-ready rather than polling until its deadline.
func TestWatchWorkloadReady_StatefulSetGone(t *testing.T) {
	name := "vanished-sts"
	statefulSetFixture(t)

	readyChan := make(chan bool, 1)
	watchWorkloadReady(newStatefulSetWorkload(appStatefulSet(name, 1, map[string]string{"app": name})), readyChan)

	select {
	case ready := <-readyChan:
		if ready {
			t.Fatal("a statefulset that is not there was reported ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out; a missing statefulset %q should report failure", name)
	}
}

// TestPortForward_StatefulSetResolvesItsOwnPods covers the seam between the
// CLI and the cluster: the kind the user typed has to reach the lookup, or
// `inject statefulset` goes looking for a Deployment of that name and reports
// #91's error from one level deeper.
func TestPortForward_StatefulSetResolvesItsOwnPods(t *testing.T) {
	name := "owl-app"
	selector := map[string]string{"app": name}
	svc := statefulSetFixture(t, appStatefulSet(name, 0, selector))

	stopChan := make(chan struct{})
	sourcePorts, _, err := svc.PortForward(context.Background(), KindStatefulSet, "default", name, "28688", stopChan)
	close(stopChan)

	if err != nil && strings.Contains(err.Error(), "deployment") {
		t.Fatalf("forwarding to a statefulset failed by looking for a deployment: %v", err)
	}
	if len(sourcePorts) != 0 {
		t.Fatalf("returned %d ports for a statefulset scaled to zero; the caller would run a tunnel client over a port nothing is forwarding", len(sourcePorts))
	}
}

// TestAPIError_ForbiddenStatefulSetNamesTheResourceToGrant: the forbidden
// message is the one that sends a user to their RBAC, so the resource it tells
// them to grant has to be the one they were refused. It listed deployments and
// services flatly, which for `inject statefulset` is a list that does not
// contain the answer.
func TestAPIError_ForbiddenStatefulSetNamesTheResourceToGrant(t *testing.T) {
	resource := schema.GroupResource{Group: "apps", Resource: "statefulsets"}
	err := apiError("update", string(KindStatefulSet), "team-a", "owl-app", apierrors.NewForbidden(resource, "owl-app", errors.New("nope")))

	// Asserted on ktunnel's own sentence rather than on the word
	// "statefulsets", which the wrapped client-go cause contains anyway --
	// that would pass against the old message, which listed deployments and
	// services and left the reader to guess.
	if !strings.Contains(err.Error(), "on statefulsets") {
		t.Errorf("%q does not say which resource to grant; it is the list a user copies into a Role", err)
	}
	if strings.Contains(err.Error(), "on deployments") {
		t.Errorf("%q tells a user who was refused a statefulset to ask for deployments", err)
	}
	for _, want := range []string{"statefulset team-a/owl-app", "docs/security.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not mention %q", err, want)
		}
	}
}
