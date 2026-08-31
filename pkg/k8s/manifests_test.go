package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// exposeOptions is the options `ktunnel expose mysvc 80:8000` produces, with
// the defaults the flags carry.
func exposeOptions() ManifestOptions {
	return ManifestOptions{
		Namespace:   "team-a",
		Name:        "mysvc",
		TunnelPort:  28688,
		Scheme:      "tcp",
		RawPorts:    []string{"80:8000"},
		Image:       Image + ":v2.2.0",
		ServiceType: "ClusterIP",
		CPURequest:  100,
		CPULimit:    500,
		MemRequest:  100,
		MemLimit:    1000,
	}
}

// decodeManifests splits the rendered document and decodes both objects, the
// way `kubectl apply -f -` reads it. Anything this cannot parse is not a
// manifest anyone can apply.
func decodeManifests(t *testing.T, rendered string) (*appsv1.Deployment, *apiv1.Service) {
	t.Helper()
	var deployment *appsv1.Deployment
	var service *apiv1.Service

	for _, doc := range strings.Split(rendered, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var meta struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("a rendered document is not YAML: %v\n%s", err, doc)
		}
		switch meta.Kind {
		case "Deployment":
			if meta.APIVersion != "apps/v1" {
				t.Errorf("the Deployment says apiVersion %q, want apps/v1; kubectl cannot apply it", meta.APIVersion)
			}
			deployment = &appsv1.Deployment{}
			if err := yaml.Unmarshal([]byte(doc), deployment); err != nil {
				t.Fatalf("failed decoding the Deployment: %v", err)
			}
		case "Service":
			if meta.APIVersion != "v1" {
				t.Errorf("the Service says apiVersion %q, want v1; kubectl cannot apply it", meta.APIVersion)
			}
			service = &apiv1.Service{}
			if err := yaml.Unmarshal([]byte(doc), service); err != nil {
				t.Fatalf("failed decoding the Service: %v", err)
			}
		default:
			t.Errorf("unexpected object of kind %q in the output", meta.Kind)
		}
	}
	return deployment, service
}

// TestRenderManifests is the v2.3 item both #94 and #120 asked for on their
// way to something else: the Deployment and Service, as ktunnel would create
// them, to apply yourself.
//
// Every field a hand-written copy has to get right is checked here, because
// the whole value of the output is that it is not hand-written.
func TestRenderManifests(t *testing.T) {
	rendered, err := RenderManifests(exposeOptions())
	if err != nil {
		t.Fatalf("RenderManifests: %v", err)
	}

	deployment, service := decodeManifests(t, rendered)
	if deployment == nil {
		t.Fatal("no Deployment in the output")
	}
	if service == nil {
		t.Fatal("no Service in the output")
	}

	if deployment.Namespace != "team-a" || service.Namespace != "team-a" {
		t.Errorf("objects are in %q/%q, want team-a: applying them would put the tunnel in the wrong namespace",
			deployment.Namespace, service.Namespace)
	}
	if deployment.Name != "mysvc" || service.Name != "mysvc" {
		t.Errorf("objects are named %q/%q, want mysvc", deployment.Name, service.Name)
	}

	containers := deployment.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("the pod template has %d containers, want 1", len(containers))
	}
	if containers[0].Image != Image+":v2.2.0" {
		t.Errorf("the container image is %q, want the one --server-image selected", containers[0].Image)
	}
	if got := strings.Join(containers[0].Args, " "); !strings.Contains(got, "28688") {
		t.Errorf("the container args %q do not carry the tunnel port, so the server would listen on the wrong one", got)
	}

	// The service port and the container port are the two halves of the same
	// decision, and a manifest that gets one of them wrong routes nowhere.
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 80 {
		t.Errorf("service ports are %+v, want a single port 80", service.Spec.Ports)
	}
	if len(containers[0].Ports) != 1 || containers[0].Ports[0].ContainerPort != 80 {
		t.Errorf("container ports are %+v, want a single port 80", containers[0].Ports)
	}
	if service.Spec.Type != apiv1.ServiceTypeClusterIP {
		t.Errorf("the service type is %q, want ClusterIP", service.Spec.Type)
	}

	// The selector is what makes the Service reach the pod at all. It is the
	// single easiest thing to get wrong by hand.
	for label, want := range deployment.Spec.Selector.MatchLabels {
		if service.Spec.Selector[label] != want {
			t.Errorf("the service selector does not match the deployment's label %s=%s, so it routes to nothing", label, want)
		}
	}
}

// TestRenderManifests_DeploymentOnly: --deployment-only creates no Service, so
// the printed manifests must not contain one either.
func TestRenderManifests_DeploymentOnly(t *testing.T) {
	options := exposeOptions()
	options.DeploymentOnly = true

	rendered, err := RenderManifests(options)
	if err != nil {
		t.Fatalf("RenderManifests: %v", err)
	}

	deployment, service := decodeManifests(t, rendered)
	if deployment == nil {
		t.Fatal("no Deployment in the output")
	}
	if service != nil {
		t.Error("a Service was printed under --deployment-only, which creates none")
	}
}

// TestRenderManifests_RejectsWhatExposeWouldReject: the manifests are supposed
// to be what ktunnel would have created. A scheme or a port ktunnel would not
// accept has no manifest, and saying so beats printing something that differs
// from what the command does.
func TestRenderManifests_RejectsWhatExposeWouldReject(t *testing.T) {
	options := exposeOptions()
	options.Scheme = "carrier-pigeon"
	if _, err := RenderManifests(options); err == nil {
		t.Error("an unsupported scheme rendered manifests; expose would have refused it")
	}

	options = exposeOptions()
	options.RawPorts = []string{"not-a-port"}
	if _, err := RenderManifests(options); err == nil {
		t.Error("an unparseable port rendered manifests, so the printed Service would have no ports at all")
	}
}

// TestExposeAsService_UsesTheSameManifests pins the property that makes the
// printed output worth anything: it is the same code path the command runs,
// not a second description of it that can drift.
func TestExposeAsService_UsesTheSameManifests(t *testing.T) {
	name := "mysvc"
	svc := exposeFixture(t)
	if _, err := expose(t, svc, name, false); err != nil {
		t.Fatalf("expose failed: %v", err)
	}

	created, err := svc.clients.Deployments.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed reading the deployment expose created: %v", err)
	}

	options := exposeOptions()
	options.Namespace = "default"
	options.Name = name
	options.RawPorts = []string{"8080"}
	options.Image = Image
	rendered, err := RenderManifests(options)
	if err != nil {
		t.Fatalf("RenderManifests: %v", err)
	}
	printed, _ := decodeManifests(t, rendered)

	if printed.Spec.Template.Spec.Containers[0].Image != created.Spec.Template.Spec.Containers[0].Image {
		t.Errorf("the printed image is %q and the created one is %q",
			printed.Spec.Template.Spec.Containers[0].Image, created.Spec.Template.Spec.Containers[0].Image)
	}
	if len(printed.Spec.Template.Spec.Containers[0].Ports) != len(created.Spec.Template.Spec.Containers[0].Ports) {
		t.Error("the printed container ports differ from the ones expose created")
	}
	if printed.Spec.Selector.String() != created.Spec.Selector.String() {
		t.Errorf("the printed selector is %v and the created one is %v: applying the manifests would build a different object than the command does",
			printed.Spec.Selector, created.Spec.Selector)
	}
}
