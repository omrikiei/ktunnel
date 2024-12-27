// Package k8s provides Kubernetes integration functionality for ktunnel
package k8s

import (
	"sync"

	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	v12 "k8s.io/client-go/kubernetes/typed/core/v1"
)

var (
	clientMutex       sync.RWMutex
	deploymentsClient v1.DeploymentInterface
	podsClient        v12.PodInterface
	svcClient         v12.ServiceInterface
)

func getDeploymentsClient() v1.DeploymentInterface {
	clientMutex.RLock()
	defer clientMutex.RUnlock()
	return deploymentsClient
}

func getServicesClient() v12.ServiceInterface {
	clientMutex.RLock()
	defer clientMutex.RUnlock()
	return svcClient
}

func setClients(deployments v1.DeploymentInterface, pods v12.PodInterface, services v12.ServiceInterface) {
	clientMutex.Lock()
	defer clientMutex.Unlock()
	deploymentsClient = deployments
	podsClient = pods
	svcClient = services
}
