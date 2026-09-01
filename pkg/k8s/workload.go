package k8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadKind is what `inject` was pointed at.
//
// It exists because #91 is not a bug in the injector: appending a container to
// a pod template, rolling it out and forwarding to the resulting pods is the
// same operation whatever owns the template. Only three things differ between
// the kinds -- which client reads the object, how the controller reports a
// finished rollout, and the order the pods are handed back in -- so those are
// the only three places that switch on this, and a DaemonSet would be a fourth
// constant plus a case in each.
//
// The string value is the error-message vocabulary too: "statefulset ns/name",
// which is also `kubectl get statefulset -n ns`.
type WorkloadKind string

const (
	KindDeployment  WorkloadKind = "deployment"
	KindStatefulSet WorkloadKind = "statefulset"
)

func (k WorkloadKind) String() string { return string(k) }

// workload is a Deployment or a StatefulSet reduced to what ktunnel needs from
// either: how its pods are selected, how many there should be, and the pod
// template the sidecar goes into.
//
// It keeps the object it was built from rather than copying fields out of it,
// because injecting means writing the same object back with one container
// added. Copying would discard whatever else the user's spec holds.
type workload struct {
	kind      WorkloadKind
	namespace string
	name      string
	replicas  int32
	selector  *metav1.LabelSelector
	// podSpec points into the live object below, so appending to its
	// Containers and then updating that object is the whole of an injection.
	podSpec *apiv1.PodSpec

	deployment  *appsv1.Deployment
	statefulSet *appsv1.StatefulSet
}

func newDeploymentWorkload(d *appsv1.Deployment) *workload {
	return &workload{
		kind:       KindDeployment,
		namespace:  d.Namespace,
		name:       d.Name,
		replicas:   replicaCount(d),
		selector:   d.Spec.Selector,
		podSpec:    &d.Spec.Template.Spec,
		deployment: d,
	}
}

func newStatefulSetWorkload(s *appsv1.StatefulSet) *workload {
	return &workload{
		kind:        KindStatefulSet,
		namespace:   s.Namespace,
		name:        s.Name,
		replicas:    statefulSetReplicaCount(s),
		selector:    s.Spec.Selector,
		podSpec:     &s.Spec.Template.Spec,
		statefulSet: s,
	}
}

// String is how a workload appears in an error or a plan line: the kind the
// user typed, then the namespace and name the flags resolved to. client-go's
// own errors name neither, and between those two lies the answer most of the
// time -- see apiError.
func (w *workload) String() string {
	return fmt.Sprintf("%s %s/%s", w.kind, w.namespace, w.name)
}

// getWorkload reads the object `inject` was pointed at.
func (k *KubeService) getWorkload(ctx context.Context, kind WorkloadKind, namespace, name string) (*workload, error) {
	switch kind {
	case KindStatefulSet:
		s, err := k.clients.StatefulSets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, apiError("read", string(kind), namespace, name, err)
		}
		return newStatefulSetWorkload(s), nil
	default:
		d, err := k.clients.Deployments.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, apiError("read", string(KindDeployment), namespace, name, err)
		}
		return newDeploymentWorkload(d), nil
	}
}

// update writes the workload back, having had its pod template changed, and
// returns the object the API server accepted -- which is what the rollout wait
// then observes.
func (k *KubeService) update(ctx context.Context, w *workload, verb string) (*workload, error) {
	opts := metav1.UpdateOptions{}
	switch w.kind {
	case KindStatefulSet:
		s, err := k.clients.StatefulSets.Update(ctx, w.statefulSet, opts)
		if err != nil {
			return nil, apiError(verb, string(w.kind), w.namespace, w.name, err)
		}
		return newStatefulSetWorkload(s), nil
	default:
		d, err := k.clients.Deployments.Update(ctx, w.deployment, opts)
		if err != nil {
			return nil, apiError(verb, string(w.kind), w.namespace, w.name, err)
		}
		return newDeploymentWorkload(d), nil
	}
}

