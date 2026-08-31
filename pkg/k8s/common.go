// Package k8s provides Kubernetes integration functionality for ktunnel
package k8s

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
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
	podsClient = clientSet.CoreV1().Pods(namespace)
	svcClient = clientSet.CoreV1().Services(namespace)

	return &Clients{
		Deployments: deploymentsClient,
		Pods:        podsClient,
		Services:    svcClient,
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

func newContainer(port int, image string, containerPorts []apiv1.ContainerPort, cert, key string, cReq, cLimit, mReq, mLimit int64) *apiv1.Container {
	args := []string{"server", "-p", strconv.FormatInt(int64(port), 10)}
	if IsVerbose() {
		args = append(args, "-v")
	}
	if cert != "" {
		args = append(args, fmt.Sprintf("--cert %s", cert))
	}
	if key != "" {
		args = append(args, fmt.Sprintf("--key %s", key))
	}
	cpuRequest, cpuLimit, memRequest, memLimit := resource.Quantity{}, resource.Quantity{}, resource.Quantity{}, resource.Quantity{}
	cpuRequest.SetMilli(cReq)
	cpuLimit.SetMilli(cLimit)
	memRequest.SetScaled(mReq, resource.Mega)
	memLimit.SetScaled(mLimit, resource.Mega)
	containerUID := int64(1000)

	return &apiv1.Container{
		Name:    "ktunnel",
		Image:   image,
		Command: []string{"/ktunnel/ktunnel"},
		Args:    args,
		Ports:   containerPorts,
		Resources: apiv1.ResourceRequirements{
			Requests: apiv1.ResourceList{
				"cpu":    cpuRequest,
				"memory": memRequest,
			},
			Limits: apiv1.ResourceList{
				"cpu":    cpuLimit,
				"memory": memLimit,
			},
		},
		SecurityContext: &apiv1.SecurityContext{
			RunAsUser: &containerUID,
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
	cert, key string,
	cpuReq, cpuLimit, memReq, memLimit int64,
) *appsv1.Deployment {
	replicas := int32(1)
	deploymentLabels[deploymentNameLabel] = name
	deploymentLabels[deploymentInstanceLabel] = name
	co := newContainer(port, image, ports, cert, key, cpuReq, cpuLimit, memReq, memLimit)

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
				},
			},
		},
	}
}

