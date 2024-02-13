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
	"time"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
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
	Image            = "docker.io/omrieival/ktunnel"
	kubeConfigEnvVar = "KUBECONFIG"
)

type ByCreationTime []apiv1.Pod

func (a ByCreationTime) Len() int { return len(a) }
func (a ByCreationTime) Less(i, j int) bool {
	return a[i].CreationTimestamp.After(a[j].CreationTimestamp.Time)
}
func (a ByCreationTime) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

var deploymentOnce = sync.Once{}
var deploymentsClient v1.DeploymentInterface
var podsClient v12.PodInterface
var svcClient v12.ServiceInterface
var kubeconfig *rest.Config
var o = sync.Once{}
var Verbose = false

func getKubeConfig(kubeCtx *string) *rest.Config {
	o.Do(func() {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

		kConfig := os.Getenv(kubeConfigEnvVar)
		if home := homedir.HomeDir(); kConfig == "" && home != "" {
			kConfig = filepath.Join(home, ".kube", "config")
			loadingRules.ExplicitPath = kConfig
		}

		var configOverrides *clientcmd.ConfigOverrides
		if (kubeCtx) != nil {
			configOverrides = &clientcmd.ConfigOverrides{CurrentContext: *kubeCtx}
		}

		config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides).ClientConfig()
		if err != nil {
			log.Errorf("Failed getting kubernetes config: %v", err)
		}
		kubeconfig = config
	})
	return kubeconfig
}

func getClients(namespace *string, kubeCtx *string) {
	deploymentOnce.Do(func() {
		clientSet, err := kubernetes.NewForConfig(getKubeConfig(kubeCtx))
		if err != nil {
			log.Errorf("Failed to get k8s client: %v", err)
			os.Exit(1)
		}

		deploymentsClient = clientSet.AppsV1().Deployments(*namespace)
		podsClient = clientSet.CoreV1().Pods(*namespace)
		svcClient = clientSet.CoreV1().Services(*namespace)
	})
}

func getAllPods(namespace, kubeCtx *string) (*apiv1.PodList, error) {
	getClients(namespace, kubeCtx)
	// TODO: filter pod list
	pods, err := podsClient.List(context.Background(), metav1.ListOptions{})
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
	if Verbose == true {
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
	containerUid := int64(1000)

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
			RunAsUser: &containerUid,
		},
	}
}

func newDeployment(namespace, name string, port int, image string, ports []apiv1.ContainerPort, selector map[string]string, deploymentLabels map[string]string, cert, key string, cpuReq, cpuLimit, memReq, memLimit int64) *appsv1.Deployment {
	replicas := int32(1)
	deploymentLabels["app.kubernetes.io/name"] = name
	deploymentLabels["app.kubernetes.io/instance"] = name
	co := newContainer(port, image, ports, cert, key, cpuReq, cpuLimit, memReq, memLimit)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    deploymentLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: deploymentLabels,
			},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: deploymentLabels,
				},
				Spec: apiv1.PodSpec{
					NodeSelector: selector,
					Containers: []apiv1.Container{
						*co,
					},
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

func getPodNames(namespace, deploymentName *string, podsPtr *[]string, kubeCtx *string) error {
	allPods, err := getAllPods(namespace, kubeCtx)
	if err != nil {
		return err
	}
	pods := *podsPtr
	matchingPods := ByCreationTime{}
	pIndex := 0
	for _, p := range allPods.Items {
		if pIndex >= len(pods) {
			log.Info("All pods located for port-forwarding")
			break
		}
		if strings.HasPrefix(p.Name, *deploymentName) && p.Status.Phase == apiv1.PodRunning {
			matchingPods = append(matchingPods, p)
		}
	}
	sort.Sort(matchingPods)
	for i := 0; i < len(pods); i++ {
		pods[i] = matchingPods[i].Name
	}

	return nil

}

func PortForward(namespace, deploymentName *string, targetPort string, fwdWaitGroup *sync.WaitGroup, stopChan <-chan struct{}, kubecontext *string) (*[]string, error) {
	getClients(namespace, kubecontext)

	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	deployment, err := deploymentsClient.Get(context.Background(), *deploymentName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	podNames := make([]string, *deployment.Spec.Replicas)
	err = getPodNames(namespace, deploymentName, &podNames, kubecontext)
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
	for i, podName := range podNames {
		readyChan := make(chan struct{}, 1)
		ports := []string{fmt.Sprintf("%s:%s", sourcePorts[i], targetPort)}
		serverURL := getPortForwardUrl(getKubeConfig(kubecontext), *namespace, podName)

		transport, upgrader, err := spdy.RoundTripperFor(getKubeConfig(kubecontext))
		if err != nil {
			return nil, err
		}
		log.Infof("port forwarding to %s", serverURL)
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, serverURL)

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
				log.Error(err)
			}
		}()
	}
	return &sourcePorts, nil
}

