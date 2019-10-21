package injector

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
	pods, err := podsClient.List(metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", *deployment),
	})
	if err != nil {
		return nil, err
	}
	return pods, nil
}

func hasSidecar(deployment *appsv1.Deployment) bool {
	for _, c := range deployment.Spec.Template.Spec.Containers {
		if c.Image == image {
			return true
		}
	}
	return false
}

func InjectSidecar(namespace, deploymentName *string, port *int, readyChan chan<- bool) (bool, error) {
	log.Infof("Injecting tunnel sidecar to deployment %s/%s", *namespace, *deploymentName)
	getClients(namespace)
	d := *deploymentName
	deployment, err := deploymentsClient.Get(d, metav1.GetOptions{})
	
	if err != nil {
		return false, err
	}

	if hasSidecar(deployment) {
		log.Warn(fmt.Sprintf("%s already injected to the deplyoment", image))
		readyChan <- true
		return true, nil
	}
	
	co := apiv1.Container{
		Name: "ktunnel",
		Image: image,
		Command: []string{"/ktunnel/ktunnel"},
		Args: []string{ "server", fmt.Sprintf("-p=%d", *port)},
	}

	deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers, co)

	deplotmentCreationTime := time.Now().Add(-1*time.Second)
	deployment, updateErr := deploymentsClient.Update(deployment)
	if updateErr != nil {
		return false, updateErr
	}
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
			pods, err := podsClient.List(metav1.ListOptions{})
			if err != nil {
				log.Error(err)
				os.Exit(1)
			}
			for _, p := range pods.Items {
				if strings.HasPrefix(p.Name, *deploymentName) && p.CreationTimestamp.After(deplotmentCreationTime) && p.Status.Phase == apiv1.PodRunning {
					readyChan <- true
				}
			}
		}
	}()
	return true, nil
}

func RemoveSidecar(namespace, deploymentName *string) (bool, error) {
	log.Infof("Removing tunnel sidecar from deployment %s/%s", *namespace, *deploymentName)
	getClients(namespace)
	deployment, err := deploymentsClient.Get(*deploymentName, metav1.GetOptions{})

	if err != nil {
		log.Error(err)
		return false, err
	}

	if !hasSidecar(deployment) {
		return true, errors.New(fmt.Sprintf("%s is not present on the deployment", image))
	}

	cIndex := -1
	for i, c := range deployment.Spec.Template.Spec.Containers {
		if c.Image == image {
			cIndex = i
			break
		}
	}

	if cIndex != -1 {
		log.Info("Found side-car in deployment - removing it from the manifest")
		containers := deployment.Spec.Template.Spec.Containers
		deployment.Spec.Template.Spec.Containers =  append(containers[:cIndex], containers[cIndex+1:]...)
		_, updateErr := deploymentsClient.Update(deployment)
		if updateErr != nil {
			return false, updateErr
		}
		return true, nil
	}

	return false, errors.New("container not found")

}

func getPodName(namespace, deploymentName *string) (string, error) {
	getClients(namespace)
	deployment, err := deploymentsClient.Get(*deploymentName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if *deployment.Spec.Replicas != int32(1) {
		return "", errors.New("%s is not scaled down to 1 replicas")
	}

	pods, err := getAllPods(namespace, deploymentName)
	if err != nil {
		return "", err
	}

	for _,p := range pods.Items {
		if strings.HasPrefix(p.Name, *deploymentName) {
			if p.Status.Phase != apiv1.PodRunning {
				for {
					pods, err := getAllPods(namespace, deploymentName)
					if err != nil {
						return "", err
					}
					for _, pod := range pods.Items {
						if pod.Status.Phase == apiv1.PodRunning {
							return pod.Name, nil
						}
					}

				}
			}
			return p.Name, nil
		}
	}

	return "", errors.New(fmt.Sprintf("Pod for deployment %s not found", *deploymentName))

}

func PortForward(namespace, deploymentName *string, ports *[]string, fwdChan chan<- bool) (bool, error) {
	transport, upgrader, err := spdy.RoundTripperFor(kubeconfig)
	if err != nil {
		return false, err
	}

	stopChan, readyChan := make(chan struct{}, 1), make(chan struct{}, 1)
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)

	podName, err := getPodName(namespace, deploymentName)

	if err != nil {
		return false, err
	}

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", "default", podName)
	hostIP := strings.TrimLeft(kubeconfig.Host, "https://")
	serverURL := url.URL{
		Scheme: "https",
		Path: path,
		Host: hostIP,
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, &serverURL)

	forwarder, err := portforward.New(dialer, *ports, stopChan, readyChan, out, errOut)
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
				fwdChan <- true
			}
		}
	}()
	go func() {
		if err = forwarder.ForwardPorts(); err != nil { // Locks until stopChan is closed.
			log.Error(err)
		}
	}()
	return true, nil
}