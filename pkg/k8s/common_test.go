package k8s

import (
	"net/url"
	"sort"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// TestVerbose checks the SetVerbose and IsVerbose functions.
func TestVerbose(t *testing.T) {
	// Reset to default state before test
	SetVerbose(false)

	t.Run("set verbose to true", func(t *testing.T) {
		SetVerbose(true)
		if !IsVerbose() {
			t.Error("Expected IsVerbose() to be true after setting it to true")
		}
	})

	t.Run("set verbose to false", func(t *testing.T) {
		SetVerbose(false)
		if IsVerbose() {
			t.Error("Expected IsVerbose() to be false after setting it to false")
		}
	})
}

// Test_hasSidecar checks the hasSidecar function.
func Test_hasSidecar(t *testing.T) {
	sidecarImage := "ktunnel-sidecar:latest"
	testCases := []struct {
		name     string
		spec     v1.PodSpec
		expected bool
	}{
		{
			name: "sidecar present",
			spec: v1.PodSpec{
				Containers: []v1.Container{
					{Name: "main-app", Image: "main-app:v1"},
					{Name: "ktunnel", Image: sidecarImage},
				},
			},
			expected: true,
		},
		{
			name: "sidecar not present",
			spec: v1.PodSpec{
				Containers: []v1.Container{
					{Name: "main-app", Image: "main-app:v1"},
				},
			},
			expected: false,
		},
		{
			name:     "no containers",
			spec:     v1.PodSpec{Containers: []v1.Container{}},
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if result := hasSidecar(tc.spec, sidecarImage); result != tc.expected {
				t.Errorf("Expected %v, got %v", tc.expected, result)
			}
		})
	}
}

// Test_getDeploymentCondition checks the getDeploymentCondition function.
func Test_getDeploymentCondition(t *testing.T) {
	progressingCondition := appsv1.DeploymentCondition{
		Type:   appsv1.DeploymentProgressing,
		Status: v1.ConditionTrue,
		Reason: "NewReplicaSetAvailable",
	}
	availableCondition := appsv1.DeploymentCondition{
		Type:   appsv1.DeploymentAvailable,
		Status: v1.ConditionTrue,
	}
	status := appsv1.DeploymentStatus{
		Conditions: []appsv1.DeploymentCondition{progressingCondition, availableCondition},
	}

	t.Run("condition exists", func(t *testing.T) {
		cond := getDeploymentCondition(status, appsv1.DeploymentProgressing)
		if cond == nil {
			t.Fatal("Expected to find condition, but got nil")
		}
		if cond.Reason != "NewReplicaSetAvailable" {
			t.Errorf("Expected reason 'NewReplicaSetAvailable', got '%s'", cond.Reason)
		}
	})

	t.Run("condition does not exist", func(t *testing.T) {
		cond := getDeploymentCondition(status, appsv1.DeploymentReplicaFailure)
		if cond != nil {
			t.Error("Expected not to find condition, but got one")
		}
	})
}

// Test_getPodsFilteredByLabel checks the getPodsFilteredByLabel function.
func Test_getPodsFilteredByLabel(t *testing.T) {
	namespace := "test"
	pod1 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: namespace, Labels: map[string]string{"app": "test-app"}}}
	pod2 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: namespace, Labels: map[string]string{"app": "another-app"}}}
	pod3 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod3", Namespace: namespace, Labels: map[string]string{"app": "test-app"}}}

	fakeClient := fake.NewSimpleClientset(pod1, pod2, pod3)
	kubeSvc := &KubeService{
		clients: &Clients{Pods: fakeClient.CoreV1().Pods(namespace)},
	}

	pods, err := kubeSvc.getPodsFilteredByLabel("app=test-app")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(pods.Items) != 2 {
		t.Errorf("Expected 2 pods, got %d", len(pods.Items))
	}

	podNames := map[string]bool{}
	for _, p := range pods.Items {
		podNames[p.Name] = true
	}
	if !podNames["pod1"] || !podNames["pod3"] {
		t.Errorf("Did not get the expected pods. Got: %v", podNames)
	}
}

