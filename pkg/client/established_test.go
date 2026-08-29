package client_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/client"
	"github.com/omrikiei/ktunnel/pkg/common"
)

// These use the harness in loopback_test.go: a real server and a real client
// in one process, over real TCP.

// startClientWith runs RunClient with extra options and returns its result
// channel and its cancel function.
func startClientWith(t *testing.T, grpcPort int, tunnels []string, extra ...client.Option) (<-chan error, context.CancelFunc) {
	t.Helper()

	clientLog, _ := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	opts := append([]client.Option{
		client.WithServer("127.0.0.1", grpcPort),
		client.WithLogger(clientLog),
		client.WithTunnels("tcp", tunnels...),
	}, extra...)

	errCh := make(chan error, 1)
	go func() { errCh <- client.RunClient(ctx, opts...) }()
	return errCh, cancel
}

// waitFor polls cond until it holds, and fails with consequence if it never
// does.
func waitFor(t *testing.T, timeout time.Duration, consequence string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(consequence)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunClient_ReportsWhenItsTunnelsAreOpen covers the signal a supervisor
// needs and cannot get from a return value: RunClient does not return while a
// tunnel is working, so "it is up" has to be pushed out.
//
// It matters that this fires: an attempt that never reports itself established
// never becomes stable, so its backoff keeps doubling and its failure streak
// never clears. A tunnel that flaps once an hour would creep to the maximum
// retry delay and stay there.
func TestRunClient_ReportsWhenItsTunnelsAreOpen(t *testing.T) {
	echoPort := startEchoServer(t)
	grpcPort := freePort(t)
	startServer(t, grpcPort)

	var established atomic.Int32
	tunnels := []string{
		fmt.Sprintf("%d:127.0.0.1:%d", freePort(t), echoPort),
		fmt.Sprintf("%d:127.0.0.1:%d", freePort(t), echoPort),
	}
	// No session store: RunClient makes its own when it is not given one.
	_, cancel := startClientWith(t, grpcPort, tunnels,
		client.WithEstablishedCallback(func() { established.Add(1) }),
	)

	waitFor(t, 20*time.Second,
		"RunClient never reported its tunnels established; the supervisor above it would treat a working tunnel as one that never came up, and back off further after every blip",
		func() bool { return established.Load() > 0 })

	// Give any straggler a chance to report, so the count below means
	// something.
	time.Sleep(500 * time.Millisecond)
	if got := established.Load(); got != 1 {
		t.Fatalf("RunClient reported established %d times for 2 tunnels, want exactly 1; "+
			"reporting per tunnel announces a client that is serving only some of the ports it was asked for", got)
	}

	cancel()
}

// TestRunClient_DoesNotReportEstablishedWhenItCannotConnect is the other half:
// a report that fires regardless is worth nothing to a supervisor, which uses
// it to decide a failed attempt was actually a working one.
func TestRunClient_DoesNotReportEstablishedWhenItCannotConnect(t *testing.T) {
	echoPort := startEchoServer(t)
	// Nothing is listening here, and nothing ever will be.
	grpcPort := freePort(t)

	var established atomic.Int32
	errCh, _ := startClientWith(t, grpcPort,
		[]string{fmt.Sprintf("%d:127.0.0.1:%d", freePort(t), echoPort)},
		client.WithEstablishedCallback(func() { established.Add(1) }),
	)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("RunClient returned nil when it could not reach the server")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunClient never returned when it could not reach the server")
	}

	if got := established.Load(); got != 0 {
		t.Fatalf("RunClient reported established %d time(s) for a server it never reached; "+
			"the supervisor would reset its backoff and hammer an unreachable cluster", got)
	}
}

// TestRunClient_ClosesItsSessionsWhenItReturns is the other half of the
// reconnect leak. Each attempt gets a fresh session store, which drops the
// bookkeeping -- but the sockets belong to the operating system, not to the
// map, and nothing was closing them.
func TestRunClient_ClosesItsSessionsWhenItReturns(t *testing.T) {
	echoPort := startEchoServer(t)
	grpcPort := freePort(t)
	startServer(t, grpcPort)
	tunnelPort := freePort(t)

	store := common.NewSessionStore()
	errCh, cancel := startClientWith(t, grpcPort,
		[]string{fmt.Sprintf("%d:127.0.0.1:%d", tunnelPort, echoPort)},
		client.WithSessionStore(store),
	)

	// A round trip is what creates a session on the client side.
	if got, want := roundTrip(t, tunnelPort, "hello"), "HELLO"; got != want {
		t.Fatalf("round trip returned %q, want %q -- no session was ever opened, so this test would pass without proving anything", got, want)
	}
	waitFor(t, 10*time.Second, "the client never registered a session for a connection it served",
		func() bool { return store.Len() > 0 })

	cancel()
	select {
	case <-errCh:
	case <-time.After(30 * time.Second):
		t.Fatal("RunClient did not return after its context was cancelled")
	}

	if got := store.Len(); got != 0 {
		t.Fatalf("RunClient left %d session(s) open after returning; with a supervisor rebuilding the tunnel, "+
			"that is a socket and a goroutine leaked per session per reconnect", got)
	}
}
