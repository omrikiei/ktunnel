package k8s

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A plan is what a command will do to the cluster, decided before it does any
// of it, so that it can be said out loud first.
//
// Both commands used to narrate as they went: "Created deployment x", then
// "Created service y", each line after the fact. That reads fine when
// everything works, and badly otherwise. `expose` creates the deployment
// before the service, so a namespace that already held a service of that name
// -- without --reuse -- got a deployment created, then failed, and left the
// deployment behind. And with --reuse the interesting question is the one
// narration cannot answer in time: which of these objects is mine, and which
// will disappear when I press Ctrl+C (#134).

// plannedObject is one object a run will create or adopt.
type plannedObject struct {
	kind   string // "deployment" or "service"
	name   string
	adopt  bool   // adopted: it is already there and stays there
	detail string // what it is, in one line, so the user can recognise it
}

func (o plannedObject) line(namespace string) string {
	if o.adopt {
		return fmt.Sprintf("  use the existing %s %s/%s as it is (%s); it will be neither modified nor deleted",
			o.kind, namespace, o.name, o.detail)
	}
	return fmt.Sprintf("  create %s %s/%s (%s)", o.kind, namespace, o.name, o.detail)
}

// exposePlan is what ExposeAsService will do.
type exposePlan struct {
	namespace string
	objects   []plannedObject

	// The objects that are already there, kept so that acting on the plan
	// does not read them a second time and reach a different conclusion.
	existingDeployment *appsv1.Deployment
	existingService    *apiv1.Service
}

// describe is the pre-flight summary, one line per element.
func (p *exposePlan) describe() []string {
	lines := []string{fmt.Sprintf("In namespace %s, ktunnel will:", p.namespace)}
	var created, adopted []string
	for _, o := range p.objects {
		lines = append(lines, o.line(p.namespace))
		if o.adopt {
			adopted = append(adopted, fmt.Sprintf("%s %s", o.kind, o.name))
		} else {
			created = append(created, fmt.Sprintf("%s %s", o.kind, o.name))
		}
	}

	switch {
	case len(created) == 0:
		lines = append(lines, "On exit it will remove nothing: every object was already there, and is left as it is.")
	case len(adopted) == 0:
		lines = append(lines, fmt.Sprintf("On exit it will remove what it created: %s.", strings.Join(created, ", ")))
	default:
		lines = append(lines, fmt.Sprintf("On exit it will remove %s, and leave %s as it was.",
			strings.Join(created, ", "), strings.Join(adopted, ", ")))
	}
	return lines
}

// planExpose reads what is already in the namespace and decides, for each
// object, whether this run creates it or uses the one that is there.
//
// Every reason to refuse is found here, before the first write: an object that
// exists without --reuse, or a namespace that cannot be read at all.
func (k *KubeService) planExpose(
	namespace, name string,
	deploymentTemplate *appsv1.Deployment,
	serviceTemplate *apiv1.Service,
	reuse, deploymentOnly bool,
) (*exposePlan, error) {
	plan := &exposePlan{namespace: namespace}

	existingDeployment, err := k.clients.Deployments.Get(context.Background(), name, v1.GetOptions{})
	switch {
	case err == nil:
		if !reuse {
			return nil, fmt.Errorf("deployment %s/%s already exists; pass --reuse to tunnel through it as it is, or --force to replace it", namespace, name)
		}
		plan.existingDeployment = existingDeployment
		plan.objects = append(plan.objects, plannedObject{
			kind: "deployment", name: name, adopt: true, detail: describePodTemplate(existingDeployment),
		})
	case apierrors.IsNotFound(err):
		plan.objects = append(plan.objects, plannedObject{
			kind: "deployment", name: name, detail: describePodTemplate(deploymentTemplate),
		})
	default:
		// Anything else -- forbidden, API server unreachable -- used to fall
		// through to "deployment with same name already exists", which sends
		// the user to look at an object rather than at their permissions.
		return nil, fmt.Errorf("failed reading deployment %s/%s: %w", namespace, name, err)
	}

	if deploymentOnly {
		return plan, nil
	}

	existingService, err := k.clients.Services.Get(context.Background(), name, v1.GetOptions{})
	switch {
	case err == nil:
		if !reuse {
			return nil, fmt.Errorf("service %s/%s already exists; pass --reuse to tunnel through it as it is, or --force to replace it", namespace, name)
		}
		plan.existingService = existingService
		plan.objects = append(plan.objects, plannedObject{
			kind: "service", name: name, adopt: true, detail: describeService(existingService),
		})
	case apierrors.IsNotFound(err):
		plan.objects = append(plan.objects, plannedObject{
			kind: "service", name: name, detail: describeService(serviceTemplate),
		})
	default:
		return nil, fmt.Errorf("failed reading service %s/%s: %w", namespace, name, err)
	}

	return plan, nil
}