// Test_getPodNames checks the getPodNames function.
func Test_getPodNames(t *testing.T) {
	namespace := "test"
	deploymentName := "test-deployment"
	labels := map[string]string{
		deploymentNameLabel:     deploymentName,
		deploymentInstanceLabel: deploymentName,
	}
	now := time.Now()

	pod1 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1-older", Namespace: namespace, Labels: labels, CreationTimestamp: metav1.Time{Time: now.Add(-1 * time.Hour)}}}
	pod2 := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod2-newest", Namespace: namespace, Labels: labels, CreationTimestamp: metav1.Time{Time: now}}}
	pod3 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod3-running", Namespace: namespace, Labels: labels, CreationTimestamp: metav1.Time{Time: now.Add(-30 * time.Minute)}},
		Status:     v1.PodStatus{Phase: v1.PodRunning},
	}
	pod4 := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod4-pending", Namespace: namespace, Labels: labels, CreationTimestamp: metav1.Time{Time: now.Add(-1 * time.Minute)}},
		Status:     v1.PodStatus{Phase: v1.PodPending},
	}

	// Set pods to running so they're picked up
	pod1.Status.Phase = v1.PodRunning
	pod2.Status.Phase = v1.PodRunning

	fakeClient := fake.NewSimpleClientset(pod1, pod2, pod3, pod4)
	kubeSvc := &KubeService{
		clients: &Clients{Pods: fakeClient.CoreV1().Pods(namespace)},
	}

	podNames := make([]string, 3)
	err := kubeSvc.getPodNames(deploymentName, podNames)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Expected order is newest to oldest of RUNNING pods
	expectedOrder := []string{"pod2-newest", "pod3-running", "pod1-older"}
	for i, name := range podNames {
		if name != expectedOrder[i] {
			t.Errorf("Expected pod name '%s' at index %d, but got '%s'. Full list: %v", expectedOrder[i], i, name, podNames)
		}
	}
}

// Test_getPortForwardURL consolidates tests for getPortForwardURL.
func Test_getPortForwardURL(t *testing.T) {
	testCases := []struct {
		name      string
		config    rest.Config
		namespace string
		pod       string
		expected  *url.URL
	}{
		{
			name: "standard api server",
			config: rest.Config{
				Host: "https://api.qa.kube.com",
			},
			namespace: "default",
			pod:       "test",
			expected: &url.URL{
				Scheme: "https",
				Host:   "api.qa.kube.com",
				Path:   "/api/v1/namespaces/default/pods/test/portforward",
			},
		},
		{
			name: "rancher proxy",
			config: rest.Config{
				Host: "https://rancher.xyz.io/k8s/clusters/c-wfdqx",
			},
			namespace: "default",
			pod:       "test",
			expected: &url.URL{
				Scheme: "https",
				Host:   "rancher.xyz.io",
				Path:   "/k8s/clusters/c-wfdqx/api/v1/namespaces/default/pods/test/portforward",
			},
		},
		{
			name: "ip and port host",
			config: rest.Config{
				Host: "https://srv01.mydomain.de:6443",
			},
			pod:       "myapp-5b65c8777b-dd54r",
			namespace: "default",
			expected: &url.URL{
				Scheme: "https",
				Host:   "srv01.mydomain.de:6443",
				Path:   "/api/v1/namespaces/default/pods/myapp-5b65c8777b-dd54r/portforward",
			},
		},
		{
			name: "http host",
			config: rest.Config{
				Host: "http://localhost:8080",
			},
			pod:       "test-pod",
			namespace: "kube-system",
			expected: &url.URL{
				Scheme: "http",
				Host:   "localhost:8080",
				Path:   "/api/v1/namespaces/kube-system/pods/test-pod/portforward",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res := getPortForwardURL(&tc.config, tc.namespace, tc.pod)
			if res.String() != tc.expected.String() {
				t.Errorf("expected: %v, got: %v", tc.expected, res)
			}
		})
	}
}

