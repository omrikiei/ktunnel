package k8s

import (
        "net/url"
        "testing"

        v14 "k8s.io/api/core/v1"
        testclient "k8s.io/client-go/kubernetes/fake"
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
        fakeClient := testclient.NewSimpleClientset()
        
        setClients(
                fakeClient.AppsV1().Deployments(namespace),
                fakeClient.CoreV1().Pods(namespace),
                fakeClient.CoreV1().Services(namespace),
        )
        
        // Set up a mock kubeconfig
        kubeconfig = &rest.Config{
                Host: "https://fake.example.com",
        }
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
                res := getPortForwardURL(&table.Config, table.Namespace, table.Pod)
                if res.Scheme != table.Expected.Scheme || res.Host != table.Expected.Host || res.Path != table.Expected.Path {
                        t.Errorf("expected: %v, got: %v", table.Expected, res)
                }
        }
}

func Test_InjectSidecar(t *testing.T) {
        createMockClient(nil)
}