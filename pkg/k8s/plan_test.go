package k8s

import (
	"context"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// contains reports whether any of the lines mentions every one of the
// substrings, so a test can pin what a line says without pinning its wording.
func contains(lines []string, substrings ...string) bool {
	for _, line := range lines {
		matched := true
		for _, s := range substrings {
			if !strings.Contains(line, s) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// exitLine is the line of a plan that says what happens on Ctrl+C, which is
// the one a user reads to find out what they get to keep.
func exitLine(t *testing.T, lines []string) string {
	t.Helper()
	for _, line := range lines {
		if strings.HasPrefix(line, "On exit") {
			return line
		}
	}
	t.Fatalf("%q has no line saying what happens on exit", lines)
	return ""
}

// planned runs the same planning ExposeAsService does, for the arguments
// `ktunnel expose NAME 8080` produces.
func planned(t *testing.T, svc *KubeService, name string, reuse, deploymentOnly bool) *exposePlan {
	t.Helper()
	plan, err := planFor(t, svc, name, reuse, deploymentOnly)
	if err != nil {
		t.Fatalf("planning expose failed: %v", err)
	}
	return plan
}

func planFor(t *testing.T, svc *KubeService, name string, reuse, deploymentOnly bool) (*exposePlan, error) {
	t.Helper()
	deployment := newDeployment("default", name, 28688, Image, nil,
		map[string]string{}, map[string]string{}, map[string]string{}, nil, "", "", 100, 500, 100, 1000)
	service := newService("default", name, nil, "ClusterIP")
	return svc.planExpose("default", name, deployment, service, reuse, deploymentOnly)
}

// podContainers is a one-container pod spec, for deployments whose contents
// beyond the image under test do not matter.
func podContainers(name, image string) []v1.Container {
	return []v1.Container{{Name: name, Image: image}}
}

// TestPlanExpose_SaysWhatItWillCreate covers the first half of #134: a run
// against an empty namespace says what it is about to put in the cluster
// before it puts it there.
func TestPlanExpose_SaysWhatItWillCreate(t *testing.T) {
	svc := exposeFixture(t)

	lines := planned(t, svc, "kewlapp", false, false).describe()

	if !contains(lines, "default") {
		t.Errorf("nothing in %q names the namespace, which is the one thing a user checks before letting it run", lines)
	}
	if !contains(lines, "create", "deployment", "kewlapp") {
		t.Errorf("%q does not say the deployment will be created", lines)
	}
	if !contains(lines, "create", "service", "kewlapp") {
		t.Errorf("%q does not say the service will be created", lines)
	}
	if !contains(lines, "remove", "deployment kewlapp") || !contains(lines, "remove", "service kewlapp") {
		t.Errorf("%q does not say both objects are removed on exit", lines)
	}
}

// TestPlanExpose_SaysWhatItWillAdopt is the --reuse side. Adopting is the
// case where saying so up front matters most: the objects are the user's, and
// the plan is what tells them ktunnel is not about to rewrite them.
func TestPlanExpose_SaysWhatItWillAdopt(t *testing.T) {
	name := "mysvc"
	svc := exposeFixture(t, handWrittenDeployment(name), handWrittenService(name))

	lines := planned(t, svc, name, true, false).describe()

	if !contains(lines, "existing", "deployment", name) {
		t.Errorf("%q does not say the deployment is used as it stands rather than created", lines)
	}
	if !contains(lines, "existing", "deployment", privateRegistryImage) {
		t.Errorf("%q does not say which deployment is being used; the image is how a user tells theirs from ktunnel's", lines)
	}
	if !contains(lines, "existing", "service", name) {
		t.Errorf("%q does not say the service is used as it stands", lines)
	}
	if exit := exitLine(t, lines); !strings.Contains(exit, "nothing") {
		t.Errorf("%q promises to remove objects ktunnel did not create", exit)
	}
}

// TestPlanExpose_MixedSaysWhichOneGoes is the case the exit message has to get
// right: one object adopted, the other created. Only the created one is
// removed on exit.
func TestPlanExpose_MixedSaysWhichOneGoes(t *testing.T) {
	name := "mysvc"
	svc := exposeFixture(t, handWrittenDeployment(name))

	lines := planned(t, svc, name, true, false).describe()

	if !contains(lines, "existing", "deployment", name) {
		t.Errorf("%q does not say the existing deployment is used as it stands", lines)
	}
	if !contains(lines, "create", "service", name) {
		t.Errorf("%q does not say the missing service is created", lines)
	}
	exit := exitLine(t, lines)
	if !strings.Contains(exit, "remove service "+name) {
		t.Errorf("%q does not say the created service is removed on exit", exit)
	}
	if !strings.Contains(exit, "leave deployment "+name) {
		t.Errorf("%q does not say the existing deployment is left alone on exit", exit)
	}
}

// TestPlanExpose_DeploymentOnlySaysNothingAboutAService: --deployment-only
// creates no service, so the plan must not mention one.
func TestPlanExpose_DeploymentOnlySaysNothingAboutAService(t *testing.T) {
	svc := exposeFixture(t)

	lines := planned(t, svc, "kewlapp", false, true).describe()

	if contains(lines, "service") {
		t.Errorf("%q mentions a service under --deployment-only, which creates none", lines)
	}
}

// TestExposeAsService_RefusesBeforeCreatingAnything is why the plan is
// computed up front rather than narrated as it goes.
//
// The deployment was created first and the service second, so a namespace that
// already held a service of that name -- without --reuse -- got a deployment
// created, then the run failed, and the deployment stayed behind. Deciding the
// whole plan before the first write means the run either does all of it or
// touches nothing.
func TestExposeAsService_RefusesBeforeCreatingAnything(t *testing.T) {
	name := "mysvc"
	svc := exposeFixture(t, handWrittenService(name))

	_, err := expose(t, svc, name, false)
	if err == nil {
		t.Fatal("expose overwrote an existing service without --reuse")
	}
	if !strings.Contains(err.Error(), "service") {
		t.Errorf("the error %q does not say the service is what stopped it", err)
	}

	_, getErr := svc.clients.Deployments.Get(context.Background(), name, metav1.GetOptions{})
	if getErr == nil {
		t.Errorf("the deployment was created before the run failed on the service, and is left behind in the cluster")
	} else if !apierrors.IsNotFound(getErr) {
		t.Fatalf("failed reading the deployment: %v", getErr)
	}
}

// TestPlanInject_SaysWhatItWillPatch is #134 on the inject path. Injecting
// modifies an object the user owns and rolls every one of its pods, which is
// the last place to find out afterwards.
func TestPlanInject_SaysWhatItWillPatch(t *testing.T) {
	namespace, name := "default", "api"
	svc := exposeFixture(t)
	containers := podContainers("app", "app:latest")
	if err := createDeployment(svc.clients.Deployments, name, 3, &containers); err != nil {
		t.Fatalf("failed creating the deployment under test: %v", err)
	}

	plan, err := svc.PlanInject(namespace, name, "test-image:latest", 28688)
	if err != nil {
		t.Fatalf("PlanInject: %v", err)
	}
	lines := plan.Describe(true)

	if !contains(lines, namespace) {
		t.Errorf("nothing in %q names the namespace", lines)
	}
	if !contains(lines, name, "test-image:latest") {
		t.Errorf("%q does not say which deployment gets which image", lines)
	}
	if !contains(lines, "3") {
		t.Errorf("%q does not say how many pods roll; every replica restarts", lines)
	}
	if !contains(lines, "28688-28690") {
		t.Errorf("%q does not say which local ports are taken; three replicas take three of them", lines)
	}
	if !contains(lines, "remove") {
		t.Errorf("%q does not say the container is removed on exit", lines)
	}
}

// TestPlanInject_SaysWhenTheSidecarStays pins the --eject=false half: what is
// left in the cluster afterwards is the part the plan exists to state.
func TestPlanInject_SaysWhenTheSidecarStays(t *testing.T) {
	namespace, name := "default", "api"
	svc := exposeFixture(t)
	containers := podContainers("app", "app:latest")
	if err := createDeployment(svc.clients.Deployments, name, 1, &containers); err != nil {
		t.Fatalf("failed creating the deployment under test: %v", err)
	}

	plan, err := svc.PlanInject(namespace, name, "test-image:latest", 28688)
	if err != nil {
		t.Fatalf("PlanInject: %v", err)
	}
	lines := plan.Describe(false)

	if !contains(lines, "--eject") {
		t.Errorf("%q does not say the container is left in the deployment on exit", lines)
	}
}

// TestPlanInject_SaysWhenNothingChanges: a deployment that already carries the
// sidecar is not patched and does not roll, and the plan should not claim it
// will be.
func TestPlanInject_SaysWhenNothingChanges(t *testing.T) {
	namespace, name, image := "default", "api", "test-image:latest"
	svc := exposeFixture(t)
	containers := podContainers("app", "app:latest")
	containers = append(containers, podContainers("ktunnel", image)...)
	if err := createDeployment(svc.clients.Deployments, name, 1, &containers); err != nil {
		t.Fatalf("failed creating the deployment under test: %v", err)
	}

	plan, err := svc.PlanInject(namespace, name, image, 28688)
	if err != nil {
		t.Fatalf("PlanInject: %v", err)
	}
	if !plan.AlreadyInjected {
		t.Fatal("a deployment that already has the sidecar is planned as an injection, so the plan promises a rollout that will not happen")
	}
	if !contains(plan.Describe(true), "already") {
		t.Errorf("%q does not say the deployment already has the sidecar", plan.Describe(true))
	}
}
