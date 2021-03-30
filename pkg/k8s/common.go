package k8s

import (
	"bytes"
	"context"
	"fmt"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp" // https://github.com/kubernetes/client-go/issues/242
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	_ "k8s.io/client-go/plugin/pkg/client/auth/openstack"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/homedir"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Image = "quay.io/omrikiei/ktunnel"
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

func getKubeConfig() *rest.Config {
	o.Do(func() {
		kconfig := os.Getenv("KUBECONFIG")
		if home := homedir.HomeDir(); kconfig == "" && home != "" {
			kconfig = filepath.Join(home, ".kube", "config")
		}

		config, err := clientcmd.BuildConfigFromFlags("", kconfig)
		if err != nil {
			log.Errorf("Failed getting kubernetes config: %v", err)
		}
		kubeconfig = config
	})
	return kubeconfig
}

func getClients(namespace *string) {
	deploymentOnce.Do(func() {
		clientset, err := kubernetes.NewForConfig(getKubeConfig())
		if err != nil {
			log.Errorf("Failed to get k8s client: %v", err)
			os.Exit(1)
		}

		deploymentsClient = clientset.AppsV1().Deployments(*namespace)
		podsClient = clientset.CoreV1().Pods(*namespace)
		svcClient = clientset.CoreV1().Services(*namespace)
	})
}

func getAllPods(namespace *string) (*apiv1.PodList, error) {
	getClients(namespace)
	// TODO: filter pod list
	pods, err := podsClient.List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return pods, nil
}

func waitForReady(name *string, ti time.Time, numPods int32, readyChan chan<- bool) {
	go func() {
		for {
			count := int32(0)
			pods, err := podsClient.List(context.Background(), metav1.ListOptions{})
			if err != nil {
				log.Error(err)
				os.Exit(1)
			}
			for _, p := range pods.Items {
				if strings.HasPrefix(p.Name, *name) && p.CreationTimestamp.After(ti) && p.Status.Phase == apiv1.PodRunning {
					count += 1
				}
				if count == numPods {
					readyChan <- true
					break
				}
			}
			time.Sleep(time.Millisecond * 300)
		}
	}()
}

func hasSidecar(podSpec apiv1.PodSpec, image string) bool {
	for _, c := range podSpec.Containers {
		if c.Image == image {
			return true
		}
	}
	return false
}

func newContainer(port int, image string, containerPorts []apiv1.ContainerPort) *apiv1.Container {
	args := []string{"server", "-p", strconv.FormatInt(int64(port), 10)}
	if Verbose == true {
		args = append(args, "-v")
	}
	cpuRequest, cpuLimit, memRequest, memLimit := resource.Quantity{}, resource.Quantity{}, resource.Quantity{}, resource.Quantity{}
	cpuRequest.SetMilli(int64(500))
	cpuLimit.SetMilli(int64(1000))
	memRequest.SetScaled(int64(100), resource.Mega)
	memLimit.SetScaled(int64(1), resource.Giga)

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
	}
}

func newDeployment(namespace, name string, port int, image string, ports []apiv1.ContainerPort) *appsv1.Deployment {
	replicas := int32(1)
	co := newContainer(port, image, ports)
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":     name,
				"app.kubernetes.io/instance": name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     name,
					"app.kubernetes.io/instance": name,
				},
			},
			Template: apiv1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":     name,
						"app.kubernetes.io/instance": name,
					},
				},
				Spec: apiv1.PodSpec{
					Containers: []apiv1.Container{
						*co,
					},
				},
			},
		},
	}
}

func newService(namespace, name string, ports []apiv1.ServicePort) *apiv1.Service {
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
			Selector: map[string]string{
				"app.kubernetes.io/name":     name,
				"app.kubernetes.io/instance": name,
			},
		},
	}
}

func getPodNames(namespace, deploymentName *string, podsPtr *[]string) error {
	allPods, err := getAllPods(namespace)
	if err != nil {
		return err
	}
	pods := *podsPtr
	matchinePods := ByCreationTime{}
	pIndex := 0
	for _, p := range allPods.Items {
		if pIndex >= len(pods) {
			log.Info("All pods located for port-forwarding")
			break
		}
		if strings.HasPrefix(p.Name, *deploymentName) && p.Status.Phase == apiv1.PodRunning {
			matchinePods = append(matchinePods, p)
		}
	}
	sort.Sort(matchinePods)
	for i := 0; i < len(pods); i++ {
		pods[i] = matchinePods[i].Name
	}

	return nil

}

func PortForward(namespace, deploymentName *string, targetPort string, fwdWaitGroup *sync.WaitGroup, stopChan <-chan struct{}) (*[]string, error) {
	getClients(namespace)

	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	deployment, err := deploymentsClient.Get(context.Background(), *deploymentName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	podNames := make([]string, *deployment.Spec.Replicas)
	err = getPodNames(namespace, deploymentName, &podNames)
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
		serverURL := getPortForwardUrl(getKubeConfig(), *namespace, podName)

		transport, upgrader, err := spdy.RoundTripperFor(getKubeConfig())
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
	host := strings.TrimPrefix(config.Host, "https://")
	trailingHostPath := strings.Split(host, "/")
	hostIp := trailingHostPath[0]
	trailingPath := ""
	if len(trailingHostPath) > 1 && trailingHostPath[1] != "" {
		trailingPath = fmt.Sprintf("/%s/", strings.Join(trailingHostPath[1:], "/"))
	}
	path := fmt.Sprintf("%sapi/v1/namespaces/%s/pods/%s/portforward", trailingPath, namespace, podName)
	return &url.URL{
		Scheme: "https",
		Path:   path,
		Host:   hostIp,
	}
}
