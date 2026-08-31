package k8s

import (
	"sort"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_newContainer(t *testing.T) {
	testCases := []struct {
		name           string
		port           int
		image          string
		containerPorts []v1.ContainerPort
		podCreds       PodCredentials
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
			podCreds:       PodCredentials{},
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
			podCreds:       PodCredentials{SecretName: "myapp-ktunnel"},
			cReq:           100,
			cLimit:         500,
			mReq:           100,
			mLimit:         1000,
			verbose:        false,
			// This case used to expect `--cert /certs/tls.crt` as a single
			// argv entry, which is what the code emitted and what made
			// in-cluster TLS impossible. The test agreed with the bug, so it
			// never caught it.
			expectedArgs: []string{"server", "-p", "8443", "--tls",
				"--cert", certMountPath + "/tls.crt", "--key", certMountPath + "/tls.key"},
		},
		{
			name:           "verbose container",
			port:           8080,
			image:          "test-image:latest",
			containerPorts: []v1.ContainerPort{},
			podCreds:       PodCredentials{},
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
			// verbose is package-level state that newContainer reads, so it
			// has to be set for every case and restored afterwards. Setting
			// it only for the verbose case leaked into subsequent runs of the
			// same binary, which made `go test -count=2` fail with a stray
			// -v argument -- and made the flag unusable for flake detection.
			SetVerbose(tc.verbose)
			t.Cleanup(func() { SetVerbose(false) })

			container := newContainer(tc.port, tc.image, tc.containerPorts, tc.podCreds, tc.cReq, tc.cLimit, tc.mReq, tc.mLimit)

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
		podCreds       PodCredentials
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
			podCreds:       PodCredentials{},
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
				tc.podCreds,
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
			service := newService(tc.namespace, tc.svcName, tc.ports, tc.svcType, nil)

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
