package k8s

import (
	"context"
	"net/url"
	"testing"

	v12 "k8s.io/api/apps/v1"
	v14 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "k8s.io/client-go/kubernetes/fake"
	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/client-go/rest"
)

type TestCase struct {
	Containers []v14.Container
	Replicas   int32
	BoolResult bool
	ErrResult  error
}

func createMockClient(kubecontext *string) {
	namespace := "default"
	getClients(&namespace, kubecontext) // Squeeze the initialization from the tested functions and override the clients
	fakeClient := testclient.NewSimpleClientset()
	deploymentsClient = fakeClient.AppsV1().Deployments(namespace)
	podsClient = fakeClient.CoreV1().Pods(namespace)
}

func createDeployment(c v1.DeploymentInterface, replicas int32, containers *[]v14.Container) error {
	d := v12.Deployment{
		Spec: v12.DeploymentSpec{
			Replicas: &replicas,
			Template: v14.PodTemplateSpec{
				Spec: v14.PodSpec{
					Containers: *containers,
				},
			},
		},
	}
	_, err := deploymentsClient.Create(context.Background(), &d, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func TestGetPortForwardUrl(t *testing.T) {
	tables := []struct {
		Config    rest.Config
		Namespace string
		Pod       string
		Expected  *url.URL
	}{
		{
			Config: rest.Config{
				Host: "https://api.qa.kube.com",
			},
			Namespace: "default",
			Pod:       "test",
			Expected: &url.URL{
				Scheme: "https",
				Host:   "api.qa.kube.com",
				Path:   "api/v1/namespaces/default/pods/test/portforward",
			},
		},
		{
			Config: rest.Config{
				Host: "https://rancher.xyz.io/k8s/clusters/c-wfdqx",
			},
			Namespace: "default",
			Pod:       "test",
			Expected: &url.URL{
				Scheme: "https",
				Host:   "rancher.xyz.io",
				Path:   "/k8s/clusters/c-wfdqx/api/v1/namespaces/default/pods/test/portforward",
			},
		},
		{
			Config: rest.Config{
				Host: "https://srv01.mydomain.de:6443",
			},
			Pod:       "myapp-5b65c8777b-dd54r",
			Namespace: "default",
			Expected: &url.URL{
				Scheme: "https",
				Host:   "srv01.mydomain.de:6443",
				Path:   "api/v1/namespaces/default/pods/myapp-5b65c8777b-dd54r/portforward",
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

func Test_InjectSidecar(t *testing.T) {
	createMockClient(nil)
}
