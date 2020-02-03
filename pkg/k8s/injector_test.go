package k8s

import (
	v12 "k8s.io/api/apps/v1"
	v14 "k8s.io/api/core/v1"
	testclient "k8s.io/client-go/kubernetes/fake"
	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/client-go/rest"
	"net/url"
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

func TestGetPortForwardUrl(t *testing.T) {
	tables := []struct{
		Config rest.Config
		Namespace string
		Pod string
		Expected *url.URL
	}{
		{
			Config:	rest.Config{
				Host: "https://api.qa.kube.com",
			},
			Namespace: "default",
			Pod: "test",
			Expected: &url.URL{
					Scheme: "https",
					Host: "api.qa.kube.com",
					Path: "/api/v1/namespaces/default/pods/test/portforward",
			},
		},
		{
			Config:	rest.Config{
				Host: "https://rancher.xyz.io/k8s/clusters/c-wfdqx",
			},
			Namespace: "default",
			Pod: "test",
			Expected: &url.URL{
				Scheme: "https",
				Host: "rancher.xyz.io",
				Path: "/k8s/clusters/c-wfdqx/api/v1/namespaces/default/pods/test/portforward",
			},
		},
	}

	for _, table := range tables {
		res := getPortForwardUrl(&table.Config, table.Namespace, table.Pod)
		if res.Scheme != table.Expected.Scheme || res.Host != table.Expected.Host || res.Path != table.Expected.Path {
			t.Errorf("expected: %v, got: %v", table.Expected, res)
		}
	}
}

func TestInjectSidecar(t *testing.T) {
	createMockClient()
}