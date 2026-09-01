// Package k8s provides Kubernetes integration functionality for ktunnel
package k8s

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	_ "k8s.io/client-go/plugin/pkg/client/auth/exec"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp" // https://github.com/kubernetes/client-go/issues/242
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const (
	Image                   = "docker.io/omrieival/ktunnel"
	kubeConfigEnvVar        = "KUBECONFIG"
	deploymentNameLabel     = "app.kubernetes.io/name"
	deploymentInstanceLabel = "app.kubernetes.io/instance"
)

type ByCreationTime []apiv1.Pod

type KubeService struct {
	clients *Clients
	config  *rest.Config
}

func NewKubeService(kubeCtx, namespace string) (*KubeService, error) {
	cfg := GetKubeConfig(kubeCtx)

	return &KubeService{
		clients: GetClients(cfg, namespace),
		config:  cfg,
	}, nil
}

func GetClients(cfg *rest.Config, namespace string) *Clients {
	clientSet, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Errorf("Failed to get k8s client: %v", err)
		os.Exit(1)
	}

	deploymentsClient = clientSet.AppsV1().Deployments(namespace)
	statefulSetsClient = clientSet.AppsV1().StatefulSets(namespace)
	podsClient = clientSet.CoreV1().Pods(namespace)
	svcClient = clientSet.CoreV1().Services(namespace)

	return &Clients{
		Deployments:  deploymentsClient,
		StatefulSets: statefulSetsClient,
		Pods:         podsClient,
		Services:     svcClient,
		Secrets:      clientSet.CoreV1().Secrets(namespace),
	}
}

func (a ByCreationTime) Len() int { return len(a) }
func (a ByCreationTime) Less(i, j int) bool {
	return a[i].CreationTimestamp.After(a[j].CreationTimestamp.Time)
}
func (a ByCreationTime) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

var (
	configMutex  sync.RWMutex
	kubeconfig   *rest.Config
	verboseMutex sync.RWMutex
	verbose      = false
)

// SetVerbose sets the verbose flag in a thread-safe way
func SetVerbose(v bool) {
	verboseMutex.Lock()
	defer verboseMutex.Unlock()
	verbose = v
}

// IsVerbose gets the verbose flag in a thread-safe way
func IsVerbose() bool {
	verboseMutex.RLock()
	defer verboseMutex.RUnlock()
	return verbose
}

func GetKubeConfig(kubeCtx string) *rest.Config {
	configMutex.RLock()
	if kubeconfig != nil {
		defer configMutex.RUnlock()
		return kubeconfig
	}
	configMutex.RUnlock()

	configMutex.Lock()
	defer configMutex.Unlock()

	// Double-check after acquiring write lock
	if kubeconfig != nil {
		return kubeconfig
	}

	config, err := kubeClientConfig(kubeCtx).ClientConfig()
	if err != nil {
		log.Errorf("Failed getting kubernetes config: %v", err)
	}
	kubeconfig = config
	return kubeconfig
}

func (k *KubeService) getPodsFilteredByLabel(ctx context.Context, labelSelector string) (*apiv1.PodList, error) {
	pods, err := k.clients.Pods.List(
		ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		},
	)
	if err != nil {
		return nil, err
	}
	return pods, nil
}

func hasSidecar(podSpec apiv1.PodSpec, image string) bool {
	for _, c := range podSpec.Containers {
		if c.Image == image {
			return true
		}
	}
	return false
}

