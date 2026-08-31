package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	testclient "k8s.io/client-go/kubernetes/fake"
)

// privateRegistryImage is the setup both #120 and #94 describe: a
// tunnel-server deployment the user wrote themselves, because they need an
// image from their own registry and a security context their cluster admits.
const privateRegistryImage = "nexus.corp.example/ktunnel:v2.1.0"

// handWrittenDeployment is that deployment. Everything about it is
// deliberately not what newDeployment would produce.
func handWrittenDeployment(name string) *appsv1.Deployment {
	replicas := int32(1)
	runAsUser := int64(1000700000) // the kind of UID OpenShift assigns
	notPrivileged := false
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Labels:      map[string]string{"app.kubernetes.io/instance": name},
			Annotations: map[string]string{"sidecar.istio.io/inject": "false"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/instance": name}},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app.kubernetes.io/instance": name},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:  "ktunnel",
						Image: privateRegistryImage,
						SecurityContext: &v1.SecurityContext{
							RunAsUser:  &runAsUser,
							Privileged: &notPrivileged,
						},
					}},
				},
			},
		},
	}
}

// handWrittenService is the Service that goes with it, routing 8080 to the
// port the tunnel server listens on.
func handWrittenService(name string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{"app.kubernetes.io/instance": name},
		},
		Spec: v1.ServiceSpec{
			ClusterIP: "10.0.0.7",
			Selector:  map[string]string{"app.kubernetes.io/instance": name},
			Ports: []v1.ServicePort{{
				Name:       "tcp-8080",
				Protocol:   v1.ProtocolTCP,
				Port:       8080,
				TargetPort: intstr.FromInt(8080),
			}},
		},
	}
}

// exposeFixture wires a KubeService to a fake API server holding objects, and
// points the package-level clients at it too, since watchForReady polls those.
func exposeFixture(t *testing.T, objects ...runtime.Object) *KubeService {
	t.Helper()
	fake := testclient.NewSimpleClientset(objects...)

	clientMutex.Lock()
	deploymentsClient = fake.AppsV1().Deployments("default")
	podsClient = fake.CoreV1().Pods("default")
	svcClient = fake.CoreV1().Services("default")
	clientMutex.Unlock()

	return &KubeService{clients: &Clients{
		Deployments: fake.AppsV1().Deployments("default"),
		Pods:        fake.CoreV1().Pods("default"),
		Services:    fake.CoreV1().Services("default"),
	}}
}

// expose calls ExposeAsService with the arguments `ktunnel expose NAME 8080`
// would produce, so the tests differ only in what is already in the cluster.
func expose(t *testing.T, svc *KubeService, name string, reuse bool) (*ResourceTracker, error) {
	t.Helper()
	// Buffered: watchForReady's goroutine outlives the test, and an
	// unbuffered channel nobody reads would park it holding a send.
	readyChan := make(chan bool, 1)
	return svc.ExposeAsService(
		"default", name, 28688, "tcp", []string{"8080"}, "",
		Image, reuse, false, readyChan,
		map[string]string{}, map[string]string{}, map[string]string{}, nil,
		"", "", "ClusterIP",
		100, 500, 100, 1000,
	)
}

// TestExposeAsService_ReuseLeavesTheDeploymentAlone is the regression test for
// #120 and #94: `--reuse` did not reuse.
//
// It merge-patched ktunnel's own template over the existing deployment,
// keeping only the labels and selector a patch cannot change. Both reporters
// hit the same wall: they hand-write a deployment because they need an image
// from a private registry and a security context their cluster admits, pass
// -r so that ktunnel adopts it, and ktunnel overwrites the image with
// docker.io/omrieival/ktunnel, rolls a second revision, and leaves a pod stuck
// pulling an image the cluster cannot reach.
func TestExposeAsService_ReuseLeavesTheDeploymentAlone(t *testing.T) {
	name := "mysvc"
	svc := exposeFixture(t, handWrittenDeployment(name), handWrittenService(name))

	before, err := svc.clients.Deployments.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed reading the deployment under test: %v", err)
	}

	if _, err := expose(t, svc, name, true); err != nil {
		t.Fatalf("expose --reuse failed against an existing deployment: %v", err)
	}

	after, err := svc.clients.Deployments.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed re-reading the deployment: %v", err)
	}

	if got := after.Spec.Template.Spec.Containers[0].Image; got != privateRegistryImage {
		t.Errorf("the container image is now %q, was %q: ktunnel overwrote it, and the new pod hangs pulling an image the cluster cannot reach",
			got, privateRegistryImage)
	}
	if sc := after.Spec.Template.Spec.Containers[0].SecurityContext; sc == nil || sc.RunAsUser == nil {
		t.Error("the security context is gone: the deployment was hand-written to carry one the cluster would admit")
	}
	if !equalPodSpec(&before.Spec.Template.Spec, &after.Spec.Template.Spec) {
		t.Errorf("the pod template changed, so the deployment rolls a new revision:\nbefore: %+v\nafter:  %+v",
			before.Spec.Template.Spec, after.Spec.Template.Spec)
	}
}

