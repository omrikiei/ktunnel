// Package k8s provides Kubernetes integration functionality for ktunnel
package k8s

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	_ "k8s.io/client-go/plugin/pkg/client/auth/exec"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp" // https://github.com/kubernetes/client-go/issues/242
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/homedir"
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

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	kConfig := os.Getenv(kubeConfigEnvVar)
	if home := homedir.HomeDir(); kConfig == "" && home != "" {
		kConfig = filepath.Join(home, ".kube", "config")
		loadingRules.ExplicitPath = kConfig
	}

	var configOverrides *clientcmd.ConfigOverrides
	if (kubeCtx) != "" {
		configOverrides = &clientcmd.ConfigOverrides{CurrentContext: kubeCtx}
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides).ClientConfig()
	if err != nil {
		log.Errorf("Failed getting kubernetes config: %v", err)
	}
	kubeconfig = config
	return kubeconfig
}

func getClients(namespace *string, kubeCtx string) {
	clientMutex.Lock()
	defer clientMutex.Unlock()

	// Check if clients are already initialized
	if deploymentsClient != nil && podsClient != nil && svcClient != nil {
		return
	}

	clientSet, err := kubernetes.NewForConfig(GetKubeConfig(kubeCtx))
	if err != nil {
		log.Errorf("Failed to get k8s client: %v", err)
		os.Exit(1)
	}

	deploymentsClient = clientSet.AppsV1().Deployments(*namespace)
	podsClient = clientSet.CoreV1().Pods(*namespace)
	svcClient = clientSet.CoreV1().Services(*namespace)
}

func (k *KubeService) getPodsFilteredByLabel(labelSelector string) (*apiv1.PodList, error) {
	pods, err := k.clients.Pods.List(
		context.Background(), metav1.ListOptions{
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

func (k *KubeService) getPodNames(deploymentName string, pods []string) error {
	labelSelector := deploymentNameLabel + "=" + deploymentName + "," + deploymentInstanceLabel + "=" + deploymentName
	filteredPods, err := k.getPodsFilteredByLabel(labelSelector)
	if err != nil {
		return err
	}
	matchingPods := ByCreationTime{}
	pIndex := 0
	for _, p := range filteredPods.Items {
		if pIndex >= len(pods) {
			log.Info("All pods located for port-forwarding")
			break
		}
		if p.Status.Phase == apiv1.PodRunning {
			matchingPods = append(matchingPods, p)
		}
	}
	sort.Sort(matchingPods)
	for i := 0; i < len(pods); i++ {
		pods[i] = matchingPods[i].Name
	}

	return nil
}

func (k *KubeService) PortForward(namespace, deploymentName string, targetPort string, fwdWaitGroup *sync.WaitGroup, stopChan <-chan struct{}) (*[]string, error) {
	clientMutex.RLock()
	deployment, err := deploymentsClient.Get(context.Background(), deploymentName, metav1.GetOptions{})
	clientMutex.RUnlock()
	if err != nil {
		return nil, err
	}

	podNames := make([]string, *deployment.Spec.Replicas)
	err = k.getPodNames(deploymentName, podNames)
	fwdWaitGroup.Add(int(*deployment.Spec.Replicas))

	if err != nil {
		return nil, err
	}
	log.Debugf("Injecting to this pods: %v", podNames)
	sourcePorts := make([]string, *deployment.Spec.Replicas)
	numPort, err := strconv.ParseInt(targetPort, 10, 32)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(sourcePorts); i++ {
		sourcePorts[i] = strconv.FormatInt(numPort+int64(i), 10)
	}

	forwarderErrChan := make(chan error)
	for i, podName := range podNames {
		readyChan := make(chan struct{}, 1)
		ports := []string{fmt.Sprintf("%s:%s", sourcePorts[i], targetPort)}
		serverURL := getPortForwardURL(k.config, namespace, podName)

		transport, upgrader, err := spdy.RoundTripperFor(k.config)
		if err != nil {
			return nil, err
		}
		log.Infof("port forwarding to %s", serverURL)
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, serverURL)

		out, errOut := new(bytes.Buffer), new(bytes.Buffer)
		forwarder, err := portforward.New(dialer, ports, stopChan, readyChan, out, errOut)
		if err != nil {
			log.Error(err)
		}

		go func() {
			for range readyChan { // Kubernetes will close this channel when it has something to tell us.
			}
			if len(errOut.String()) != 0 {
				log.Errorf("Failed forwarding. %s", errOut.String())
				fwdWaitGroup.Done()
			} else if len(out.String()) != 0 {
				log.Info(out.String())
				if strings.HasPrefix(out.String(), "Forwarding") {
					fwdWaitGroup.Done()
				}
			}
		}()
		go func() {
			if err = forwarder.ForwardPorts(); err != nil { // Locks until stopChan is closed.
				forwarderErrChan <- err
			}
		}()
	}

	log.Info("Waiting for port forward to finish")

	doneCh := make(chan struct{})
	go func() {
		fwdWaitGroup.Wait()
		close(doneCh)
	}()

	select {
	case err := <-forwarderErrChan:
		return nil, err
	case <-doneCh:
		return &sourcePorts, nil
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

func watchForReady(deployment *appsv1.Deployment, readyChan chan<- bool) {
	go func() {
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
		progressDeadlineSeconds += 5

		clientMutex.RLock()
		watch, err := deploymentsClient.Watch(context.Background(), metav1.ListOptions{
			LabelSelector:  labels.Set(deployment.Labels).String(),
			TimeoutSeconds: &progressDeadlineSeconds,
		})
		clientMutex.RUnlock()

		if err != nil {
			log.Error(err)
			readyChan <- false
			return
		}

		defer watch.Stop()

		for event := range watch.ResultChan() {
			d, ok := event.Object.(*appsv1.Deployment)
			if !ok {
				continue
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
		}

		readyChan <- false
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