func newContainer(port int, image string, containerPorts []apiv1.ContainerPort, podCreds PodCredentials, cReq, cLimit, mReq, mLimit int64) *apiv1.Container {
	args := []string{"server", "-p", strconv.FormatInt(int64(port), 10)}
	if IsVerbose() {
		args = append(args, "-v")
	}
	args = append(args, podCreds.args()...)
	// Constructed rather than zero-valued: a resource.Quantity built as
	// resource.Quantity{} has the empty Format, which serialises as
	// DecimalExponent -- `500e-3` for half a core and `100e6` for 100MB.
	// Those are correct and nothing else in a cluster writes them that way,
	// so they read as a bug, and they cannot be compared by eye against a
	// LimitRange or the deployment next to them (#118).
	cpuRequest := resource.NewMilliQuantity(cReq, resource.DecimalSI)
	cpuLimit := resource.NewMilliQuantity(cLimit, resource.DecimalSI)
	memRequest := resource.NewScaledQuantity(mReq, resource.Mega)
	memLimit := resource.NewScaledQuantity(mLimit, resource.Mega)
	return &apiv1.Container{
		Name:         "ktunnel",
		Image:        image,
		Command:      []string{"/ktunnel/ktunnel"},
		Args:         args,
		Ports:        containerPorts,
		Env:          podCreds.env(),
		VolumeMounts: podCreds.volumeMounts(),
		Resources: apiv1.ResourceRequirements{
			Requests: apiv1.ResourceList{
				"cpu":    *cpuRequest,
				"memory": *memRequest,
			},
			Limits: apiv1.ResourceList{
				"cpu":    *cpuLimit,
				"memory": *memLimit,
			},
		},
		// No RunAsUser, and no RunAsGroup. OpenShift assigns a UID from a
		// per-namespace range and rejects a pod that demands its own, which
		// is why `expose` did not work there at all (#87). The non-root
		// property that hardcoded 1000 was protecting now comes from the
		// image, which carries USER 1000: a vanilla cluster runs as 1000
		// exactly as before, and OpenShift overrides it, which is what it
		// wants to do.
		//
		// Dropping every capability and refusing privilege escalation are
		// both required by OpenShift's restricted-v2 SCC and cost nothing
		// anywhere else -- the tunnel server opens sockets and execs
		// nothing.
		//
		// Nothing is added back. NET_BIND_SERVICE is the obvious grant for
		// binding ports below 1024, and it is inert here: a non-root process
		// with no file capabilities gets an empty effective set on exec, so
		// the capability never reaches the permitted set and the bind still
		// fails. Measured, not assumed. Privileged ports are handled by a
		// pod-level sysctl instead -- see podSecurityContext.
		SecurityContext: &apiv1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities: &apiv1.Capabilities{
				Drop: []apiv1.Capability{"ALL"},
			},
		},
	}
}

func newDeployment(
	namespace, name string,
	port int,
	image string,
	ports []apiv1.ContainerPort,
	selector map[string]string,
	deploymentLabels map[string]string,
	deploymentAnnotations map[string]string,
	podTolerations []apiv1.Toleration,
	podCreds PodCredentials,
	cpuReq, cpuLimit, memReq, memLimit int64,
) *appsv1.Deployment {
	replicas := int32(1)
	deploymentLabels[deploymentNameLabel] = name
	deploymentLabels[deploymentInstanceLabel] = name
	co := newContainer(port, image, ports, podCreds, cpuReq, cpuLimit, memReq, memLimit)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      deploymentLabels,
			Annotations: deploymentAnnotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: deploymentLabels,
			},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      deploymentLabels,
					Annotations: deploymentAnnotations,
				},
				Spec: apiv1.PodSpec{
					NodeSelector: selector,
					Containers: []apiv1.Container{
						*co,
					},
					Tolerations: podTolerations,
					Volumes:     podCreds.volumes(),
					// Pod-level, because a sysctl has nowhere else to go.
					// Nil unless some port actually needs it, so an
					// ordinary deployment carries no securityContext at
					// all and looks exactly as it did before (#164).
					SecurityContext: newPodSecurityContext(ports),
				},
			},
		},
	}
}

func newService(namespace, name string, ports []apiv1.ServicePort, serviceType apiv1.ServiceType, annotations map[string]string) *apiv1.Service {
	return &apiv1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind: "Service",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":     name,
				"app.kubernetes.io/instance": name,
			},
			Annotations: annotations,
		},
		Spec: apiv1.ServiceSpec{
			Ports: ports,
			Type:  serviceType,
			Selector: map[string]string{
				"app.kubernetes.io/name":     name,
				"app.kubernetes.io/instance": name,
			},
		},
	}
}

// replicaCount is the number of pods a deployment wants.
//
// spec.replicas is a pointer that the API server defaults to 1 when it is
// unset, so nil means one pod rather than none. It was dereferenced in two
// places without a check, which is a panic on an object built by anything that
// does not apply that default.
func replicaCount(deployment *appsv1.Deployment) int32 {
	if deployment.Spec.Replicas == nil {
		return 1
	}
	return *deployment.Spec.Replicas
}