func Test_newContainer(t *testing.T) {
	testCases := []struct {
		name           string
		port           int
		image          string
		containerPorts []v1.ContainerPort
		cert           string
		key            string
		cReq           int64
		cLimit         int64
		mReq           int64
		mLimit         int64
		verbose        bool
		expectedArgs   []string
	}{
		{
			name:           "basic container",
			port:           8080,
			image:          "test-image:latest",
			containerPorts: []v1.ContainerPort{},
			cert:           "",
			key:            "",
			cReq:           100,
			cLimit:         500,
			mReq:           100,
			mLimit:         1000,
			verbose:        false,
			expectedArgs:   []string{"server", "-p", "8080"},
		},
		{
			name:           "container with certs",
			port:           8443,
			image:          "test-image:latest",
			containerPorts: []v1.ContainerPort{},
			cert:           "/certs/tls.crt",
			key:            "/certs/tls.key",
			cReq:           100,
			cLimit:         500,
			mReq:           100,
			mLimit:         1000,
			verbose:        false,
			expectedArgs:   []string{"server", "-p", "8443", "--cert /certs/tls.crt", "--key /certs/tls.key"},
		},
		{
			name:           "verbose container",
			port:           8080,
			image:          "test-image:latest",
			containerPorts: []v1.ContainerPort{},
			cert:           "",
			key:            "",
			cReq:           100,
			cLimit:         500,
			mReq:           100,
			mLimit:         1000,
			verbose:        true,
			expectedArgs:   []string{"server", "-p", "8080", "-v"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.verbose {
				SetVerbose(true)
			}

			container := newContainer(tc.port, tc.image, tc.containerPorts, tc.cert, tc.key, tc.cReq, tc.cLimit, tc.mReq, tc.mLimit)

			if container.Name != "ktunnel" {
				t.Errorf("Expected container name to be 'ktunnel', got %s", container.Name)
			}
			if container.Image != tc.image {
				t.Errorf("Expected image %s, got %s", tc.image, container.Image)
			}
			if len(container.Args) != len(tc.expectedArgs) {
				t.Errorf("Expected %d args, got %d", len(tc.expectedArgs), len(container.Args))
			}
			for i, arg := range tc.expectedArgs {
				if container.Args[i] != arg {
					t.Errorf("Expected arg %s at position %d, got %s", arg, i, container.Args[i])
				}
			}
			// Reset verbose flag for other tests
			SetVerbose(false)
		})
	}
}

func Test_newDeployment(t *testing.T) {
	testCases := []struct {
		name           string
		namespace      string
		deploymentName string
		port           int
		image          string
		ports          []v1.ContainerPort
		selector       map[string]string
		labels         map[string]string
		annotations    map[string]string
		tolerations    []v1.Toleration
		cert           string
		key            string
		cpuReq         int64
		cpuLimit       int64
		memReq         int64
		memLimit       int64
	}{
		{
			name:           "basic deployment",
			namespace:      "default",
			deploymentName: "test-deployment",
			port:           8080,
			image:          "test-image:latest",
			ports:          []v1.ContainerPort{},
			selector:       map[string]string{"node": "test"},
			labels:         map[string]string{"app": "test"},
			annotations:    map[string]string{"note": "test"},
			tolerations:    []v1.Toleration{},
			cert:           "",
			key:            "",
			cpuReq:         100,
			cpuLimit:       500,
			memReq:         100,
			memLimit:       1000,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			deployment := newDeployment(
				tc.namespace,
				tc.deploymentName,
				tc.port,
				tc.image,
				tc.ports,
				tc.selector,
				tc.labels,
				tc.annotations,
				tc.tolerations,
				tc.cert,
				tc.key,
				tc.cpuReq,
				tc.cpuLimit,
				tc.memReq,
				tc.memLimit,
			)

			if deployment.Name != tc.deploymentName {
				t.Errorf("Expected deployment name %s, got %s", tc.deploymentName, deployment.Name)
			}
			if deployment.Namespace != tc.namespace {
				t.Errorf("Expected namespace %s, got %s", tc.namespace, deployment.Namespace)
			}
			if *deployment.Spec.Replicas != int32(1) {
				t.Errorf("Expected 1 replica, got %d", *deployment.Spec.Replicas)
			}
			if len(deployment.Spec.Template.Spec.Containers) != 1 {
				t.Errorf("Expected 1 container, got %d", len(deployment.Spec.Template.Spec.Containers))
			}
		})
	}
}

