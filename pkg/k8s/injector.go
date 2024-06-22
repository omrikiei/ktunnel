package k8s

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func SetLogLevel(l log.Level) {
	log.SetLevel(l)
	if l.String() == "verbose" || l.String() == "debug" {
		Verbose = true
	}
}

func injectToDeployment(o *appsv1.Deployment, c *apiv1.Container, image string, readyChan chan<- bool) (bool, error) {
	if hasSidecar(o.Spec.Template.Spec, image) {
		log.Warn(fmt.Sprintf("%s already injected to the deployment", image))
		watchForReady(o, readyChan)
		return true, nil
	}
	o.Spec.Template.Spec.Containers = append(o.Spec.Template.Spec.Containers, *c)
	u, updateErr := deploymentsClient.Update(context.Background(), o, metav1.UpdateOptions{
		TypeMeta:     metav1.TypeMeta{},
		DryRun:       nil,
		FieldManager: "",
	})
	if updateErr != nil {
		return false, updateErr
	}
	watchForReady(u, readyChan)
	return true, nil
}

func InjectSidecar(namespace, objectName *string, port *int, image string, cert string, key string, readyChan chan<- bool, kubecontext *string) (bool, error) {
	log.Infof("Injecting tunnel sidecar to %s/%s", *namespace, *objectName)
	getClients(namespace, kubecontext)
	cpuReq := int64(100) // in milli-cpu
	cpuLimit := int64(500)
	memReq := int64(100) // in mega-bytes
	memLimit := int64(1000)
	co := newContainer(*port, image, []apiv1.ContainerPort{}, cert, key, cpuReq, cpuLimit, memReq, memLimit)
	obj, err := deploymentsClient.Get(context.Background(), *objectName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if *obj.Spec.Replicas > int32(1) {
		return false, errors.New("sidecar injection only support deployments with one replica")
	}
	_, err = injectToDeployment(obj, co, image, readyChan)
	if err != nil {
		return false, err
	}
	return true, nil
}

func removeFromSpec(s *apiv1.PodSpec, image string) (bool, error) {
	if !hasSidecar(*s, image) {
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
		s.Containers = append(containers[:cIndex], containers[cIndex+1:]...)
		return true, nil
	} else {
		return false, errors.New("container not found on spec")
	}
}

func RemoveSidecar(namespace, objectName *string, image string, readyChan chan<- bool, kubecontext *string) (bool, error) {
	log.Infof("Removing tunnel sidecar from %s/%s", *namespace, *objectName)
	getClients(namespace, kubecontext)
	obj, err := deploymentsClient.Get(context.Background(), *objectName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	_, err = removeFromSpec(&obj.Spec.Template.Spec, image)
	if err != nil {
		return false, err
	}
	u, updateErr := deploymentsClient.Update(context.Background(), obj, metav1.UpdateOptions{
		TypeMeta:     metav1.TypeMeta{},
		DryRun:       nil,
		FieldManager: "",
	})
	if updateErr != nil {
		return false, updateErr
	}
	watchForReady(u, readyChan)
	return true, nil
}