func newService(namespace, name string, ports []apiv1.ServicePort, serviceType apiv1.ServiceType) *apiv1.Service {
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

// podLabelSelector returns the selector that resolves a deployment's own pods.
//
// This is the deployment's spec.selector -- the labels it selects its pods by,
// whatever they are -- and not the two labels ktunnel puts on the deployments
// `expose` creates. Those exist only on ktunnel's own deployments, so
// selecting on them worked for `expose` and could not work for `inject` at
// all: the sidecar went in, the pod reported 2/2 Running, and the port-forward
// retried "found 0 running pod(s)" against an application deployment labelled
// any other way, which is every application deployment (#171, #115).
func podLabelSelector(deployment *appsv1.Deployment) (string, error) {
	// An absent or empty spec.selector converts to "match everything", so
	// these two are refusals rather than a wildcard: forwarding to whichever
	// unrelated pod happened to sort first, and reporting it as success, is
	// worse than saying which object cannot be resolved.
	if deployment.Spec.Selector == nil {
		return "", fmt.Errorf("deployment %s has no spec.selector, so its pods cannot be identified", deployment.Name)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return "", fmt.Errorf("deployment %s has a spec.selector that cannot be resolved: %w", deployment.Name, err)
	}
	if selector.Empty() {
		return "", fmt.Errorf("deployment %s has an empty spec.selector, which matches every pod in the namespace", deployment.Name)
	}
	return selector.String(), nil
}

func (k *KubeService) getPodNames(ctx context.Context, deployment *appsv1.Deployment, pods []string) error {
	labelSelector, err := podLabelSelector(deployment)
	if err != nil {
		return err
	}
	filteredPods, err := k.getPodsFilteredByLabel(ctx, labelSelector)
	if err != nil {
		return apiError("list the pods of", "deployment", deployment.Namespace, deployment.Name, err)
	}
	// Every running pod is collected and the newest len(pods) of them taken
	// below. The counter that used to guard this loop was never incremented,
	// so its early exit and the "All pods located" line it logged could only
	// fire when there were no pods to locate at all.
	//
	// Newest first matters because a deployment's selector matches the pods of
	// its old ReplicaSet and its new one at once, and injecting the sidecar is
	// itself a rollout: for a moment two running pods answer to the selector
	// and only the newer one has a tunnel server in it.
	//
	// A pod that is being deleted stays Phase Running for the whole of its
	// grace period, and is the newest match until its replacement exists, so
	// it is skipped outright -- a forward built to it dies with it.
	matchingPods := ByCreationTime{}
	for _, p := range filteredPods.Items {
		if p.Status.Phase == apiv1.PodRunning && p.DeletionTimestamp == nil {
			matchingPods = append(matchingPods, p)
		}
	}
	sort.Sort(matchingPods)
	if len(matchingPods) < len(pods) {
		// Indexing past the end used to panic here, taking the whole process
		// down. That is not a hypothetical: it is the gap between a tunnel
		// server pod being deleted and its replacement reaching Running --
		// exactly the case reconnecting exists to survive. Reported as an
		// error, it is one failed attempt that the supervisor backs off and
		// retries until the new pod is up.
		return fmt.Errorf("found %d running pod(s) for deployment %s/%s, want %d; "+
			"the rollout may still be coming up, or may have failed -- kubectl get pods -n %s -l %s",
			len(matchingPods), deployment.Namespace, deployment.Name, len(pods), deployment.Namespace, labelSelector)
	}
	for i := 0; i < len(pods); i++ {
		pods[i] = matchingPods[i].Name
	}

	return nil
}

// PortForward forwards targetPort on every pod of the deployment to a local
// port, and returns those local ports once the forwards are up.
//
// ctx bounds the calls to the API server that resolve the deployment and its
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
func (k *KubeService) PortForward(ctx context.Context, namespace, deploymentName string, targetPort string, stopChan <-chan struct{}) ([]string, <-chan error, error) {
	clientMutex.RLock()
	deployment, err := deploymentsClient.Get(ctx, deploymentName, metav1.GetOptions{})
	clientMutex.RUnlock()
	if err != nil {
		return nil, nil, apiError("read", "deployment", namespace, deploymentName, err)
	}

	podNames := make([]string, replicaCount(deployment))
	err = k.getPodNames(ctx, deployment, podNames)
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

// readyPollInterval is how often watchForReady re-reads the deployment while
// waiting for its rollout to finish.
const readyPollInterval = time.Second

// watchForReady reports on readyChan whether the deployment finished rolling
// out.
//
// This polls rather than using a watch. A watch only delivers events that occur
// after it is established, so a deployment that finished rolling out before we
// got here produced no event at all and the caller blocked until the progress
// deadline expired -- the "waiting for deployment to be ready" hang. Polling
// reads the current state first, so an already-complete rollout is seen
// immediately. Reading by name also avoids the previous label-selector watch,
// which matched on whatever labels the caller happened to pass in.
func watchForReady(deployment *appsv1.Deployment, readyChan chan<- bool) {
	go func() {
		name := deployment.Name
		lastMsg := ""

		if deployment.Spec.Strategy.RollingUpdate != nil &&
			deployment.Spec.Strategy.RollingUpdate.MaxUnavailable != nil {
			maxUnavailable := deployment.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue()
			if maxUnavailable > 0 {
				log.Warnf("RollingUpdate.MaxUnavailable: %v. This may prevent deployment failures from being detected. Set to 0 to ensure ProgressDeadlineInSeconds is enforced.", maxUnavailable)
			}
		}

		//spec.progressDeadlineSeconds defaults to 600
		progressDeadlineSeconds := int64(600)
		if deployment.Spec.ProgressDeadlineSeconds != nil {
			progressDeadlineSeconds = int64(*deployment.Spec.ProgressDeadlineSeconds)
		}

		log.Infof("ProgressDeadlineInSeconds is currently %vs. It may take this long to detect a deployment failure.", progressDeadlineSeconds)

		// Kubernetes reports ProgressDeadlineExceeded itself, which
		// deploymentStatus turns into an error. This deadline is only a
		// backstop for the cases where it never will -- the deployment being
		// deleted out from under us, for instance.
		deadline := time.Now().Add(time.Duration(progressDeadlineSeconds+5) * time.Second)

		for {
			clientMutex.RLock()
			d, err := deploymentsClient.Get(context.Background(), name, metav1.GetOptions{})
			clientMutex.RUnlock()
			if err != nil {
				log.WithError(err).Errorf("failed reading deployment %q while waiting for it to be ready", name)
				readyChan <- false
				return
			}

			msg, ready, err := deploymentStatus(d)
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
				log.Errorf("timed out after %vs waiting for deployment %q to be ready", progressDeadlineSeconds+5, name)
				readyChan <- false
				return
			}

			time.Sleep(readyPollInterval)
		}
	}()
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