func (k *KubeService) getPodNames(ctx context.Context, w *workload, pods []string) error {
	labelSelector, err := podLabelSelector(w)
	if err != nil {
		return err
	}
	filteredPods, err := k.getPodsFilteredByLabel(ctx, labelSelector)
	if err != nil {
		return apiError("list the pods of", string(w.kind), w.namespace, w.name, err)
	}
	// Every running pod is collected and the first len(pods) of them taken
	// below, in the order sortPods puts them in -- which differs by kind, and
	// why it differs is on sortPods. The counter that used to guard this loop
	// was never incremented, so its early exit and the "All pods located" line
	// it logged could only fire when there were no pods to locate at all.
	//
	// A pod that is being deleted stays Phase Running for the whole of its
	// grace period, and is the newest match until its replacement exists, so
	// it is skipped outright -- a forward built to it dies with it.
	matchingPods := []apiv1.Pod{}
	for _, p := range filteredPods.Items {
		if p.Status.Phase == apiv1.PodRunning && p.DeletionTimestamp == nil {
			matchingPods = append(matchingPods, p)
		}
	}
	sortPods(w.kind, matchingPods)
	if len(matchingPods) < len(pods) {
		// Indexing past the end used to panic here, taking the whole process
		// down. That is not a hypothetical: it is the gap between a tunnel
		// server pod being deleted and its replacement reaching Running --
		// exactly the case reconnecting exists to survive. Reported as an
		// error, it is one failed attempt that the supervisor backs off and
		// retries until the new pod is up.
		return fmt.Errorf("found %d running pod(s) for %s, want %d; "+
			"the rollout may still be coming up, or may have failed -- kubectl get pods -n %s -l %s",
			len(matchingPods), w, len(pods), w.namespace, labelSelector)
	}
	for i := 0; i < len(pods); i++ {
		pods[i] = matchingPods[i].Name
	}

	return nil
}

