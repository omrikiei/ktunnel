package k8s

import (
        "context"
        "net/url"
        "sync"
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
        fakeClient := testclient.NewSimpleClientset()
        deploymentsClient = fakeClient.AppsV1().Deployments(namespace)
        podsClient = fakeClient.CoreV1().Pods(namespace)
        
        // Set up a mock kubeconfig
        kubeconfig = &rest.Config{
                Host: "https://fake.example.com",
        }
}

func createDeployment(c v1.DeploymentInterface, name string, replicas int32, containers *[]v14.Container) error {
        d := v12.Deployment{
                ObjectMeta: metav1.ObjectMeta{
                        Name: name,
                },
                Spec: v12.DeploymentSpec{
                        Replicas: &replicas,
                        Selector: &metav1.LabelSelector{
                                MatchLabels: map[string]string{
                                        "app": name,
                                },
                        },
                        Template: v14.PodTemplateSpec{
                                ObjectMeta: metav1.ObjectMeta{
                                        Labels: map[string]string{
                                                "app": name,
                                        },
                                },
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
        // Reset the deploymentOnce to allow reinitialization
        deploymentOnce = sync.Once{}
        
        namespace := "default"
        objectName := "test-deployment"
        port := 8080
        image := "test-image:latest"
        readyChan := make(chan bool)

        // Create a test deployment
        containers := []v14.Container{
                {
                        Name:  "main-container",
                        Image: "main-image:latest",
                },
        }

        // Initialize mock client
        fakeClient := testclient.NewSimpleClientset()
        deploymentsClient = fakeClient.AppsV1().Deployments(namespace)
        podsClient = fakeClient.CoreV1().Pods(namespace)

        err := createDeployment(deploymentsClient, objectName, 1, &containers)
        if err != nil {
                t.Fatalf("Failed to create test deployment: %v", err)
        }

        // Create a mock container for injection
        co := newContainer(port, image, []v14.ContainerPort{}, "", "", 100, 500, 100, 1000)

        // Get the deployment and inject the sidecar directly
        deployment, err := deploymentsClient.Get(context.Background(), objectName, metav1.GetOptions{})
        if err != nil {
                t.Fatalf("Failed to get deployment: %v", err)
        }

        // Test sidecar injection
        injected, err := injectToDeployment(deployment, co, image, readyChan)
        if err != nil {
                t.Errorf("injectToDeployment failed: %v", err)
        }
        if !injected {
                t.Error("injectToDeployment returned false but expected true")
        }

        // Verify the injection
        deployment, err = deploymentsClient.Get(context.Background(), objectName, metav1.GetOptions{})
        if err != nil {
                t.Fatalf("Failed to get deployment: %v", err)
        }

        found := false
        for _, container := range deployment.Spec.Template.Spec.Containers {
                if container.Image == image {
                        found = true
                        break
                }
        }
        if !found {
                t.Error("Sidecar container was not injected properly")
        }

        // Test injection when deployment has more than one replica
        deployment.Spec.Replicas = new(int32)
        *deployment.Spec.Replicas = 2
        _, err = deploymentsClient.Update(context.Background(), deployment, metav1.UpdateOptions{})
        if err != nil {
                t.Fatalf("Failed to update deployment: %v", err)
        }

        // Try to inject again - should fail due to multiple replicas
        deployment, err = deploymentsClient.Get(context.Background(), objectName, metav1.GetOptions{})
        if err != nil {
                t.Fatalf("Failed to get deployment: %v", err)
        }

        // Test duplicate injection (should return true with no error)
        injected, err = injectToDeployment(deployment, co, image, readyChan)
        if err != nil {
                t.Errorf("Unexpected error on duplicate injection: %v", err)
        }
        if !injected {
                t.Error("Expected true for duplicate injection")
        }
}

func Test_removeFromSpec(t *testing.T) {
        // Test cases
        testCases := []struct {
                name          string
                containers    []v14.Container
                imageToRemove string
                expectError   bool
                expectedLen   int
        }{
                {
                        name: "Remove existing container",
                        containers: []v14.Container{
                                {Name: "container1", Image: "image1:latest"},
                                {Name: "container2", Image: "image2:latest"},
                        },
                        imageToRemove: "image1:latest",
                        expectError:   false,
                        expectedLen:   1,
                },
                {
                        name: "Remove non-existent container",
                        containers: []v14.Container{
                                {Name: "container1", Image: "image1:latest"},
                        },
                        imageToRemove: "image2:latest",
                        expectError:   true,
                        expectedLen:   1,
                },
                {
                        name:          "Remove from empty spec",
                        containers:    []v14.Container{},
                        imageToRemove: "image1:latest",
                        expectError:   true,
                        expectedLen:   0,
                },
        }

        for _, tc := range testCases {
                t.Run(tc.name, func(t *testing.T) {
                        spec := &v14.PodSpec{
                                Containers: tc.containers,
                        }

                        success, err := removeFromSpec(spec, tc.imageToRemove)
                        if tc.expectError && err == nil {
                                t.Error("Expected error but got none")
                        }
                        if !tc.expectError && err != nil {
                                t.Errorf("Unexpected error: %v", err)
                        }
                        if len(spec.Containers) != tc.expectedLen {
                                t.Errorf("Expected %d containers, got %d", tc.expectedLen, len(spec.Containers))
                        }
                        if !tc.expectError && !success {
                                t.Error("Expected success but got failure")
                        }
                })
        }
}
