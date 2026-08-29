package k8s

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	testclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// TestPortForward_FailedAttemptsDoNotLeakGoroutines is the regression test for
// an unbounded leak on exactly the path reconnecting exercises.
//
// client-go closes its ready channel only after a successful dial *and* a
// successful listen. The two most common reconnect failures -- the API server
// unreachable, the local port not yet released by the previous attempt --
// return before that, so the channel is never closed. The goroutine waiting on
// it blocked forever, and so did the one waiting on the WaitGroup it was
// supposed to release: two goroutines per failed attempt, retained for the
// life of the process. A laptop left overnight on a dead VPN accumulates
// thousands.
func TestPortForward_FailedAttemptsDoNotLeakGoroutines(t *testing.T) {
	// Nothing is listening here, so every SPDY dial fails immediately.
	dead := freeLocalPort(t)

	name := "leaky"
	fake := testclient.NewSimpleClientset(
		oneReplicaDeployment(name),
		labelledPod(name, name+"-7d9f-x2m1", v1.PodRunning),
	)

	clientMutex.Lock()
	deploymentsClient = fake.AppsV1().Deployments("default")
	clientMutex.Unlock()

	svc := &KubeService{
		clients: &Clients{Pods: fake.CoreV1().Pods("default")},
		config:  &rest.Config{Host: fmt.Sprintf("http://127.0.0.1:%d", dead)},
	}

	const attempts = 20

	// One attempt first, so that anything started once -- lazily initialised
	// clients, background workers -- is already running when the baseline is
	// taken and does not read as a leak.
	failOnce(t, svc, name)
	before := settledGoroutines()

	for range attempts {
		failOnce(t, svc, name)
	}

	after := settledGoroutines()
	delta := after - before
	t.Logf("%d failed attempts: %d goroutines before, %d after (delta %d, %.1f per attempt)",
		attempts, before, after, delta, float64(delta)/float64(attempts))

	// A small drift is normal; two per attempt is the bug.
	if delta > attempts {
		t.Fatalf("%d failed port forwards left %d goroutines behind (%.1f per attempt); "+
			"every reconnect against an unreachable API server or a port that is still held leaks another pair, for the life of the process",
			attempts, delta, float64(delta)/float64(attempts))
	}
}

// failOnce runs one port-forward attempt that cannot succeed, and releases it
// the way the caller does: by closing the stop channel.
func failOnce(t *testing.T, svc *KubeService, deployment string) {
	t.Helper()

	stopChan := make(chan struct{})
	_, _, err := svc.PortForward(context.Background(), "default", deployment, "28688", stopChan)
	if err == nil {
		t.Fatal("the port forward reported success against an API server that is not there")
	}
	close(stopChan)
}

// settledGoroutines waits for the goroutine count to stop moving, so that
// goroutines still unwinding are not counted as leaked.
func settledGoroutines() int {
	last := runtime.NumGoroutine()
	stable := 0
	for i := 0; i < 100; i++ {
		time.Sleep(20 * time.Millisecond)
		now := runtime.NumGoroutine()
		if now == last {
			if stable++; stable == 3 {
				return now
			}
			continue
		}
		last, stable = now, 0
	}
	return last
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed reserving a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("failed releasing reserved port: %v", err)
	}
	return port
}
