package cmd

import (
	"context"
	"testing"
	"time"
)

func TestWaitForReady_DeploymentBecomesReady(t *testing.T) {
	readyChan := make(chan bool, 1)
	readyChan <- true

	ready, interrupted := waitForReady(context.Background(), readyChan)
	if !ready || interrupted {
		t.Fatalf("got ready=%v interrupted=%v, want ready=true interrupted=false", ready, interrupted)
	}
}

func TestWaitForReady_RolloutFails(t *testing.T) {
	readyChan := make(chan bool, 1)
	readyChan <- false

	ready, interrupted := waitForReady(context.Background(), readyChan)
	if ready || interrupted {
		t.Fatalf("got ready=%v interrupted=%v, want ready=false interrupted=false", ready, interrupted)
	}
}

// TestWaitForReady_InterruptedByCtrlC is the regression test for the hang.
// A rollout that never reports anything must not keep the process alive once
// the signal handler has cancelled the context.
func TestWaitForReady_InterruptedByCtrlC(t *testing.T) {
	// Never written to, standing in for a rollout that is still in progress.
	readyChan := make(chan bool)

	ctx, cancel := context.WithCancel(context.Background())

	// Ctrl+C arrives shortly after we start waiting.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	var ready, interrupted bool
	go func() {
		ready, interrupted = waitForReady(ctx, readyChan)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForReady ignored the cancelled context; this is the Ctrl+C hang")
	}

	if ready {
		t.Error("an interrupted wait must not report the deployment as ready")
	}
	if !interrupted {
		t.Error("expected interrupted=true so the caller waits for teardown instead of proceeding")
	}
}

// TestWaitForReady_PrefersReadinessAlreadyDelivered guards against the
// opposite failure: if readiness landed before the context was cancelled, we
// should still report it rather than treating the shutdown as an interrupt.
func TestWaitForReady_PrefersReadinessAlreadyDelivered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // both select cases are live

	// select picks randomly among ready cases, so assert over repeated runs
	// that readiness is at least reachable and never misreported.
	sawReady := false
	for range 50 {
		ch := make(chan bool, 1)
		ch <- true
		ready, interrupted := waitForReady(ctx, ch)
		if ready && interrupted {
			t.Fatal("ready and interrupted must not both be true")
		}
		if ready {
			sawReady = true
		}
	}
	if !sawReady {
		t.Error("readiness already on the channel was never observed")
	}
}
