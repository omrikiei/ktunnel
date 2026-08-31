package k8s

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/omrikiei/ktunnel/pkg/common"
	appsv1 "k8s.io/api/apps/v1"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// ManifestOptions is everything that decides what `expose` puts in the
// cluster: the flags, and nothing about the cluster itself.
//
// It exists so that the objects can be built without an API server in reach,
// which is what makes RenderManifests possible, and so that the printed
// manifests and the created objects come from one place rather than from two
// descriptions of the same thing that can drift.
type ManifestOptions struct {
	Namespace  string
	Name       string
	TunnelPort int
	Scheme     string
	// RawPorts are the port arguments as typed: `80`, `80:8000`,
	// `80:otherhost:8000`.
	RawPorts []string
	// PortName overrides the generated `<scheme>-<port>` name, for the
	// clusters that require a particular one.
	PortName              string
	Image                 string
	DeploymentOnly        bool
	NodeSelectorTags      map[string]string
	DeploymentLabels      map[string]string
	DeploymentAnnotations map[string]string
	PodTolerations        []v12.Toleration
	Cert, Key             string
	ServiceType           string
	CPURequest, CPULimit  int64
	MemRequest, MemLimit  int64
}

// build turns the options into the objects, and returns the service ports
// alongside them because the caller checks them against the service it ends up
// with.
//
// A port that cannot be parsed is an error here rather than a line on the log
// and a shorter list. `expose` used to skip it with a message and carry on,
// which meant a typo in one of several ports produced a working tunnel that
// was quietly missing the port you cared about -- and, when the list was built
// by index, a ServicePort for port 0 sent to the API server as if it had been
// asked for.
func (o ManifestOptions) build() (*appsv1.Deployment, *v12.Service, []v12.ServicePort, error) {
	protocol, ok := supportedSchemes[o.Scheme]
	if !ok {
		schemes := make([]string, 0, len(supportedSchemes))
		for scheme := range supportedSchemes {
			schemes = append(schemes, scheme)
		}
		sort.Strings(schemes)
		return nil, nil, nil, fmt.Errorf("unsupported scheme %q; --scheme takes one of %s",
			o.Scheme, strings.Join(schemes, ", "))
	}
	if len(o.RawPorts) == 0 {
		return nil, nil, nil, errors.New("no ports given; `ktunnel expose NAME PORT [PORT...]` needs at least one")
	}

	ports := make([]v12.ServicePort, 0, len(o.RawPorts))
	ctrPorts := make([]v12.ContainerPort, 0, len(o.RawPorts))
	for _, p := range o.RawPorts {
		parsed, err := common.ParsePorts(p)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cannot parse the port argument %q; it takes PORT, SOURCE:TARGET or SOURCE:HOST:TARGET: %w", p, err)
		}
		portname := fmt.Sprintf("%s-%d", o.Scheme, parsed.Source)
		if o.PortName != "" {
			portname = o.PortName
		}
		ports = append(ports, v12.ServicePort{
			Protocol:   protocol,
			Name:       portname,
			Port:       parsed.Source,
			TargetPort: intstr.FromInt(int(parsed.Source)),
		})
		ctrPorts = append(ctrPorts, v12.ContainerPort{
			ContainerPort: parsed.Source,
			Protocol:      protocol,
			Name:          portname,
		})
	}

	// Defensive copies: newDeployment writes ktunnel's two labels into the map
	// it is handed, and the caller's map is the one the flags were parsed
	// into. Rendering the manifests twice used to be enough to notice.
	deployment := newDeployment(
		o.Namespace, o.Name, o.TunnelPort, o.Image, ctrPorts,
		copyLabels(o.NodeSelectorTags),
		copyLabels(o.DeploymentLabels),
		copyLabels(o.DeploymentAnnotations),
		o.PodTolerations, o.Cert, o.Key,
		o.CPURequest, o.CPULimit, o.MemRequest, o.MemLimit,
	)
	if o.DeploymentOnly {
		return deployment, nil, ports, nil
	}
	return deployment, newService(o.Namespace, o.Name, ports, v12.ServiceType(o.ServiceType)), ports, nil
}

// RenderManifests returns the Deployment and Service `expose` would create, as
// a YAML document `kubectl apply -f -` accepts.
//
// It touches no cluster: the objects are built from the flags alone, so this
// works with no kubeconfig, against a cluster you cannot reach, and in a CI
// job that only wants the manifests checked in.
//
// #94 and #120 both arrived at hand-written manifests the same way -- they
// needed an image from their own registry and a security context their cluster
// admits, so they wrote the Deployment themselves and fought `-r` to get
// ktunnel to adopt it. `-r` adopts properly now, so this is no longer the way
// out of a bug; it is the convenience of starting from what ktunnel would have
// created rather than from a blank file.
func RenderManifests(options ManifestOptions) (string, error) {
	deployment, service, _, err := options.build()
	if err != nil {
		return "", err
	}

	// The TypeMeta the API server fills in for you is absent on an object
	// built in memory, and a document without apiVersion and kind is not
	// something kubectl will apply.
	deployment.TypeMeta.APIVersion = "apps/v1"
	deployment.TypeMeta.Kind = "Deployment"

	documents := []interface{}{deployment}
	if service != nil {
		service.TypeMeta.APIVersion = "v1"
		service.TypeMeta.Kind = "Service"
		documents = append(documents, service)
	}

	var out strings.Builder
	for i, document := range documents {
		encoded, err := yaml.Marshal(document)
		if err != nil {
			return "", fmt.Errorf("failed rendering the manifests: %w", err)
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		out.Write(encoded)
	}
	return out.String(), nil
}

// copyLabels returns a map the caller's flags cannot be written back through,
// and never nil, since newDeployment writes into the one it is given.
func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
