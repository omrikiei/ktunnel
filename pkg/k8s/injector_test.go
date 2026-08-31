package k8s

import (
	"context"
	"net/url"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "k8s.io/client-go/kubernetes/fake"
	v1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	"k8s.io/client-go/rest"

	v12 "k8s.io/api/apps/v1"
	v14 "k8s.io/api/core/v1"
)

type TestCase struct {
	Containers []v14.Container
	Replicas   int32
	BoolResult bool
	ErrResult  error
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

// useFakeClients points the package-level clients at a fake API server.
//
// Under the mutex, because watchForReady polls deploymentsClient from a
// goroutine that outlives the test that started it -- a fake deployment never
// reaches ready, so it polls for the rest of the run. Assigning over it
// unsynchronized is a data race between two tests in this package, which is
// how it was found.
func useFakeClients(fake *testclient.Clientset, namespace string) {
	clientMutex.Lock()
	defer clientMutex.Unlock()
	deploymentsClient = fake.AppsV1().Deployments(namespace)
	podsClient = fake.CoreV1().Pods(namespace)
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

func TestGetPortForwardURL(t *testing.T) {
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
	// Reset the deploymentOnce to allow reinitialization
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
	useFakeClients(fakeClient, namespace)

	err := createDeployment(deploymentsClient, objectName, 1, &containers)
	if err != nil {
		t.Fatalf("Failed to create test deployment: %v", err)
	}

	// Create a mock container for injection
	co := newContainer(port, image, []v14.ContainerPort{}, PodCredentials{}, 100, 500, 100, 1000)

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

// Test_InjectSidecar_MultipleReplicas is the regression test for `inject`
// refusing every deployment that runs more than one pod.
//
// It used to return "sidecar injection only support deployments with one
// replica" and stop there, which rules out most of what a cluster runs. The
// sidecar goes into the pod template, so a rollout puts one in every replica,
// and PortForward already builds a forward and a tunnel client per pod --
// there was nothing behind the refusal to build.
func Test_InjectSidecar_MultipleReplicas(t *testing.T) {
	namespace := "default"
	objectName := "multi-replica"
	port := 28688
	image := "test-image:latest"

	fakeClient := testclient.NewSimpleClientset()
	useFakeClients(fakeClient, namespace)
	svc := &KubeService{clients: &Clients{
		Deployments: fakeClient.AppsV1().Deployments(namespace),
		Pods:        fakeClient.CoreV1().Pods(namespace),
	}}

	containers := []v14.Container{{Name: "app", Image: "app:latest"}}
	if err := createDeployment(deploymentsClient, objectName, 3, &containers); err != nil {
		t.Fatalf("Failed to create test deployment: %v", err)
	}

	// Buffered, so watchForReady's goroutine is not left blocked on a channel
	// this test never reads.
	readyChan := make(chan bool, 1)
	ok, err := svc.InjectSidecar(&namespace, &objectName, &port, image, PodCredentials{}, readyChan, nil)
	if err != nil {
		t.Fatalf("failed injecting into a three-replica deployment: %v; "+
			"most deployments worth tunnelling into run more than one pod, and refusing them rules out the command", err)
	}
	if !ok {
		t.Fatal("InjectSidecar reported failure without an error for a three-replica deployment")
	}

	deployment, err := deploymentsClient.Get(context.Background(), objectName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get deployment: %v", err)
	}
	// The sidecar goes into the template, which is what makes every replica
	// carry one: injecting into a subset of the pods is not a thing the API
	// offers, and it is not what this supports.
	if !hasSidecar(deployment.Spec.Template.Spec, image) {
		t.Error("the sidecar is not in the pod template, so no replica would come back carrying it")
	}
	if replicaCount(deployment) != 3 {
		t.Errorf("the deployment now wants %d replicas; injecting must not scale the user's deployment to make its own life easier", replicaCount(deployment))
	}
}

// Test_ReplicaCount pins the default behind spec.replicas being a pointer: the
// API server defaults an unset one to 1, so nil is one pod, not none. It was
// dereferenced unchecked in the injector and in PortForward.
func Test_ReplicaCount(t *testing.T) {
	if got := replicaCount(&v12.Deployment{}); got != 1 {
		t.Errorf("replicaCount of a deployment with no spec.replicas is %d, want 1", got)
	}
	three := int32(3)
	if got := replicaCount(&v12.Deployment{Spec: v12.DeploymentSpec{Replicas: &three}}); got != 3 {
		t.Errorf("replicaCount is %d, want 3", got)
	}
}
