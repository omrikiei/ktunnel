package k8s

import (
	"strings"
	"testing"
)

// #69: a Traefik ingress will not speak HTTPS to a backend unless the Service
// carries an annotation saying it should. The reporter spent three hours on
// it. Nothing about it is Traefik-specific from ktunnel's side -- every
// ingress controller has its own annotation for the same fact -- so the flag
// carries whatever the user's controller wants rather than one vendor's key.
func TestRenderManifests_PutsServiceAnnotationsOnTheService(t *testing.T) {
	out, err := RenderManifests(ManifestOptions{
		Namespace:  "dev",
		Name:       "myapp",
		TunnelPort: 28688,
		Scheme:     "tcp",
		RawPorts:   []string{"8000"},
		Image:      Image,
		ServiceAnnotations: map[string]string{
			"traefik.ingress.kubernetes.io/service.serversscheme": "https",
		},
	})
	if err != nil {
		t.Fatalf("RenderManifests: %v", err)
	}

	if !strings.Contains(out, "traefik.ingress.kubernetes.io/service.serversscheme: https") {
		t.Errorf("the service annotation is missing from the rendered manifests:\n%s", out)
	}

	// It belongs to the Service. On the Deployment it does nothing at all,
	// which is the failure mode that is hardest to notice.
	service := out[strings.Index(out, "kind: Service"):]
	if !strings.Contains(service, "traefik.ingress.kubernetes.io") {
		t.Error("the annotation landed somewhere other than the Service")
	}
}

func TestNewService_WithNoAnnotationsIsUnchanged(t *testing.T) {
	svc := newService("dev", "myapp", nil, "ClusterIP", nil)
	if len(svc.Annotations) != 0 {
		t.Errorf("a service with no annotations requested has %v", svc.Annotations)
	}
}