// PortForward forwards targetPort on every pod of the workload to a local
// port, and returns those local ports once the forwards are up.
//
// Pods are ordered by sortPods, so which pod a given local port reaches is a
// property of the workload rather than of the order the API server answered
// in -- which matters for a StatefulSet, whose pods are not interchangeable.
//
// ctx bounds the calls to the API server that resolve the workload and its
// pods. It does not bound the SPDY dial inside the forwarder itself, which
// client-go gives no way to cancel; see forwardHandle in cmd for how a caller
// keeps that from wedging a retry loop.
//
// The returned channel carries failures that happen after startup. A forward
// that dies takes the tunnel above it with it, and the only way a caller can
// learn about that is to be told: this used to be an unbuffered channel read
// exactly once, during startup, so a later failure blocked its forwarder
// forever and the tunnel just went quiet.
//
// It is buffered for every forwarder, so reading it is optional and a caller
// that walks away cannot wedge a forwarder that outlives it. It closes once
// every forwarder has returned, which happens when stopChan is closed. It is
// returned alongside a non-nil error too, because forwarders already launched
// hold local ports whether or not startup succeeded. See watchForward in cmd
// for why a caller about to retry has to wait for that close.
func (k *KubeService) PortForward(ctx context.Context, kind WorkloadKind, namespace, name string, targetPort string, stopChan <-chan struct{}) ([]string, <-chan error, error) {
	w, err := k.getWorkload(ctx, kind, namespace, name)
	if err != nil {
		return nil, nil, err
	}

	podNames := make([]string, w.replicas)
	err = k.getPodNames(ctx, w, podNames)
	if err != nil {
		return nil, nil, err
	}
	log.Debugf("Injecting to this pods: %v", podNames)
	sourcePorts := make([]string, len(podNames))
	numPort, err := strconv.ParseInt(targetPort, 10, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("the tunnel port %q is not a number; --port takes a port number: %w", targetPort, err)
	}
	for i := 0; i < len(sourcePorts); i++ {
		sourcePorts[i] = strconv.FormatInt(numPort+int64(i), 10)
	}

	// ready counts down as each forward reports itself up. It used to be
	// supplied by the caller, which meant a counter that outlived one call
	// and an Add that ran before the error returns above could leave it
	// permanently non-zero. Nothing outside this function ever read it.
	var ready sync.WaitGroup
	ready.Add(len(podNames))

	forwarderErrChan := make(chan error, len(podNames))
	var forwarders sync.WaitGroup
	forwarders.Add(len(podNames))
	go func() {
		forwarders.Wait()
		close(forwarderErrChan)
	}()
	for i, podName := range podNames {
		readyChan := make(chan struct{}, 1)
		ports := []string{fmt.Sprintf("%s:%s", sourcePorts[i], targetPort)}
		serverURL := getPortForwardURL(k.config, namespace, podName)

		transport, upgrader, err := spdy.RoundTripperFor(k.config)
		if err != nil {
			// Release the forwarders that will now never be launched, so
			// the goroutine waiting to close forwarderErrChan is not left
			// waiting on them.
			for j := i; j < len(podNames); j++ {
				forwarders.Done()
			}
			return nil, forwarderErrChan, fmt.Errorf("failed building a port forward to pod %s/%s: %w", namespace, podName, err)
		}
		log.Infof("port forwarding to %s", serverURL)
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, serverURL)

		out, errOut := new(bytes.Buffer), new(bytes.Buffer)
		forwarder, err := portforward.New(dialer, ports, stopChan, readyChan, out, errOut)
		if err != nil {
			// Returned rather than logged: the forwarder is nil on this
			// path, and the goroutine below would dereference it. Nothing
			// reaches it today, but it is the same shape as the two nil
			// dereferences this loop has already had to be fixed for, and
			// it now sits inside a retry loop where a panic recurs.
			for j := i; j < len(podNames); j++ {
				forwarders.Done()
			}
			return nil, forwarderErrChan, fmt.Errorf("failed setting up the port forward from local port %s to pod %s/%s: %w",
				sourcePorts[i], namespace, podName, err)
		}

		go func() {
			// This forwarder stops being pending exactly once, whatever
			// happens to it. client-go closes readyChan only after a dial
			// *and* a listen have both succeeded, so the two most common
			// failures on the retry path -- an API server that cannot be
			// reached, a local port the previous attempt has not released
			// -- never close it. Waiting on it alone parked this goroutine
			// for good, and the one waiting on `ready` below with it: two
			// per failed attempt, retained for the life of the process, on
			// precisely the path reconnecting exercises.
			//
			// stopChan is the escape, and the caller always closes it --
			// release() does, on every path an attempt can end by.
			defer ready.Done()

			select {
			case <-readyChan: // closed once Kubernetes has something to tell us
			case <-stopChan:
				// Torn down before it ever became ready. Nothing is coming.
				return
			}

			if len(errOut.String()) != 0 {
				log.Errorf("Failed forwarding. %s", errOut.String())
			} else if len(out.String()) != 0 {
				log.Info(out.String())
			}
		}()
		localPort := sourcePorts[i]
		go func() {
			defer forwarders.Done()
			// err is declared here rather than assigned to the function's
			// own, which every forwarder would otherwise have written to.
			if err := forwarder.ForwardPorts(); err != nil { // Locks until stopChan is closed.
				forwarderErrChan <- forwardError(namespace, podName, localPort, err)
			}
		}()
	}

	log.Info("Waiting for port forward to finish")

	doneCh := make(chan struct{})
	go func() {
		ready.Wait()
		close(doneCh)
	}()

	select {
	case <-ctx.Done():
		// Neither of the cases below is guaranteed to arrive. doneCh needs
		// every forward to report itself ready, which cannot happen until
		// its SPDY dial completes, and that dial is not cancellable by any
		// means client-go exposes -- so a forward opening against an
		// unresponsive API server parks this select indefinitely. The
		// caller's release timeout does not cover it, because the caller is
		// still inside this call and has not registered it yet. Handing back
		// the error channel lets it release on the path that already works.
		return nil, forwarderErrChan, ctx.Err()
	case err, ok := <-forwarderErrChan:
		if ok {
			return nil, forwarderErrChan, err
		}
		// The channel is closed, meaning every forwarder has already
		// returned. With no pods to forward to there were none to start
		// with, and both cases of this select are ready at once -- so half
		// the time this branch is taken and a receive from a closed channel
		// is reported as a nil error. The ports are returned with it, and
		// they are empty, which is what the caller checks for.
		//
		// It used to return them as a *[]string, and this path returned a
		// nil one: callers dereferenced it to check for emptiness and
		// panicked instead, inside a retry loop, on roughly half the
		// attempts. A plain slice cannot express the difference between
		// "none" and "not there", so the panic is now unrepresentable
		// rather than guarded against.
		return sourcePorts, forwarderErrChan, nil
	case <-doneCh:
		return sourcePorts, forwarderErrChan, nil
	}
}

func getPortForwardURL(config *rest.Config, namespace string, podName string) *url.URL {
	host := config.Host
	scheme := "https"
	if strings.HasPrefix(config.Host, "https://") {
		host = strings.TrimPrefix(config.Host, "https://")
	} else if strings.HasPrefix(config.Host, "http://") {
		host = strings.TrimPrefix(config.Host, "http://")
		scheme = "http"
	}
	trailingHostPath := strings.Split(host, "/")
	hostIP := trailingHostPath[0]
	trailingPath := ""
	if len(trailingHostPath) > 1 && trailingHostPath[1] != "" {
		trailingPath = fmt.Sprintf("/%s/", strings.Join(trailingHostPath[1:], "/"))
	}
	path := fmt.Sprintf("%sapi/v1/namespaces/%s/pods/%s/portforward", trailingPath, namespace, podName)
	return &url.URL{
		Scheme: scheme,
		Path:   path,
		Host:   hostIP,
	}
}

