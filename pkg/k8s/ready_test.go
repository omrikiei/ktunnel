package k8s

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testclient "k8s.io/client-go/kubernetes/fake"
)

// readyDeployment builds a Deployment whose status already reports a completed
// rollout, i.e. deploymentStatus() would call it ready.
func readyDeployment(name string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Generation: 1,
			Labels: map[string]string{
				deploymentNameLabel:     name,
				deploymentInstanceLabel: name,
			},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}
}

// pendingDeployment builds a Deployment that is mid-rollout.
func pendingDeployment(name string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "default",
			Generation: 1,
			Labels: map[string]string{
				deploymentNameLabel:     name,
				deploymentInstanceLabel: name,
			},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    0,
			AvailableReplicas:  0,
		},
	}
}

// TestWatchForReady_AlreadyReady covers the case that made `ktunnel expose`
// hang on "waiting for deployment to be ready": the deployment finished its
// rollout before we started observing it. A watch only delivers events that
// happen after it is established, so nothing ever arrived and the caller
// blocked until the progress deadline expired.
func TestWatchForReady_AlreadyReady(t *testing.T) {
	name := "already-ready"
	fakeClient := testclient.NewSimpleClientset(readyDeployment(name))

	clientMutex.Lock()
	deploymentsClient = fakeClient.AppsV1().Deployments("default")
	clientMutex.Unlock()

	readyChan := make(chan bool, 1)
	watchForReady(readyDeployment(name), readyChan)

	select {
	case ready := <-readyChan:
		if !ready {
			t.Fatal("expected the deployment to be reported ready, got false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for readiness; the rollout completed before " +
			"we started observing it and was never noticed")
	}
}

// TestWatchForReady_BecomesReady covers the ordinary case: the deployment is
// still rolling out when we start observing, and becomes ready later.
func TestWatchForReady_BecomesReady(t *testing.T) {
	name := "becomes-ready"
	fakeClient := testclient.NewSimpleClientset(pendingDeployment(name))

	clientMutex.Lock()
	deploymentsClient = fakeClient.AppsV1().Deployments("default")
	clientMutex.Unlock()

	readyChan := make(chan bool, 1)
	watchForReady(pendingDeployment(name), readyChan)

	// Flip the deployment to ready shortly after we start observing.
	go func() {
		time.Sleep(500 * time.Millisecond)
		_, err := fakeClient.AppsV1().Deployments("default").
			UpdateStatus(context.Background(), readyDeployment(name), metav1.UpdateOptions{})
		if err != nil {
			t.Errorf("failed updating deployment status: %v", err)
		}
	}()

	select {
	case ready := <-readyChan:
		if !ready {
			t.Fatal("expected the deployment to be reported ready, got false")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the deployment to become ready")
	}
}

// TestWatchForReady_ProgressDeadlineExceeded asserts that a failed rollout is
// reported as not-ready rather than hanging.
func TestWatchForReady_ProgressDeadlineExceeded(t *testing.T) {
	name := "stuck"
	d := pendingDeployment(name)
	d.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentProgressing,
		Status: v1.ConditionFalse,
		Reason: "ProgressDeadlineExceeded",
	}}
	fakeClient := testclient.NewSimpleClientset(d)

	clientMutex.Lock()
	deploymentsClient = fakeClient.AppsV1().Deployments("default")
	clientMutex.Unlock()

	readyChan := make(chan bool, 1)
	watchForReady(d, readyChan)

	select {
	case ready := <-readyChan:
		if ready {
			t.Fatal("expected a stuck rollout to be reported not ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out; a rollout past its progress deadline should report failure")
	}
}

// TestWatchForReady_DeploymentGone asserts that a deployment which disappears
// out from under us reports not-ready instead of polling forever.
func TestWatchForReady_DeploymentGone(t *testing.T) {
	name := "vanished"
	fakeClient := testclient.NewSimpleClientset()

	clientMutex.Lock()
	deploymentsClient = fakeClient.AppsV1().Deployments("default")
	clientMutex.Unlock()

	readyChan := make(chan bool, 1)
	watchForReady(pendingDeployment(name), readyChan)

	select {
	case ready := <-readyChan:
		if ready {
			t.Fatal("expected a missing deployment to be reported not ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out; a missing deployment %q should report failure", name)
	}
}
