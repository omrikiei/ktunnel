package k8s
/*
import (
	"context"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type cleanerState struct {
	namespace string
	deployments []appsv1.Deployment
	services []corev1.Service
	injectedDeployments []appsv1.Deployment
}

func NewCleanerState(namespace string) *cleanerState {
	return &cleanerState{
		namespace:   namespace,
		deployments: []appsv1.Deployment{},
		services:    []corev1.Service{},
	}
}

func (c *cleanerState) RefreshAll(ctx context.Context) {
	getClients(&c.namespace)
	listOptions := metav1.ListOptions{ LabelSelector: customLabelSelector,}
	deploymentList, err := deploymentsClient.List(ctx, listOptions)

	if err != nil {
		log.Errorf("failed finding ktunnel deployments in cluster: %v", err)
	} else {
		c.deployments = deploymentList.Items
	}

	servicesList, err := svcClient.List(ctx, listOptions)

	if err != nil {
		log.Errorf("failed finding ktunnel services in cluster: %v", err)
	} else {
		c.services = servicesList.Items
	}

	podsList, err := podsClient.List(ctx, )
}

func (c *cleanerState) Clean(ctx context.Context) error {
	getClients(&c.namespace)

}*/