func Test_newService(t *testing.T) {
	testCases := []struct {
		name      string
		namespace string
		svcName   string
		ports     []v1.ServicePort
		svcType   v1.ServiceType
	}{
		{
			name:      "basic service",
			namespace: "default",
			svcName:   "test-service",
			ports: []v1.ServicePort{
				{
					Name:     "http",
					Port:     80,
					Protocol: v1.ProtocolTCP,
				},
			},
			svcType: v1.ServiceTypeClusterIP,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			service := newService(tc.namespace, tc.svcName, tc.ports, tc.svcType)

			if service.Name != tc.svcName {
				t.Errorf("Expected service name %s, got %s", tc.svcName, service.Name)
			}
			if service.Namespace != tc.namespace {
				t.Errorf("Expected namespace %s, got %s", tc.namespace, service.Namespace)
			}
			if service.Spec.Type != tc.svcType {
				t.Errorf("Expected service type %s, got %s", tc.svcType, service.Spec.Type)
			}
			if len(service.Spec.Ports) != len(tc.ports) {
				t.Errorf("Expected %d ports, got %d", len(tc.ports), len(service.Spec.Ports))
			}
		})
	}
}

func Test_deploymentStatus(t *testing.T) {
	testCases := []struct {
		name          string
		deployment    *appsv1.Deployment
		expectedMsg   string
		expectedDone  bool
		expectedError bool
	}{
		{
			name: "successful rollout",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-deployment",
					Generation: 1,
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					Replicas:           1,
					UpdatedReplicas:    1,
					AvailableReplicas:  1,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: new(int32),
				},
			},
			expectedMsg:   "deployment \"test-deployment\" successfully rolled out\n",
			expectedDone:  true,
			expectedError: false,
		},
		{
			name: "waiting for update",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-deployment",
					Generation: 2,
				},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
				},
			},
			expectedMsg:   "Waiting for deployment spec update to be observed...\n",
			expectedDone:  false,
			expectedError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			msg, done, err := deploymentStatus(tc.deployment)

			if msg != tc.expectedMsg {
				t.Errorf("Expected message %q, got %q", tc.expectedMsg, msg)
			}
			if done != tc.expectedDone {
				t.Errorf("Expected done to be %v, got %v", tc.expectedDone, done)
			}
			if (err != nil) != tc.expectedError {
				t.Errorf("Expected error to be %v, got %v", tc.expectedError, err != nil)
			}
		})
	}
}

func Test_ByCreationTime(t *testing.T) {
	now := time.Now()
	pods := ByCreationTime{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "pod1",
				CreationTimestamp: metav1.Time{Time: now.Add(-1 * time.Hour)},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "pod2",
				CreationTimestamp: metav1.Time{Time: now},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "pod3",
				CreationTimestamp: metav1.Time{Time: now.Add(-2 * time.Hour)},
			},
		},
	}

	// Test Len
	if pods.Len() != 3 {
		t.Errorf("Expected length 3, got %d", pods.Len())
	}

	// Test Less (should return true if i is newer than j)
	if !pods.Less(1, 0) {
		t.Error("Expected pod2 to be newer than pod1")
	}
	if !pods.Less(0, 2) {
		t.Error("Expected pod1 to be newer than pod3")
	}

	// Test sorting
	sorted := make(ByCreationTime, len(pods))
	copy(sorted, pods)
	sort.Sort(sorted)

	// After sorting, pods should be ordered from newest to oldest
	if sorted[0].Name != "pod2" || sorted[1].Name != "pod1" || sorted[2].Name != "pod3" {
		t.Errorf("Incorrect sort order: got %v, %v, %v", sorted[0].Name, sorted[1].Name, sorted[2].Name)
	}
}
