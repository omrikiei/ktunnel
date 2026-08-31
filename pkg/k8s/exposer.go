package k8s

import (
	"context"
	"fmt"
	"strings"

	"github.com/omrikiei/ktunnel/pkg/creds"
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
	bundle *creds.Bundle,
	serviceType string,
	cpuReq, cpuLimit, memReq, memLimit int64,
) (*ResourceTracker, PodCredentials, error) {
	// The tracker holds what this call creates, and only that. It is returned
	// on the error paths too, so a caller that gives up can still remove a
	// deployment that was created before the service failed.
	tracker := NewResourceTracker(namespace, k.clients)

	// Built optimistically, as though this run will create the deployment
	// and can therefore secure it. Whether that holds is not known until
	// the plan below has looked at the cluster, and the pod spec depends on
	// the answer -- a mounted Secret means --tls and a secretKeyRef, and no
	// Secret means an inline token and no TLS -- so the build is a closure,
	// callable a second time if the answer turns out to be different.
	podCreds := PodCredentialsFor(name, bundle)

	// The objects are built by the same code that prints them under
	// --print-manifests, so what the command creates and what it says it
	// would create cannot drift apart.
	build := func(podCreds PodCredentials) (*appsv1.Deployment, *v12.Service, []v12.ServicePort, error) {
		return ManifestOptions{
			Namespace:             namespace,
			Name:                  name,
			TunnelPort:            tunnelPort,
			Scheme:                scheme,
			RawPorts:              rawPorts,
			PortName:              portName,
			Image:                 image,
			DeploymentOnly:        DeploymentOnly,
			NodeSelectorTags:      nodeSelectorTags,
			DeploymentLabels:      deploymentLabels,
			DeploymentAnnotations: deploymentAnnotations,
			PodTolerations:        podTolerations,
			Creds:                 podCreds,
			Bundle:                bundle,
			ServiceType:           serviceType,
			CPURequest:            cpuReq,
			CPULimit:              cpuLimit,
			MemRequest:            memReq,
			MemLimit:              memLimit,
		}.build()
	}

	deploymentTemplate, service, ports, err := build(podCreds)
	if err != nil {
		return tracker, PodCredentials{}, err
	}

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
	//
	// The whole plan is decided, and said, before the first write: see plan.go.
	plan, err := k.planExpose(namespace, name, deploymentTemplate, service, Reuse, DeploymentOnly)
	if err != nil {
		return tracker, PodCredentials{}, err
	}

	// An adopted deployment runs exactly as its author wrote it: it does not
	// mount ktunnel's Secret and its server never reads ktunnel's token.
	// Provisioning credentials for it would create a Secret nothing consumes
	// -- demanding a permission the people who hand-write these deployments
	// may well not have -- and would leave the client expecting a handshake
	// the adopted server cannot complete. So it gets none, and the caller is
	// told, rather than finding out through a failed connection.
	//
	// --reuse with nothing there is a different case: ktunnel creates the
	// deployment from its own template, and that run is secured like any
	// other.
	switch {
	case plan.existingDeployment != nil:
		if bundle != nil {
			log.Warnf("--reuse is tunnelling through deployment %s/%s as it stands, so this run is "+
				"UNENCRYPTED and UNAUTHENTICATED: ktunnel cannot mount credentials into a deployment it did not create", namespace, name)
		}
		podCreds = PodCredentials{}
	case bundle != nil:
		podCreds, err = k.provisionCredentials(namespace, name, bundle, tracker)
		if err != nil {
			return tracker, PodCredentials{}, err
		}
		// The Secret could not be created, so the pod spec that was built
		// on the assumption that it could is now wrong: rebuild it with an
		// inline token and no TLS.
		if !podCreds.mountsSecret() {
			deploymentTemplate, service, ports, err = build(podCreds)
			if err != nil {
				return tracker, PodCredentials{}, err
			}
		}
	}
	for _, line := range plan.describe() {
		log.Info(line)
	}

	deployment := plan.existingDeployment
	if deployment == nil {
		deployment, err = k.createDeployment(namespace, name, deploymentTemplate, tracker)
		if err != nil {
			return tracker, podCreds, err
		}
	}

	if !DeploymentOnly {
		newSvc := plan.existingService
		if newSvc == nil {
			newSvc, err = k.createService(namespace, name, service, tracker)
			if err != nil {
				return tracker, podCreds, err
			}
		}
		if newSvc.Spec.ClusterIP != "" {
			log.Infof("Exposed service's cluster ip is: %s", newSvc.Spec.ClusterIP)
		}
		warnOnUnroutedPorts(namespace, name, newSvc, deployment, ports)
	}

	watchForReady(deployment, readyChan)
	return tracker, podCreds, nil
}