// statefulSetReplicaCount is replicaCount for a StatefulSet: spec.replicas is
// a pointer the API server defaults to 1, so nil is one pod and not none.
func statefulSetReplicaCount(s *appsv1.StatefulSet) int32 {
	if s.Spec.Replicas == nil {
		return 1
	}
	return *s.Spec.Replicas
}

// rolloutWarnings are the things about this particular object that will stop
// the rollout doing what the plan says it does.
//
// Both of them are StatefulSet-only and both are visible in the spec before
// anything is written, which is the whole reason to say them here: ktunnel
// patches the object and then waits for a rollout, and in these two cases it
// would wait out its whole deadline for a reason nothing on screen explains.
func (w *workload) rolloutWarnings() []string {
	if w.kind != KindStatefulSet || w.statefulSet == nil {
		return nil
	}
	strategy := w.statefulSet.Spec.UpdateStrategy
	var warnings []string
	if strategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		warnings = append(warnings, fmt.Sprintf(
			"%s uses the OnDelete update strategy, so writing the container into its template restarts nothing; "+
				"delete its pods yourself to pick up the sidecar (kubectl delete pod %s-0 -n %s)",
			w, w.name, w.namespace))
	}
	if strategy.Type != appsv1.OnDeleteStatefulSetStrategyType &&
		strategy.RollingUpdate != nil && strategy.RollingUpdate.Partition != nil && *strategy.RollingUpdate.Partition > 0 {
		partition := *strategy.RollingUpdate.Partition
		warnings = append(warnings, fmt.Sprintf(
			"%s has spec.updateStrategy.rollingUpdate.partition=%d, so only ordinals %d and above are updated; "+
				"the pods below it keep the old template, never get the sidecar, and the rollout never finishes",
			w, partition, partition))
	}
	return warnings
}

// podLabelSelector returns the selector that resolves a workload's own pods.
//
// This is the workload's spec.selector -- the labels it selects its pods by,
// whatever they are -- and not the two labels ktunnel puts on the deployments
// `expose` creates. Those exist only on ktunnel's own deployments, so
// selecting on them worked for `expose` and could not work for `inject` at
// all: the sidecar went in, the pod reported 2/2 Running, and the port-forward
// retried "found 0 running pod(s)" against an application workload labelled
// any other way, which is every application workload (#171, #115).
func podLabelSelector(w *workload) (string, error) {
	// An absent or empty spec.selector converts to "match everything", so
	// these two are refusals rather than a wildcard: forwarding to whichever
	// unrelated pod happened to sort first, and reporting it as success, is
	// worse than saying which object cannot be resolved.
	if w.selector == nil {
		return "", fmt.Errorf("%s has no spec.selector, so its pods cannot be identified", w)
	}
	selector, err := metav1.LabelSelectorAsSelector(w.selector)
	if err != nil {
		return "", fmt.Errorf("%s has a spec.selector that cannot be resolved: %w", w, err)
	}
	if selector.Empty() {
		return "", fmt.Errorf("%s has an empty spec.selector, which matches every pod in the namespace", w)
	}
	return selector.String(), nil
}

// sortPods puts the workload's running pods into the order their local ports
// are assigned in: pods[i] is reached on the i'th port counting up from
// --port.
//
// The two kinds want opposite orders, and both orders are load-bearing.
//
// A Deployment's pods are interchangeable, and its selector matches the pods
// of its old ReplicaSet and its new one at once -- injecting is itself a
// rollout, so for a moment two running pods answer to it and only the newer
// one carries a tunnel server. Newest first.
//
// A StatefulSet's pods are the opposite by definition: owl-app-0 is a
// different thing from owl-app-1, with its own volume and its own identity, so
// the local port a pod is reached on has to keep meaning that pod across
// reconnects -- and a reconnect re-resolves the pods from scratch. Creation
// time gives no such guarantee: a rolling update recreates the highest ordinal
// first, and a single rescheduled pod reorders the whole list. Ordinal does,
// so ordinal it is. The rollout window a Deployment has does not exist here
// anyway, because a StatefulSet deletes a pod before creating its replacement
// and two pods never share an ordinal.
func sortPods(kind WorkloadKind, pods []apiv1.Pod) {
	if kind == KindStatefulSet {
		sort.Sort(ByOrdinal(pods))
		return
	}
	sort.Sort(ByCreationTime(pods))
}

