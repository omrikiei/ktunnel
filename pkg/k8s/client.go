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

type Clients struct {
	Deployments v1.DeploymentInterface
	Pods        v12.PodInterface
	Services    v12.ServiceInterface
}
