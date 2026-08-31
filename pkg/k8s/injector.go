package k8s

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func SetLogLevel(l log.Level) {
	log.SetLevel(l)
	if l.String() == "verbose" || l.String() == "debug" {
		SetVerbose(true)
	}
}

func injectToDeployment(o *appsv1.Deployment, c *apiv1.Container, image string, readyChan chan<- bool) (bool, error) {
	if hasSidecar(o.Spec.Template.Spec, image) {
		log.Warn(fmt.Sprintf("%s already injected to the deployment", image))
		watchForReady(o, readyChan)
		return true, nil
	}
	o.Spec.Template.Spec.Containers = append(o.Spec.Template.Spec.Containers, *c)
	u, updateErr := deploymentsClient.Update(context.Background(), o, metav1.UpdateOptions{
		TypeMeta:     metav1.TypeMeta{},
		DryRun:       nil,
		FieldManager: "",
	})
	if updateErr != nil {
		return false, updateErr
	}
	watchForReady(u, readyChan)
	return true, nil
}

// InjectSidecar adds the tunnel server to a deployment's pod template as a
// sidecar, and reports on readyChan once the resulting rollout has finished.
//
// Every replica is injected and every replica is tunnelled. This used to
// refuse outright on a deployment with more than one replica, and the
// alternative to refusing is not "forward to one of them".
//
// The sidecar's listeners are pod-local: the tunnel server binds the tunnel
// ports inside the pod it was injected into, and only that pod's containers
// reach your machine through them. Forwarding to one arbitrary pod of N would
// therefore leave N-1 pods with the port closed and nothing to say which pod
// is the working one -- a deployment where a third of the requests reach your
// laptop and the rest get connection refused is a worse thing to debug than a
// deployment where none of them do.
//
// So the choice is all of them or none of them, and ktunnel takes all of them:
// one port-forward and one tunnel client per replica, every one of them
// carrying traffic to the same local service. PortForward has always built a
// forward per pod, so this is the refusal being removed rather than a fan-out
// being added. It costs one local port per replica, taken consecutively from
// --port, and one gRPC stream per replica.
//
// Replicas added after the tunnel is up are not picked up until the tunnel is
// rebuilt, since the set of pods is resolved once per attempt.
//
// What this is about to do to the deployment, including how many pods it
// restarts and how many local ports it takes, is stated by PlanInject before
// the rollout starts.
func (k *KubeService) InjectSidecar(namespace, objectName *string, port *int, image string, cert string, key string, readyChan chan<- bool, kubecontext *string) (bool, error) {
	log.Infof("Injecting tunnel sidecar to %s/%s", *namespace, *objectName)
	cpuReq := int64(100) // in milli-cpu
	cpuLimit := int64(500)
	memReq := int64(100) // in mega-bytes
	memLimit := int64(1000)
	co := newContainer(*port, image, []apiv1.ContainerPort{}, cert, key, cpuReq, cpuLimit, memReq, memLimit)
	obj, err := k.clients.Deployments.Get(context.Background(), *objectName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	_, err = injectToDeployment(obj, co, image, readyChan)
	if err != nil {
		return false, err
	}
	return true, nil
}

func removeFromSpec(s *apiv1.PodSpec, image string) (bool, error) {
	if !hasSidecar(*s, image) {
		return true, fmt.Errorf("%s is not present on spec", image)
	}
	cIndex := -1
	for i, c := range s.Containers {
		if c.Image == image {
			cIndex = i
			break
		}
	}

	if cIndex != -1 {
		containers := s.Containers
		s.Containers = append(containers[:cIndex], containers[cIndex+1:]...)
		return true, nil
	} else {
		return false, errors.New("container not found on spec")
	}
}

func (k *KubeService) RemoveSidecar(namespace, objectName *string, image string, readyChan chan<- bool, kubecontext *string) (bool, error) {
	log.Infof("Removing tunnel sidecar from %s/%s", *namespace, *objectName)
	obj, err := k.clients.Deployments.Get(context.Background(), *objectName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	_, err = removeFromSpec(&obj.Spec.Template.Spec, image)
	if err != nil {
		return false, err
	}
	u, updateErr := k.clients.Deployments.Update(context.Background(), obj, metav1.UpdateOptions{
		TypeMeta:     metav1.TypeMeta{},
		DryRun:       nil,
		FieldManager: "",
	})
	if updateErr != nil {
		return false, updateErr
	}
	watchForReady(u, readyChan)
	return true, nil
}
