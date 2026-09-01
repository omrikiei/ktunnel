package k8s

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	apiv1 "k8s.io/api/core/v1"
)

func SetLogLevel(l log.Level) {
	log.SetLevel(l)
	if l.String() == "verbose" || l.String() == "debug" {
		SetVerbose(true)
	}
}

// injectToWorkload appends the ktunnel container to the workload's pod
// template and waits for the rollout that follows.
//
// Nothing here is Deployment-specific -- a StatefulSet's spec.template is the
// same PodTemplateSpec, and the sidecar goes into it the same way. Only the
// client that writes the object back and the controller's account of "rolled
// out" differ, and both of those live on workload.
func (k *KubeService) injectToWorkload(w *workload, c *apiv1.Container, image string, readyChan chan<- bool) (bool, error) {
	if hasSidecar(*w.podSpec, image) {
		log.Warn(fmt.Sprintf("%s already injected to the %s", image, w.kind))
		watchWorkloadReady(w, readyChan)
		return true, nil
	}
	w.podSpec.Containers = append(w.podSpec.Containers, *c)
	updated, err := k.update(context.Background(), w, "add the ktunnel container to")
	if err != nil {
		return false, err
	}
	watchWorkloadReady(updated, readyChan)
	return true, nil
}

// InjectSidecar adds the tunnel server to a workload's pod template as a
// sidecar, and reports on readyChan once the resulting rollout has finished.
// kind is what the user typed: `inject deployment` or `inject statefulset`.
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
// The same reasoning is stronger for a StatefulSet (#91), whose pods are
// deliberately not interchangeable: forwarding to whichever one of them
// ktunnel happened to pick would be forwarding to an identity the user did not
// choose. So every ordinal is injected and every ordinal is tunnelled, in
// ordinal order -- see sortPods. Targeting a single ordinal (`--ordinal 0`, to
// debug owl-app-0 alone and leave the rest of the set alone) is a real thing
// to want and is deliberately not offered yet: the sidecar lives in the shared
// pod template, so restricting it to one pod means either editing that pod
// directly, which no controller would leave in place, or a partitioned rolling
// update, which changes the user's spec in a way eject cannot cleanly reverse.
// Narrowing only the forwarding half -- every pod injected, one pod tunnelled
// -- needs none of that, and is where an --ordinal flag should start.
//
// What this is about to do to the workload, including how many pods it
// restarts and how many local ports it takes, is stated by PlanInject before
// the rollout starts.
func (k *KubeService) InjectSidecar(namespace, objectName *string, kind WorkloadKind, port *int, image string, podCreds PodCredentials, readyChan chan<- bool, kubecontext *string) (bool, error) {
	log.Infof("Injecting tunnel sidecar to %s %s/%s", kind, *namespace, *objectName)
	cpuReq := int64(100) // in milli-cpu
	cpuLimit := int64(500)
	memReq := int64(100) // in mega-bytes
	memLimit := int64(1000)
	co := newContainer(*port, image, []apiv1.ContainerPort{}, podCreds, cpuReq, cpuLimit, memReq, memLimit)
	w, err := k.getWorkload(context.Background(), kind, *namespace, *objectName)
	if err != nil {
		return false, err
	}
	_, err = k.injectToWorkload(w, co, image, readyChan)
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

func (k *KubeService) RemoveSidecar(namespace, objectName *string, kind WorkloadKind, image string, readyChan chan<- bool, kubecontext *string) (bool, error) {
	log.Infof("Removing tunnel sidecar from %s %s/%s", kind, *namespace, *objectName)
	w, err := k.getWorkload(context.Background(), kind, *namespace, *objectName)
	if err != nil {
		return false, err
	}
	// Nothing to eject is not a failure: the workload is already in the
	// state that was asked for. It used to come back as `IMAGE is not present
	// on spec`, logged as `Failed removing tunnel sidecar` -- an error, naming
	// an image rather than the object -- for a run whose rollout never
	// finished, or a container someone had already taken out by hand.
	if !hasSidecar(*w.podSpec, image) {
		log.Infof("Nothing to eject from %s: no container in it runs %s", w, image)
		// Non-blocking, because there is no rollout to wait for and the
		// caller is about to read this before it exits. Every caller passes a
		// buffered channel; a caller that does not is not left hanging on a
		// send instead.
		select {
		case readyChan <- true:
		default:
		}
		return true, nil
	}
	_, err = removeFromSpec(w.podSpec, image)
	if err != nil {
		return false, err
	}
	updated, err := k.update(context.Background(), w, "remove the ktunnel container from")
	if err != nil {
		return false, err
	}
	watchWorkloadReady(updated, readyChan)
	return true, nil
}