// ByOrdinal orders StatefulSet pods by the ordinal in their name, which is the
// identity Kubernetes gives them: NAME-0, NAME-1, NAME-2.
//
// A pod whose name does not end in a number sorts after the ones that do,
// ordered by name -- there is nothing better to do with it, and it is not a
// reason to fail: a StatefulSet's pod names are the controller's business, and
// guessing wrong about them should degrade to a stable arbitrary order rather
// than refuse to tunnel.
type ByOrdinal []apiv1.Pod

func (a ByOrdinal) Len() int      { return len(a) }
func (a ByOrdinal) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByOrdinal) Less(i, j int) bool {
	oi, iok := podOrdinal(a[i].Name)
	oj, jok := podOrdinal(a[j].Name)
	if iok != jok {
		return iok
	}
	if !iok {
		return a[i].Name < a[j].Name
	}
	if oi != oj {
		return oi < oj
	}
	return a[i].Name < a[j].Name
}

func podOrdinal(name string) (int, bool) {
	dash := strings.LastIndex(name, "-")
	if dash < 0 || dash == len(name)-1 {
		return 0, false
	}
	ordinal, err := strconv.Atoi(name[dash+1:])
	if err != nil {
		return 0, false
	}
	return ordinal, true
}

// statefulSetReadyDeadline backstops the rollout wait.
//
// A Deployment carries its own spec.progressDeadlineSeconds and the controller
// reports ProgressDeadlineExceeded itself; a StatefulSet has neither, so
// nothing but this stops the wait polling forever. The value is the
// Deployment default, for want of a better one -- a StatefulSet rolls its pods
// one at a time, so the deadline is more likely to be reached honestly here,
// but a wait that ends with a message beats one that never ends.
const statefulSetReadyDeadline = 600 * time.Second

// statefulSetStatus is deploymentStatus for a StatefulSet.
//
// Ready pods are not the same thing as pods carrying the new template, which
// is what the revision comparison is for: a StatefulSet mid-update has every
// pod ready and half of them still on the old spec, and forwarding to those
// reaches a pod with no tunnel server in it.
//
// The revision check is applied to OnDelete too, deliberately. Under OnDelete
// the pods never restart on their own, so currentRevision stays behind until
// the user deletes them -- and waiting is the correct thing to do, because the
// alternative is declaring success and forwarding to pods with no sidecar. The
// plan says beforehand that this is what will happen and what to do about it;
// see rolloutWarnings.
func statefulSetStatus(s *appsv1.StatefulSet) (string, bool, error) {
	if s.Status.ObservedGeneration < s.Generation {
		return "Waiting for statefulset spec update to be observed...\n", false, nil
	}
	want := statefulSetReplicaCount(s)
	if s.Status.ReadyReplicas < want {
		return fmt.Sprintf("Waiting for statefulset %q rollout to finish: %d of %d pods are ready...\n",
			s.Name, s.Status.ReadyReplicas, want), false, nil
	}
	if s.Status.UpdateRevision != s.Status.CurrentRevision {
		return fmt.Sprintf("Waiting for statefulset %q rollout to finish: %d of %d pods carry the new pod template...\n",
			s.Name, s.Status.UpdatedReplicas, want), false, nil
	}
	return fmt.Sprintf("statefulset %q successfully rolled out\n", s.Name), true, nil
}

