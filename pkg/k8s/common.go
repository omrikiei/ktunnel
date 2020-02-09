package k8s

import (
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
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
	"k8s.io/client-go/util/homedir"
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
var svcClient v12.ServiceInterface
var kubeconfig = getKubeConfig()
var Verbose = false

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
	deploymentOnce.Do(func() {
		clientset, err := kubernetes.NewForConfig(kubeconfig)
		if err != nil {
			log.Errorf("Failed to get k8s client: %v", err)
			os.Exit(1)
		}

		deploymentsClient = clientset.AppsV1().Deployments(*namespace)
		podsClient = clientset.CoreV1().Pods(*namespace)
		svcClient = clientset.CoreV1().Services(*namespace)
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

func waitForReady(name *string, ti *time.Time, numPods int32, readyChan chan<- bool) {
	go func() {
		/*
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
			}*/
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
			time.Sleep(time.Millisecond * 300)
		}
	}()
}

func hasSidecar(podSpec apiv1.PodSpec) bool {
	for _, c := range podSpec.Containers {
		if c.Image == image {
			return true
		}
	}
	return false
}

func newContainer(port int) *apiv1.Container {
	args := []string{"server", "-p", strconv.FormatInt(int64(port), 10)}
	if Verbose == true {
		args = append(args, "-v")
	}
	return &apiv1.Container{
		Name:    "ktunnel",
		Image:   image,
		Command: []string{"/ktunnel/ktunnel"},
		Args:    args,
	}
}

func newDeployment(namespace, name string, port int) *appsv1.Deployment {
	replicas := int32(1)
	co := newContainer(port)
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