// readyPollInterval is how often the rollout wait re-reads the object it is
// waiting on.
const readyPollInterval = time.Second

// watchForReady reports on readyChan whether the deployment finished rolling
// out. The polling, and why it polls, is in watchWorkloadReady.
func watchForReady(deployment *appsv1.Deployment, readyChan chan<- bool) {
	watchWorkloadReady(newDeploymentWorkload(deployment), readyChan)
}

func deploymentStatus(deployment *appsv1.Deployment) (string, bool, error) {
	if deployment.Generation <= deployment.Status.ObservedGeneration {
		cond := getDeploymentCondition(deployment.Status, appsv1.DeploymentProgressing)
		if cond != nil && cond.Reason == "ProgressDeadlineExceeded" {
			return "", false, fmt.Errorf("deployment %q exceeded its progress deadline", deployment.Name)
		}

		if deployment.Spec.Replicas != nil && deployment.Status.UpdatedReplicas < *deployment.Spec.Replicas {
			return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d out of %d new replicas have been updated...\n", deployment.Name, deployment.Status.UpdatedReplicas, *deployment.Spec.Replicas), false, nil
		}

		if deployment.Status.Replicas > deployment.Status.UpdatedReplicas {
			return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d old replicas are pending termination...\n", deployment.Name, deployment.Status.Replicas-deployment.Status.UpdatedReplicas), false, nil
		}

		if deployment.Status.AvailableReplicas < deployment.Status.UpdatedReplicas {
			return fmt.Sprintf("Waiting for deployment %q rollout to finish: %d of %d updated replicas are available...\n", deployment.Name, deployment.Status.AvailableReplicas, deployment.Status.UpdatedReplicas), false, nil
		}

		return fmt.Sprintf("deployment %q successfully rolled out\n", deployment.Name), true, nil
	}
	return "Waiting for deployment spec update to be observed...\n", false, nil
}

func getDeploymentCondition(status appsv1.DeploymentStatus, condType appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range status.Conditions {
		c := status.Conditions[i]
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

// boolPtr is the usual dance for an optional bool in a Kubernetes spec, where
// false and unset mean different things.
func boolPtr(b bool) *bool { return &b }

// newPodSecurityContext returns the pod securityContext, or nil when there is
// nothing to put in it. Returning nil rather than an empty struct keeps the
// rendered manifests unchanged for the runs that need nothing.
func newPodSecurityContext(ports []apiv1.ContainerPort) *apiv1.PodSecurityContext {
	numbers := make([]int, 0, len(ports))
	for _, p := range ports {
		numbers = append(numbers, int(p.ContainerPort))
	}
	sysctls := podSysctls(numbers)
	if len(sysctls) == 0 {
		return nil
	}
	return &apiv1.PodSecurityContext{Sysctls: sysctls}
}

// privilegedPortCeiling is the first unprivileged port under the kernel's
// default. A bind below it needs help; 1024 itself does not.
const privilegedPortCeiling = 1024

// podSysctls returns the pod-level sysctls the given in-cluster ports need.
//
// Binding below 1024 as a non-root process requires
// net.ipv4.ip_unprivileged_port_start to be lowered (#164). The obvious
// alternative, granting NET_BIND_SERVICE, does nothing here: a non-root
// process with no file capabilities gets an empty effective set on exec, so
// the capability never reaches the permitted set and the bind still fails.
// That was measured on a cluster, not inferred.
//
// Kubernetes classifies this sysctl as safe (1.22+), so no cluster
// configuration is needed. An SCC can still list it in forbiddenSysctls, so a
// bind failure has to name the sysctl and not only --port.
//
// It is set only when some port actually needs it. Every other run keeps the
// cluster's default, which is one less thing for a policy reviewer to ask
// about.
func podSysctls(ports []int) []apiv1.Sysctl {
	for _, p := range ports {
		if p < privilegedPortCeiling {
			return []apiv1.Sysctl{{
				Name:  "net.ipv4.ip_unprivileged_port_start",
				Value: "0",
			}}
		}
	}
	return nil
}