// TestExposeAsService_ReuseAdoptsWithoutTakingOwnership pins the other half of
// adopting: ktunnel does not delete what it did not create.
//
// Teardown used to key off --reuse rather than off what happened. That is
// wrong in both directions, and this is the direction that costs the user
// their own objects if the flag ever stops being consulted.
func TestExposeAsService_ReuseAdoptsWithoutTakingOwnership(t *testing.T) {
	name := "mysvc"
	svc := exposeFixture(t, handWrittenDeployment(name), handWrittenService(name))

	tracker, err := expose(t, svc, name, true)
	if err != nil {
		t.Fatalf("expose --reuse failed: %v", err)
	}

	deployments, services := tracker.GetTrackedResources()
	if len(deployments)+len(services) != 0 {
		t.Errorf("adopting tracked %v and %v for deletion; Ctrl+C would delete objects ktunnel did not create", deployments, services)
	}
}

// TestExposeAsService_ReuseCreatesAndOwnsWhatWasMissing is the other
// direction. `--reuse` means "use it if it is there", so against an empty
// namespace it creates -- and what it creates, it cleans up.
//
// Teardown skipped cleanup entirely whenever --reuse was passed, so this
// combination left a deployment and a service in the cluster on every run.
func TestExposeAsService_ReuseCreatesAndOwnsWhatWasMissing(t *testing.T) {
	name := "mysvc"
	svc := exposeFixture(t)

	tracker, err := expose(t, svc, name, true)
	if err != nil {
		t.Fatalf("expose --reuse failed against an empty namespace: %v", err)
	}

	if _, err := svc.clients.Deployments.Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("--reuse against an empty namespace created no deployment: %v", err)
	}

	deployments, services := tracker.GetTrackedResources()
	if len(deployments) != 1 || len(services) != 1 {
		t.Errorf("tracked %v and %v; what ktunnel created it has to remove, and --reuse used to skip teardown outright and leave both behind",
			deployments, services)
	}
}

// TestExposeAsService_WithoutReuseNamesTheObjectAndTheFix covers the error a
// user meets first. It used to read "deployment with same name already
// exists", which names neither the object nor the way out of it.
func TestExposeAsService_WithoutReuseNamesTheObjectAndTheFix(t *testing.T) {
	name := "mysvc"
	svc := exposeFixture(t, handWrittenDeployment(name))

	_, err := expose(t, svc, name, false)
	if err == nil {
		t.Fatal("expose overwrote an existing deployment without --reuse")
	}
	for _, want := range []string{"default/mysvc", "--reuse", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not mention %q, so it does not say which object or what to do about it", err, want)
		}
	}
}

// TestServiceTargetPorts covers the three shapes a service port can take,
// since an adopted service is written by someone else and may use any of them.
func TestServiceTargetPorts(t *testing.T) {
	deployment := handWrittenDeployment("mysvc")
	deployment.Spec.Template.Spec.Containers[0].Ports = []v1.ContainerPort{
		{Name: "grpc", ContainerPort: 9090},
	}

	svc := &v1.Service{Spec: v1.ServiceSpec{Ports: []v1.ServicePort{
		{Port: 80, TargetPort: intstr.FromInt(8080)}, // explicit number
		{Port: 9090, TargetPort: intstr.FromString("grpc")},
		{Port: 7000, TargetPort: intstr.FromString("nope")},
		{Port: 6379}, // unset: defaults to the service port
	}}}

	routed, unresolved := serviceTargetPorts(svc, deployment)
	for _, want := range []int32{8080, 9090, 6379} {
		if !routed[want] {
			t.Errorf("port %d is not reported as routed, so ktunnel would warn about a service that works", want)
		}
	}
	if len(unresolved) != 1 || unresolved[0] != "nope" {
		t.Errorf("unresolved target ports are %v, want exactly [nope]", unresolved)
	}
}

// equalPodSpec compares the parts of a pod spec that a merge patch of
// ktunnel's own template would have replaced.
func equalPodSpec(a, b *v1.PodSpec) bool {
	if len(a.Containers) != len(b.Containers) {
		return false
	}
	for i := range a.Containers {
		if a.Containers[i].Image != b.Containers[i].Image ||
			a.Containers[i].Name != b.Containers[i].Name ||
			len(a.Containers[i].Args) != len(b.Containers[i].Args) {
			return false
		}
	}
	return true
}
