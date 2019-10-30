package k8s

import (
	v12 "k8s.io/api/apps/v1"
	v14 "k8s.io/api/core/v1"
	testclient "k8s.io/client-go/kubernetes/fake"
	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"testing"
)

type TestCase struct {
	Containers []v14.Container
	Replicas int32
	BoolResult bool
	ErrResult error
}

func createMockClient() {
	namespace := "default"
	getClients(&namespace) // Squeeze the initialization from the tested functions and override the clients
	fakeClient := testclient.NewSimpleClientset()
	deploymentsClient = fakeClient.AppsV1().Deployments(namespace)
	podsClient = fakeClient.CoreV1().Pods(namespace)
}

func createDeployment(c v1.DeploymentInterface, replicas int32, containers *[]v14.Container) error {
	d := v12.Deployment{
		Spec:       v12.DeploymentSpec{
			Replicas: &replicas,
			Template: v14.PodTemplateSpec{
				Spec:       v14.PodSpec{
					Containers: *containers,
				},
			},
		},
	}
	_, err := deploymentsClient.Create(&d)
	if err != nil {
		return err
	}
	return nil
}

func TestInjectSidecar(t *testing.T) {
	createMockClient()

	
}