// createDeployment creates the deployment the plan said was missing, and
// records it as this run's to remove.
func (k *KubeService) createDeployment(namespace, name string, template *appsv1.Deployment, tracker *ResourceTracker) (*appsv1.Deployment, error) {
	created, err := k.clients.Deployments.Create(context.Background(), template, v1.CreateOptions{})
	if err != nil {
		return nil, apiError("create", "deployment", namespace, name, err)
	}
	tracker.AddDeployment(name)
	log.Infof("Created deployment %s/%s", namespace, name)
	return created, nil
}

// createService is createDeployment for the Service.
func (k *KubeService) createService(namespace, name string, template *v12.Service, tracker *ResourceTracker) (*v12.Service, error) {
	created, err := k.clients.Services.Create(context.Background(), template, v1.CreateOptions{})
	if err != nil {
		return nil, apiError("create", "service", namespace, name, err)
	}
	tracker.AddService(name)
	log.Infof("Created service %s/%s", namespace, name)
	return created, nil
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

// provisionCredentials puts this run's credentials where the tunnel server
// container can read them, and reports how it managed to.
//
// The Secret is the way it should go: the private key and the token live in
// an object with its own RBAC, and the pod spec only references them. When
// the namespace forbids creating one, the run does not stop -- ktunnel's
// whole pitch is that it needs no special permissions -- but it does not
// pretend either. It keeps the token, which is what stops something in the
// cluster attaching as a client, and gives up encryption, because a private
// key inlined in a pod spec is readable by anyone with `get pods` and would
// be the entire channel rather than one revocable run's secret.
func (k *KubeService) provisionCredentials(namespace, name string, bundle *creds.Bundle, tracker *ResourceTracker) (PodCredentials, error) {
	if bundle == nil {
		// --insecure, and the standalone paths: exactly v2.3 behaviour.
		return PodCredentials{}, nil
	}
	if k.clients.Secrets == nil {
		return PodCredentials{Token: bundle.Token}, nil
	}

	_, err := k.clients.Secrets.Create(context.Background(), newSecret(namespace, name, bundle), v1.CreateOptions{})
	switch {
	case err == nil:
		tracker.AddSecret(name)
		return PodCredentials{SecretName: name}, nil
	case apierrors.IsAlreadyExists(err):
		// Left by a previous run that was killed rather than stopped. Its
		// contents are another run's credentials, so they are of no use to
		// this one: replace them, and own the result.
		if _, err := k.clients.Secrets.Update(context.Background(), newSecret(namespace, name, bundle), v1.UpdateOptions{}); err != nil {
			return PodCredentials{}, fmt.Errorf("secret %s/%s already exists and could not be updated: %w\n"+
				"delete it with `kubectl delete secret -n %s %s`, or pass --insecure to run without credentials",
				namespace, name, err, namespace, name)
		}
		tracker.AddSecret(name)
		return PodCredentials{SecretName: name}, nil
	case apierrors.IsForbidden(err):
		log.Warnf("cannot create secret %s/%s: %v", namespace, name, err)
		log.Warnf("falling back to an authenticated but UNENCRYPTED tunnel; "+
			"grant `secrets: create` in %s for encryption, or pass --insecure to disable both", namespace)
		return PodCredentials{Token: bundle.Token}, nil
	default:
		return PodCredentials{}, fmt.Errorf("failed creating secret %s/%s: %w", namespace, name, err)
	}
}