func getPortForwardUrl(config *rest.Config, namespace string, podName string) *url.URL {
	host := config.Host
	scheme := "https"
	if strings.HasPrefix(config.Host, "https://") {
		host = strings.TrimPrefix(config.Host, "https://")
	} else if strings.HasPrefix(config.Host, "http://") {
		host = strings.TrimPrefix(config.Host, "http://")
		scheme = "http"
	}
	trailingHostPath := strings.Split(host, "/")
	hostIp := trailingHostPath[0]
	trailingPath := ""
	if len(trailingHostPath) > 1 && trailingHostPath[1] != "" {
		trailingPath = fmt.Sprintf("/%s/", strings.Join(trailingHostPath[1:], "/"))
	}
	path := fmt.Sprintf("%sapi/v1/namespaces/%s/pods/%s/portforward", trailingPath, namespace, podName)
	return &url.URL{
		Scheme: scheme,
		Path:   path,
		Host:   hostIp,
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

		watch, err := deploymentsClient.Watch(context.Background(), metav1.ListOptions{
			LabelSelector:  labels.Set(deployment.Labels).String(),
			TimeoutSeconds: &progressDeadlineSeconds,
		})

		if err != nil {
			log.Error(err)
			readyChan <- false
			return
		}
		resultChan := watch.ResultChan()

		for {
			event, ok := <-resultChan
			if !ok {
				log.Error("Timeout exceeded waiting for deployment to be ready")
				readyChan <- false
				return
			}

			deployment, ok := event.Object.(*appsv1.Deployment)
			if !ok {
				log.Warn("Watcher received event for non-deployment object")
				continue
			}

			msg, done, err := deploymentStatus(deployment)

			if done {
				watch.Stop()
				log.Info(msg)
				readyChan <- true
				return
			} else if err != nil {
				watch.Stop()
				log.Errorf("Failed deploying tunnel sidecar; %v", err)
				readyChan <- false
				return
			} else {
				if lastMsg != msg {
					log.Debug(msg)
				}
				lastMsg = msg
			}
		}
	}()
}

func deploymentStatus(deployment *appsv1.Deployment) (string, bool, error) {
	if deployment.Generation <= deployment.Status.ObservedGeneration {
		updateTime := time.Now()
		for _, field := range deployment.ManagedFields {
			if field.Manager == "kube-controller-manager" && field.Operation == "Update" {
				updateTime = field.Time.Time
			}
		}

		for _, cond := range deployment.Status.Conditions {
			if cond.Type == appsv1.DeploymentProgressing &&
				cond.Status == apiv1.ConditionFalse &&
				cond.Reason == "ProgressDeadlineExceeded" &&
				cond.LastUpdateTime.Time.After(updateTime) {
				return "", false, fmt.Errorf("deployment %q exceeded its progress deadline", deployment.Name)
			}
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
	return fmt.Sprintf("Waiting for deployment spec update to be observed...\n"), false, nil
}
