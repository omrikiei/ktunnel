package k8s

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/omrikiei/ktunnel/pkg/common"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	v12 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var supportedSchemes = map[string]v12.Protocol{
	"tcp":      v12.ProtocolTCP,
	"udp":      v12.ProtocolUDP,
	"grpc-web": v12.ProtocolTCP,
}

func (k *KubeService) ExposeAsService(
	namespace, name string,
	tunnelPort int,
	scheme string,
	rawPorts []string,
	portName string,
	image string,
	Reuse bool,
	DeploymentOnly bool,
	readyChan chan<- bool,
	nodeSelectorTags map[string]string,
	deploymentLabels map[string]string,
	deploymentAnnotations map[string]string,
	podTolerations []v12.Toleration,
	cert, key string,
	serviceType string,
	cpuReq, cpuLimit, memReq, memLimit int64,
) (*ResourceTracker, error) {
	// The tracker holds what this call creates, and only that. It is returned
	// on the error paths too, so a caller that gives up can still remove a
	// deployment that was created before the service failed.
	tracker := NewResourceTracker(namespace, k.clients)
	protocol, ok := supportedSchemes[scheme]
	if !ok {
		return tracker, errors.New("unsupported scheme")
	}
	// Appended rather than indexed: a port that fails to parse is skipped
	// with a message, and indexing left a zero-valued entry in its place --
	// a ServicePort for port 0, sent to the API server as if it were asked
	// for.
	ports := make([]v12.ServicePort, 0, len(rawPorts))
	ctrPorts := make([]v12.ContainerPort, 0, len(rawPorts))
	for _, p := range rawPorts {
		parsed, err := common.ParsePorts(p)
		if err != nil {
			log.Errorf("Failed to parse %s, skipping", p)
			continue
		}
		portname := fmt.Sprintf("%s-%d", scheme, parsed.Source)
		if portName != "" {
			portname = portName
		}
		ports = append(ports, v12.ServicePort{
			Protocol: protocol,
			Name:     portname,
			Port:     parsed.Source,
			TargetPort: intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: parsed.Source,
				StrVal: "",
			},
		})
		ctrPorts = append(ctrPorts, v12.ContainerPort{
			ContainerPort: parsed.Source,
			Protocol:      protocol,
			Name:          portname,
		})
	}

	deploymentTemplate := newDeployment(
		namespace,
		name,
		tunnelPort,
		image,
		ctrPorts,
		nodeSelectorTags,
		deploymentLabels,
		deploymentAnnotations,
		podTolerations,
		cert,
		key,
		cpuReq,
		cpuLimit,
		memReq,
		memLimit,
	)

	service := newService(namespace, name, ports, v12.ServiceType(serviceType))

	// --reuse adopts. It does not write.
	//
	// It used to merge-patch ktunnel's own template over the existing
	// deployment -- ktunnel's image, ktunnel's resources, ktunnel's security
	// context -- keeping only the labels and selector that a patch cannot
	// change anyway. For the people who asked for it that is the opposite of
	// reuse: they hand-write a deployment precisely because they need an
	// image from their own registry and a security context their cluster
	// admits, and ktunnel overwrote both, rolled a new revision, and left a
	// pod stuck pulling an image the cluster cannot reach (#120, #94).
	//
	// So an existing object is used exactly as it stands. Nothing ktunnel
	// did not create is modified, and nothing ktunnel did not create is
	// deleted -- the tracker is what teardown works from, and only a create
	// adds to it.
	deployment, err := k.adoptOrCreateDeployment(namespace, name, deploymentTemplate, Reuse, tracker)
	if err != nil {
		return tracker, err
	}

	if !DeploymentOnly {
		newSvc, err := k.adoptOrCreateService(namespace, name, service, Reuse, tracker)
		if err != nil {
			return tracker, err
		}
		if newSvc.Spec.ClusterIP != "" {
			log.Infof("Exposed service's cluster ip is: %s", newSvc.Spec.ClusterIP)
		}
		warnOnUnroutedPorts(namespace, name, newSvc, deployment, ports)
	}

	watchForReady(deployment, readyChan)
	return tracker, nil
}

// adoptOrCreateDeployment returns the deployment the tunnel will run in,
// creating it only if it is not already there.
func (k *KubeService) adoptOrCreateDeployment(namespace, name string, template *appsv1.Deployment, reuse bool, tracker *ResourceTracker) (*appsv1.Deployment, error) {
	existing, err := k.clients.Deployments.Get(context.Background(), name, v1.GetOptions{})
	switch {
	case err == nil:
		if !reuse {
			return nil, fmt.Errorf("deployment %s/%s already exists; pass --reuse to tunnel through it as it is, or --force to replace it", namespace, name)
		}
		log.Infof("Reusing deployment %s/%s as it is (%s); ktunnel will neither modify nor delete it",
			namespace, name, describePodTemplate(existing))
		return existing, nil
	case apierrors.IsNotFound(err):
		created, err := k.clients.Deployments.Create(context.Background(), template, v1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed creating deployment %s/%s: %w", namespace, name, err)
		}
		tracker.AddDeployment(name)
		log.Infof("Created deployment %s/%s", namespace, name)
		return created, nil
	default:
		// Anything else -- forbidden, API server unreachable -- used to fall
		// through to "deployment with same name already exists", which sends
		// the user to look at an object rather than at their permissions.
		return nil, fmt.Errorf("failed reading deployment %s/%s: %w", namespace, name, err)
	}
}

