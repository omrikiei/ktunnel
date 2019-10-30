package k8s

import (
	"bytes"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/homedir"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)


const (
	image = "quay.io/omrikiei/ktunnel:latest"
)

var deploymentOnce = sync.Once{}
var deploymentsClient v1.DeploymentInterface
var podsClient v12.PodInterface
var kubeconfig = getKubeConfig()

func getKubeConfig() *rest.Config {
	kconfig := os.Getenv("KUBE_CONFIG")
	if home := homedir.HomeDir(); kconfig == "" && home != "" {
		kconfig = filepath.Join(home, ".kube", "config")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kconfig)
	if err != nil {
		log.Errorf("Failed getting kubernetes config: %v", err)
		return nil
	}
	return config
}

func getClients(namespace *string) {
	deploymentOnce.Do(func(){
		clientset, err := kubernetes.NewForConfig(kubeconfig)
		if err != nil {
			log.Errorf("Failed to get k8s client: %v", err)
			os.Exit(1)
		}

		c := clientset.AppsV1().Deployments(*namespace)
		deploymentsClient = c
		p := clientset.CoreV1().Pods(*namespace)
		podsClient = p
	})
}

func getAllPods(namespace, deployment *string) (*apiv1.PodList, error) {
	getClients(namespace)
	// TODO: filter pod list
	pods, err := podsClient.List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return pods, nil
}

func hasSidecar(podSpec apiv1.PodSpec) bool {
	for _, c := range podSpec.Containers {
		if c.Image == image {
			return true
		}
	}
	return false
}

func injectToDeployment(o *appsv1.Deployment, c *apiv1.Container, readyChan chan<- bool) (bool, error) {
	if hasSidecar(o.Spec.Template.Spec) {
		log.Warn(fmt.Sprintf("%s already injected to the deplyoment", image))
		readyChan <- true
		return true, nil
	}
	o.Spec.Template.Spec.Containers = append(o.Spec.Template.Spec.Containers, *c)
	_, updateErr := deploymentsClient.Update(o)
	if updateErr != nil {
		return false, updateErr
	}
	return true, nil
}

func waitForReady(name *string, ti *time.Time, numPods int32, readyChan chan<-bool) {
	go func() {
		watcher, err := podsClient.Watch(metav1.ListOptions{
			TypeMeta:            metav1.TypeMeta{
				Kind: "pod",
			},
			Watch:               true,
		})
		if err != nil {
			return
		}
		for e := range watcher.ResultChan(){
			if e.Type == watch.Deleted {
				break
			}
		}
		for {
			count := int32(0)
			pods, err := podsClient.List(metav1.ListOptions{})
			if err != nil {
				log.Error(err)
				os.Exit(1)
			}
			for _, p := range pods.Items {
				if strings.HasPrefix(p.Name, *name) && p.CreationTimestamp.After(*ti) && p.Status.Phase == apiv1.PodRunning {
					count += 1
				}
				if count >= numPods {
					readyChan <- true
					break
				}
			}
		}
	}()
}

func InjectSidecar(namespace, objectName *string, port *int, readyChan chan<- bool) (bool, error) {
	log.Infof("Injecting tunnel sidecar to %s/%s", *namespace, *objectName)
	getClients(namespace)
	co := apiv1.Container{
		Name: "ktunnel",
		Image: image,
		Command: []string{"/ktunnel/ktunnel"},
		Args: []string{ "server", "-p", strconv.FormatInt(int64(*port), 10)},
	}
	creationTime := time.Now().Add(-1*time.Second)
	obj, err := deploymentsClient.Get(*objectName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if *obj.Spec.Replicas > int32(1) {
		return false, errors.New("sidecar injection only support deployments with one replica")
	}
	_, err = injectToDeployment(obj, &co, readyChan)
	if err != nil {
		return false, err
	}

	waitForReady(objectName, &creationTime, *obj.Spec.Replicas, readyChan)
	return true, nil
}

func removeFromSpec(s *apiv1.PodSpec) (bool, error) {
	if !hasSidecar(*s) {
		return true, errors.New(fmt.Sprintf("%s is not present on spec", image))
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
		s.Containers =  append(containers[:cIndex], containers[cIndex+1:]...)
		return true, nil
	} else {
		return false, errors.New("container not found on spec")
	}
}

func RemoveSidecar(namespace, objectName *string, readyChan chan<- bool) (bool, error) {
	log.Infof("Removing tunnel sidecar from %s/%s", *namespace, *objectName)
	getClients(namespace)
	obj, err := deploymentsClient.Get(*objectName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	deletionTime := time.Now().Add(-1*time.Second)
	_, err = removeFromSpec(&obj.Spec.Template.Spec)
	if err != nil {
		return false, err
	}
	_, updateErr := deploymentsClient.Update(obj)
	if updateErr != nil {
		return false, updateErr
	}
	waitForReady(objectName, &deletionTime, *obj.Spec.Replicas, readyChan)
	return true, nil
}

func getPodNames(namespace, deploymentName *string, podsPtr *[]string) error {
	allPods, err := getAllPods(namespace, deploymentName)
	if err != nil {
		return err
	}
	pods := *podsPtr
	pIndex := 0
	for _,p := range allPods.Items {
		if pIndex >= len(pods) {
			log.Info("All pods located for port-forwarding")
			break
		}
		if strings.HasPrefix(p.Name, *deploymentName) && p.Status.Phase == apiv1.PodRunning {
			pods[pIndex] = p.Name
			pIndex += 1
		}
	}

	return nil

}

func PortForward(namespace, deploymentName *string, targetPort string, fwdWaitGroup *sync.WaitGroup, stopChan <-chan struct{}) (*[]string, error) {
	getClients(namespace)

	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	deployment, err := deploymentsClient.Get(*deploymentName, metav1.GetOptions{})
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
	for i,podName := range podNames {
		readyChan := make(chan struct{}, 1)
		ports := []string{fmt.Sprintf("%s:%s", sourcePorts[i], targetPort)}
		path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", "default", podName)
		hostIP := strings.TrimLeft(kubeconfig.Host, "https://")
		serverURL := url.URL{
			Scheme: "https",
			Path: path,
			Host: hostIP,
		}

		transport, upgrader, err := spdy.RoundTripperFor(kubeconfig)
		if err != nil {
			return nil, err
		}
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, &serverURL)

		forwarder, err := portforward.New(dialer, ports, stopChan, readyChan, out, errOut)
		if err != nil {
			log.Error(err)
		}

		go func() {
			for range readyChan { // Kubernetes will close this channel when it has something to tell us.
			}
			if len(errOut.String()) != 0 {
				log.Error(errOut.String())
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