// describeService is the one-line summary of a service: enough of it to tell
// the user's own service from the one ktunnel would have created.
func describeService(svc *apiv1.Service) string {
	serviceType := string(svc.Spec.Type)
	if serviceType == "" {
		serviceType = string(apiv1.ServiceTypeClusterIP)
	}
	if len(svc.Spec.Ports) == 0 {
		return fmt.Sprintf("%s, no ports", serviceType)
	}
	ports := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		ports = append(ports, fmt.Sprintf("%d->%s", p.Port, p.TargetPort.String()))
	}
	return fmt.Sprintf("%s, port(s) %s", serviceType, strings.Join(ports, ", "))
}

// InjectPlan is what InjectSidecar will do to a deployment.
type InjectPlan struct {
	Namespace string
	Name      string
	Image     string
	Replicas  int32
	Port      int

	// AlreadyInjected: the deployment already runs this image, so injecting
	// changes nothing and no rollout follows.
	AlreadyInjected bool
}

// PlanInject reads the deployment and works out what injecting into it means,
// without touching it.
func (k *KubeService) PlanInject(namespace, name, image string, port int) (*InjectPlan, error) {
	deployment, err := k.clients.Deployments.Get(context.Background(), name, v1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed reading deployment %s/%s: %w", namespace, name, err)
	}
	return &InjectPlan{
		Namespace:       namespace,
		Name:            name,
		Image:           image,
		Replicas:        replicaCount(deployment),
		Port:            port,
		AlreadyInjected: hasSidecar(deployment.Spec.Template.Spec, image),
	}, nil
}

// Describe is the pre-flight summary for `inject`. eject is the --eject flag,
// which decides whether the container is taken back out on exit.
//
// Injecting modifies an object the user owns and restarts every one of its
// pods. Both of those belong in front of the rollout rather than behind it.
func (p *InjectPlan) Describe(eject bool) []string {
	lines := []string{fmt.Sprintf("In namespace %s, ktunnel will:", p.Namespace)}
	if p.AlreadyInjected {
		lines = append(lines, fmt.Sprintf("  leave deployment %s/%s as it is: it already runs %s, so no rollout follows",
			p.Namespace, p.Name, p.Image))
	} else {
		lines = append(lines, fmt.Sprintf("  add the ktunnel container (%s) to deployment %s/%s, which restarts its %d pod(s)",
			p.Image, p.Namespace, p.Name, p.Replicas))
	}
	// Said whether or not the sidecar is new, because the port arithmetic is
	// the surprise either way: N replicas take N local ports, not one.
	if p.Replicas > 1 {
		lines = append(lines, fmt.Sprintf("  tunnel each of the %d replicas separately, on local ports %d-%d",
			p.Replicas, p.Port, p.Port+int(p.Replicas)-1))
	} else {
		lines = append(lines, fmt.Sprintf("  tunnel it on local port %d", p.Port))
	}
	if eject {
		lines = append(lines, fmt.Sprintf("On exit it will remove that container from %s/%s.", p.Namespace, p.Name))
	} else {
		lines = append(lines, fmt.Sprintf("On exit it will leave that container in %s/%s (--eject=false), and its pods keep running it.",
			p.Namespace, p.Name))
	}
	return lines
}
