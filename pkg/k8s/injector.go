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

func injectToDeployment(o *appsv1.Deployment, c *apiv1.Container, image string, readyChan chan<- bool) (bool, error) {
	if hasSidecar(o.Spec.Template.Spec, image) {
		log.Warn(fmt.Sprintf("%s already injected to the deplyoment", image))
		readyChan <- true
		return true, nil
	}
	o.Spec.Template.Spec.Containers = append(o.Spec.Template.Spec.Containers, *c)
	_, updateErr := deploymentsClient.Update(context.Background(), o, metav1.UpdateOptions{
		TypeMeta:     metav1.TypeMeta{},
		DryRun:       nil,
		FieldManager: "",
	})
	if updateErr != nil {
		return false, updateErr
	}
	waitForReady(&o.Name, o.GetCreationTimestamp().Time, *o.Spec.Replicas, readyChan)
	return true, nil
}

func InjectSidecar(namespace, objectName *string, port *int, image string, readyChan chan<- bool) (bool, error) {
	log.Infof("Injecting tunnel sidecar to %s/%s", *namespace, *objectName)
	getClients(namespace)
	co := newContainer(*port, image, []apiv1.ContainerPort{})
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

func RemoveSidecar(namespace, objectName *string, image string, readyChan chan<- bool) (bool, error) {
	log.Infof("Removing tunnel sidecar from %s/%s", *namespace, *objectName)
	getClients(namespace)
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
	waitForReady(objectName, u.GetCreationTimestamp().Time, *obj.Spec.Replicas, readyChan)
	return true, nil
}