// adoptOrCreateService is adoptOrCreateDeployment for the Service.
func (k *KubeService) adoptOrCreateService(namespace, name string, template *v12.Service, reuse bool, tracker *ResourceTracker) (*v12.Service, error) {
	existing, err := k.clients.Services.Get(context.Background(), name, v1.GetOptions{})
	switch {
	case err == nil:
		if !reuse {
			return nil, fmt.Errorf("service %s/%s already exists; pass --reuse to tunnel through it as it is, or --force to replace it", namespace, name)
		}
		log.Infof("Reusing service %s/%s as it is; ktunnel will neither modify nor delete it", namespace, name)
		return existing, nil
	case apierrors.IsNotFound(err):
		created, err := k.clients.Services.Create(context.Background(), template, v1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed creating service %s/%s: %w", namespace, name, err)
		}
		tracker.AddService(name)
		log.Infof("Created service %s/%s", namespace, name)
		return created, nil
	default:
		return nil, fmt.Errorf("failed reading service %s/%s: %w", namespace, name, err)
	}
}

// describePodTemplate is the one-line summary of an adopted deployment, so
// that "reusing" says what is being reused rather than only that something is.
func describePodTemplate(d *appsv1.Deployment) string {
	images := make([]string, 0, len(d.Spec.Template.Spec.Containers))
	for _, c := range d.Spec.Template.Spec.Containers {
		images = append(images, c.Image)
	}
	if len(images) == 0 {
		return fmt.Sprintf("%d replica(s), no containers", replicaCount(d))
	}
	return fmt.Sprintf("%d replica(s), image %s", replicaCount(d), strings.Join(images, ", "))
}

// warnOnUnroutedPorts says so when the service does not route to a port the
// tunnel server will be listening on inside the pod.
//
// A warning and not an error. --reuse means the objects are the user's, and
// overruling them on ktunnel's reading of their service is how --reuse got
// into trouble in the first place. But a service that routes nowhere near the
// tunnel is silence at the far end with nothing to look at, so it is said
// loudly and with the fix in it.
func warnOnUnroutedPorts(namespace, name string, svc *v12.Service, deployment *appsv1.Deployment, wanted []v12.ServicePort) {
	routed, unresolved := serviceTargetPorts(svc, deployment)
	for _, p := range wanted {
		want := p.TargetPort.IntVal
		if want == 0 || routed[want] {
			continue
		}
		if len(unresolved) > 0 {
			log.Warnf("service %s/%s has no port targeting %d, and its named target port(s) %s do not match a container port on %s; if nothing in the cluster reaches the tunnel on %d, that is where to look",
				namespace, name, want, strings.Join(unresolved, ", "), name, want)
			continue
		}
		log.Warnf("service %s/%s does not route to port %d, so nothing in the cluster will reach the tunnel there; add a port with targetPort: %d to the service, or drop --reuse and let ktunnel create it",
			namespace, name, want, want)
	}
}

// serviceTargetPorts returns the container ports a service routes to, and the
// names of any target ports that could not be resolved against the deployment
// behind it.
func serviceTargetPorts(svc *v12.Service, deployment *appsv1.Deployment) (map[int32]bool, []string) {
	named := map[string]int32{}
	for _, c := range deployment.Spec.Template.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name != "" {
				named[p.Name] = p.ContainerPort
			}
		}
	}

	routed := map[int32]bool{}
	var unresolved []string
	for _, p := range svc.Spec.Ports {
		if p.TargetPort.Type == intstr.String {
			if container, ok := named[p.TargetPort.StrVal]; ok {
				routed[container] = true
			} else {
				unresolved = append(unresolved, p.TargetPort.StrVal)
			}
			continue
		}
		// An unset targetPort defaults to the service port. The API server
		// fills that in, so this only matters for an object that has not
		// been through it.
		if p.TargetPort.IntVal != 0 {
			routed[p.TargetPort.IntVal] = true
		} else {
			routed[p.Port] = true
		}
	}
	return routed, unresolved
}

// TeardownExposedService deletes the deployment and service that expose
// created. It is idempotent: a resource that is already gone is not an error,
// because teardown races with anything else that may have removed it -- a
// `kubectl delete` from another terminal, or a previous teardown of the same
// session -- and reporting "not found" as a failure sends the user looking for
// resources that were, in fact, cleaned up.
func (k *KubeService) TeardownExposedService(name string, DeploymentOnly bool) error {
	if !DeploymentOnly {
		log.Infof("Deleting service %s", name)
		err := k.clients.Services.Delete(context.Background(), name, v1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	log.Infof("Deleting deployment %s", name)
	err := k.clients.Deployments.Delete(context.Background(), name, v1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