// readWorkload re-reads a workload through the package-level clients, which is
// what the rollout wait polls -- it runs in a goroutine with no KubeService in
// hand, the same way watchForReady always has.
func readWorkload(kind WorkloadKind, namespace, name string) (*workload, error) {
	clientMutex.RLock()
	defer clientMutex.RUnlock()
	switch kind {
	case KindStatefulSet:
		s, err := statefulSetsClient.Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return newStatefulSetWorkload(s), nil
	default:
		d, err := deploymentsClient.Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return newDeploymentWorkload(d), nil
	}
}

// rolloutStatus is the controller's own account of whether the rollout this
// workload is on has finished.
func (w *workload) rolloutStatus() (string, bool, error) {
	if w.kind == KindStatefulSet {
		return statefulSetStatus(w.statefulSet)
	}
	return deploymentStatus(w.deployment)
}

// readyDeadline is how long the wait keeps polling before giving up.
func (w *workload) readyDeadline() time.Duration {
	if w.kind == KindStatefulSet {
		return statefulSetReadyDeadline
	}
	// spec.progressDeadlineSeconds defaults to 600.
	progressDeadlineSeconds := int64(600)
	if w.deployment.Spec.ProgressDeadlineSeconds != nil {
		progressDeadlineSeconds = int64(*w.deployment.Spec.ProgressDeadlineSeconds)
	}
	// Five seconds past the deadline Kubernetes enforces itself, so this is
	// only a backstop for the cases where it never will -- the object being
	// deleted out from under us, for instance.
	return time.Duration(progressDeadlineSeconds+5) * time.Second
}

// watchWorkloadReady reports on readyChan whether the workload finished
// rolling out.
//
// This polls rather than using a watch. A watch only delivers events that occur
// after it is established, so a workload that finished rolling out before we
// got here produced no event at all and the caller blocked until the progress
// deadline expired -- the "waiting for deployment to be ready" hang. Polling
// reads the current state first, so an already-complete rollout is seen
// immediately. Reading by name also avoids the previous label-selector watch,
// which matched on whatever labels the caller happened to pass in.
func watchWorkloadReady(w *workload, readyChan chan<- bool) {
	go func() {
		lastMsg := ""
		timeout := w.readyDeadline()

		if w.kind == KindDeployment {
			if rolling := w.deployment.Spec.Strategy.RollingUpdate; rolling != nil && rolling.MaxUnavailable != nil {
				if maxUnavailable := rolling.MaxUnavailable.IntValue(); maxUnavailable > 0 {
					log.Warnf("RollingUpdate.MaxUnavailable: %v. This may prevent deployment failures from being detected. Set to 0 to ensure ProgressDeadlineInSeconds is enforced.", maxUnavailable)
				}
			}
			log.Infof("ProgressDeadlineInSeconds is currently %vs. It may take this long to detect a deployment failure.", int64(timeout.Seconds())-5)
		}
		// Repeated here, having already been in the plan: this is the point
		// at which the user is watching a wait that is not going to end on
		// its own, and scrolling back up to the plan is not the answer.
		for _, warning := range w.rolloutWarnings() {
			log.Warn(warning)
		}

		deadline := time.Now().Add(timeout)
		for {
			cur, err := readWorkload(w.kind, w.namespace, w.name)
			if err != nil {
				log.WithError(err).Errorf("failed reading %s while waiting for it to be ready", w)
				readyChan <- false
				return
			}

			msg, ready, err := cur.rolloutStatus()
			if err != nil {
				log.Error(err)
				readyChan <- false
				return
			}

			if msg != lastMsg {
				log.Info(msg)
				lastMsg = msg
			}

			if ready {
				readyChan <- true
				return
			}

			if time.Now().After(deadline) {
				log.Errorf("timed out after %vs waiting for %s to be ready", int64(timeout.Seconds()), w)
				readyChan <- false
				return
			}

			time.Sleep(readyPollInterval)
		}
	}()
}
