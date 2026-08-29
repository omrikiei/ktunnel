package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/server"
	"github.com/omrikiei/ktunnel/pkg/supervisor"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// --- flags ------------------------------------------------------------------

// TestReconnectFlagsAreRegistered pins the flags onto the commands that need
// them. Every command that opens a tunnel has to offer the give-up policy, or
// the users this feature is for -- the ones running ktunnel under a process
// supervisor -- are back to wrapper scripts on whichever command they use.
func TestReconnectFlagsAreRegistered(t *testing.T) {
	commands := map[string]*cobra.Command{
		"expose":            exposeCmd,
		"inject deployment": injectDeploymentCmd,
		"client":            clientCmd,
	}
	defaults := map[string]string{
		"exit-on-disconnect":     "false",
		"max-reconnect-attempts": "0",
	}

	for name, cmd := range commands {
		for flag, want := range defaults {
			f := cmd.Flags().Lookup(flag)
			if f == nil {
				t.Errorf("ktunnel %s has no --%s flag", name, flag)
				continue
			}
			if f.DefValue != want {
				t.Errorf("ktunnel %s --%s defaults to %q, want %q; "+
					"the defaults have to preserve today's behaviour of reconnecting quietly forever", name, flag, f.DefValue, want)
			}
		}
	}
}

// withReconnectFlags sets the flag variables for the duration of a test.
func withReconnectFlags(t *testing.T, exitOnDisconnect bool, maxAttempts int) {
	t.Helper()
	prevExit, prevMax := ExitOnDisconnect, MaxReconnectAttempts
	t.Cleanup(func() { ExitOnDisconnect, MaxReconnectAttempts = prevExit, prevMax })
	ExitOnDisconnect, MaxReconnectAttempts = exitOnDisconnect, maxAttempts
}

// TestNewSupervisor_CarriesTheFlags is the wiring the flags are worth nothing
// without: a --exit-on-disconnect that never reaches the supervisor is a flag
// that silently does the opposite of what it says.
func TestNewSupervisor_CarriesTheFlags(t *testing.T) {
	withReconnectFlags(t, true, 7)

	s := newSupervisor(func(context.Context, func()) error { return nil })

	if !s.ExitOnFirst {
		t.Error("--exit-on-disconnect did not reach the supervisor; the tunnel would reconnect anyway and the process supervisor above it would never be told")
	}
	if s.MaxAttempts != 7 {
		t.Errorf("--max-reconnect-attempts=7 reached the supervisor as %d; ktunnel would retry for a different number of attempts than the user asked for", s.MaxAttempts)
	}
	if s.Attempt == nil {
		t.Error("the supervisor was built with nothing to run")
	}
	if s.Log == nil {
		t.Error("the supervisor was built without a logger; the reconnect state transitions are what replace the users' log-grepping wrapper scripts")
	}
}

// TestNewSupervisor_DefaultsToRetryingForever states the default in the terms
// a user experiences it.
func TestNewSupervisor_DefaultsToRetryingForever(t *testing.T) {
	withReconnectFlags(t, false, 0)

	s := newSupervisor(func(context.Context, func()) error { return nil })

	if s.ExitOnFirst {
		t.Error("the default exits on the first disconnection; an interactive ktunnel would die on a momentary blip")
	}
	if s.MaxAttempts != 0 {
		t.Errorf("the default gives up after %d attempts; it must retry forever", s.MaxAttempts)
	}
}

// --- exit codes -------------------------------------------------------------

// TestSuperviseAndReport_TearsDownBeforeReportingAnExitCode is the invariant
// that keeps a give-up from orphaning a Deployment and a Service in someone's
// cluster. supervise exits the process on the code this returns, and os.Exit
// runs no deferred functions -- so the teardown cannot be left to the caller's
// defer, and has to have happened by the time the code is handed back.
func TestSuperviseAndReport_TearsDownBeforeReportingAnExitCode(t *testing.T) {
	quietLogger(t)
	withReconnectFlags(t, true, 0)

	ctx, cancel := context.WithCancel(context.Background())
	var teardowns atomic.Int32
	sess := newTunnelSession(ctx, cancel, "stopping", func() { teardowns.Add(1) })

	code := superviseAndReport(sess, func(context.Context, func()) error {
		return errors.New("tunnel from source port 8000 failed")
	})

	if got := teardowns.Load(); got != 1 {
		t.Errorf("the session was torn down %d times before the exit code was reported, want 1; "+
			"the process exits on this code without running any deferred function, so the deployment and service stay in the user's cluster", got)
	}
	if code != 1 {
		t.Errorf("a tunnel that ended on its own reported exit code %d, want 1; "+
			"a process supervisor reads that code and would not restart it", code)
	}
}

// TestSuperviseAndReport_CleanShutdownExitsZero keeps the other half: Ctrl+C is
// not a failure, and must not look like one to whatever started ktunnel.
func TestSuperviseAndReport_CleanShutdownExitsZero(t *testing.T) {
	quietLogger(t)
	withReconnectFlags(t, false, 0)

	ctx, cancel := context.WithCancel(context.Background())
	var teardowns atomic.Int32
	sess := newTunnelSession(ctx, cancel, "stopping", func() { teardowns.Add(1) })

	running := make(chan struct{})
	go func() {
		<-running
		cancel()
	}()

	code := superviseAndReport(sess, func(ctx context.Context, established func()) error {
		established()
		close(running)
		<-ctx.Done()
		return nil
	})

	if code != 0 {
		t.Errorf("Ctrl+C reported exit code %d, want 0; a process supervisor would restart a tunnel the user closed on purpose", code)
	}
	if got := teardowns.Load(); got != 1 {
		t.Errorf("the session was torn down %d times on a clean shutdown, want 1", got)
	}
}

// --- session ----------------------------------------------------------------

// TestTunnelSession_FinishTearsDownExactlyOnce guards the property every exit
// path depends on. finish is deferred and also called explicitly before
// os.Exit, and the signal handler cancels underneath both.
func TestTunnelSession_FinishTearsDownExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var teardowns atomic.Int32
	sess := newTunnelSession(ctx, cancel, "stopping", func() { teardowns.Add(1) })

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.finish()
		}()
	}
	wg.Wait()

	if got := teardowns.Load(); got != 1 {
		t.Fatalf("teardown ran %d times, want 1; deleting the same deployment twice reports a failure the user did not cause", got)
	}
	if sess.ctx.Err() == nil {
		t.Error("finish left the session's context live, so whatever is running under it never stops")
	}
}

// TestTunnelSession_FinishBlocksUntilTeardownCompletes is why sync.Once is the
// right primitive here and a "already done" flag is not: the caller about to
// exit has to wait for a teardown another goroutine started, or the process
// leaves before the cluster resources are gone.
func TestTunnelSession_FinishBlocksUntilTeardownCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var done atomic.Bool
	sess := newTunnelSession(ctx, cancel, "stopping", func() {
		close(started)
		<-release
		done.Store(true)
	})

	first := make(chan struct{})
	go func() {
		sess.finish()
		close(first)
	}()

	// The second call must start after the first has claimed the once, or it
	// would be testing nothing. Waiting for the teardown itself to begin is
	// what makes that ordering a fact rather than a likely outcome.
	<-started

	second := make(chan struct{})
	go func() {
		sess.finish()
		close(second)
	}()

	select {
	case <-second:
		t.Fatal("finish returned while the teardown it was waiting on was still running; the process would exit leaving its deployment behind")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	<-first
	<-second
	if !done.Load() {
		t.Fatal("the teardown never completed")
	}
}

// TestTunnelSession_NilTeardownDoesNotPanic is a smoke test, and only that:
// `ktunnel client` creates nothing in the cluster and so supplies no teardown,
// and finish must not call through the nil.
func TestTunnelSession_NilTeardownDoesNotPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	newTunnelSession(ctx, cancel, "stopping", nil).finish()
}

// --- releasing the forwarded local port -------------------------------------

// TestWatchForward_ReleaseWaitsForTheForwardersToLetGo pins the single
// ordering property the whole feature rests on: the forward owns a local port
// and the next attempt binds the same one. See watchForward for what an early
// return costs.
func TestWatchForward_ReleaseWaitsForTheForwardersToLetGo(t *testing.T) {
	stopChan := make(chan struct{})
	fwdErrChan := make(chan error)
	forward := watchForward(stopChan, fwdErrChan, time.Minute, "default/proxy port 28688")

	released := make(chan struct{})
	go func() {
		forward.release(context.Background())
		close(released)
	}()

	// Closing stopChan is what stops the forwarders at all: client-go's
	// ForwardPorts blocks on it, and closes its listeners on the way out.
	select {
	case <-stopChan:
	case <-time.After(5 * time.Second):
		t.Fatal("release never closed the forward's stop channel, so the forwarder keeps running and keeps hold of the local port")
	}

	select {
	case <-released:
		t.Fatal("release returned while a forwarder was still running; the next attempt races it for the local port and gets \"address already in use\", and so does every attempt after that")
	case <-time.After(200 * time.Millisecond):
	}

	// The last forwarder has returned -- which, in client-go, it only does
	// after closing its listener. PortForward closes the channel to say so.
	close(fwdErrChan)

	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("release never returned after every forwarder was gone; the supervisor waits for the attempt, so no reconnect would ever be attempted")
	}
}

// TestWatchForward_ReleaseIsIdempotent: release is deferred and, on some
// paths, reached twice. Closing a closed channel panics.
func TestWatchForward_ReleaseIsIdempotent(t *testing.T) {
	fwdErrChan := make(chan error)
	close(fwdErrChan)
	forward := watchForward(make(chan struct{}), fwdErrChan, time.Minute, "default/proxy port 28688")

	forward.release(context.Background())
	forward.release(context.Background())
}

// TestWatchForward_ReleaseWithNoForwarders covers the startup failures that
// never launched one -- a deployment that has been deleted, for instance.
// PortForward returns a nil channel there, and an attempt must not wait for a
// close that will never come.
func TestWatchForward_ReleaseWithNoForwarders(t *testing.T) {
	forward := watchForward(make(chan struct{}), nil, time.Minute, "default/proxy port 28688")

	done := make(chan struct{})
	go func() {
		forward.release(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("release blocked forever when no forwarder had been launched; a failed attempt would never end and the supervisor would never retry it")
	}
}

// TestWatchForward_ReportsTheFirstFailureAndDrainsTheRest: a forward that dies
// takes its tunnel with it, so the attempt has to hear about it. The
// forwarders that follow have to be drained rather than heard, or they block
// on a send and never release their ports.
func TestWatchForward_ReportsTheFirstFailureAndDrainsTheRest(t *testing.T) {
	fwdErrChan := make(chan error, 3)
	forward := watchForward(make(chan struct{}), fwdErrChan, time.Minute, "default/proxy port 28688")

	first := errors.New("port forward to pod proxy-7d9f-x2m1 failed")
	fwdErrChan <- first
	fwdErrChan <- errors.New("port forward to pod proxy-7d9f-b4k2 failed")
	fwdErrChan <- errors.New("port forward to pod proxy-7d9f-q8p0 failed")

	select {
	case got := <-forward.failed:
		if !errors.Is(got, first) {
			t.Fatalf("reported %v, want the first failure %v", got, first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a forward that died was never reported; the tunnel above it is dead and the attempt would sit there believing it is fine")
	}

	close(fwdErrChan)

	done := make(chan struct{})
	go func() {
		forward.release(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("release blocked after the remaining failures went unread; a forwarder stuck on a send never returns, and never lets go of its local port")
	}
}

// TestWatchForward_ReleaseGivesUpRatherThanHanging: client-go's ForwardPorts
// does not look at the stop channel until after its SPDY dial, and that dial
// has no deadline we can set from here. A forwarder stuck in one never
// returns, so an unbounded wait would park the Attempt -- and Supervisor.Run
// waits for the Attempt, so the tunnel would hang with no way back.
func TestWatchForward_ReleaseGivesUpRatherThanHanging(t *testing.T) {
	quietLogger(t)

	// Never closed, standing in for a forwarder stuck in a dial.
	forward := watchForward(make(chan struct{}), make(chan error), 50*time.Millisecond, "default/proxy port 28688")

	done := make(chan struct{})
	go func() {
		forward.release(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("release never gave up on a forwarder that will not return; the attempt never ends, so the supervisor never retries and Ctrl+C does not get through either")
	}
}

// TestWatchForward_ReleaseStopsWaitingOnShutdown: on Ctrl+C there is no next
// attempt whose bind the wait is protecting, and the user has asked to be let
// go.
func TestWatchForward_ReleaseStopsWaitingOnShutdown(t *testing.T) {
	quietLogger(t)

	stopChan := make(chan struct{})
	forward := watchForward(stopChan, make(chan error), time.Hour, "default/proxy port 28688")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		forward.release(ctx)
		close(done)
	}()

	// The forwarders are still told to stop, whether or not we wait for them.
	select {
	case <-stopChan:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not stop the forwarders before waiting on them")
	}

	select {
	case <-done:
		t.Fatal("release returned before it was either cancelled or timed out; a normal failure would stop waiting for the local port, which is the wait the whole retry loop depends on")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("release kept waiting for a stuck forwarder after the user asked to shut down; Ctrl+C hangs")
	}
}

// --- the forwarding attempt's teardown ordering -----------------------------

// fakeForwarder stands in for k8s.KubeService. It records the stop channel it
// was handed, so a test can watch the forward being released, and hands back an
// error channel the test closes when it wants the forwarders to count as gone.
type fakeForwarder struct {
	sourcePorts *[]string
	err         error
	errChan     chan error

	mu       sync.Mutex
	stopChan <-chan struct{}
}

func newFakeForwarder(ports []string, err error) *fakeForwarder {
	f := &fakeForwarder{err: err, errChan: make(chan error, 1)}
	if ports != nil {
		f.sourcePorts = &ports
	}
	return f
}

func (f *fakeForwarder) PortForward(ctx context.Context, namespace, deployment, targetPort string, stopChan <-chan struct{}) (*[]string, <-chan error, error) {
	f.mu.Lock()
	f.stopChan = stopChan
	f.mu.Unlock()
	return f.sourcePorts, f.errChan, f.err
}

// awaitRelease waits for the attempt to close the forward's stop channel,
// which is the first thing release does and the only thing that stops a real
// forwarder.
func (f *fakeForwarder) awaitRelease(t *testing.T, timeout time.Duration, consequence string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		stop := f.stopChan
		f.mu.Unlock()
		if stop != nil {
			select {
			case <-stop:
				return
			default:
			}
		}
		if time.Now().After(deadline) {
			t.Fatal(consequence)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestForwardAndTunnel_ReleasesTheForwardWhenItFailsToStart pins the release
// onto the failure path too. PortForward hands back its error channel even
// when it fails, because forwarders it already launched hold local ports
// either way.
func TestForwardAndTunnel_ReleasesTheForwardWhenItFailsToStart(t *testing.T) {
	quietLogger(t)

	startupErr := errors.New("no running pods for deployment proxy")
	forwarder := newFakeForwarder(nil, startupErr)
	attempt := forwardAndTunnel(forwarder, unusedClientRunner(t), "default", "proxy", 28688, []string{"8000"})

	done := make(chan error, 1)
	go func() { done <- attempt(context.Background(), func() {}) }()

	forwarder.awaitRelease(t, 5*time.Second,
		"a port-forward that failed to start was never released; whatever it had already launched keeps its local port, and every retry after this one fails to bind")

	select {
	case <-done:
		t.Fatal("the attempt returned while its forwarders were still unwinding; the next attempt races them for the local port")
	case <-time.After(100 * time.Millisecond):
	}

	close(forwarder.errChan)
	select {
	case err := <-done:
		if !errors.Is(err, startupErr) {
			t.Fatalf("the attempt returned %v, which does not carry the reason the forward failed (%v)", err, startupErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the attempt never returned once its forwarders were gone; the supervisor waits for the attempt, so no retry would ever happen")
	}
}

// TestForwardAndTunnel_StopsTheClientsBeforeReleasingTheForward is the
// ordering the whole teardown rests on, and the one a reordering of the two
// deferred calls would silently break.
//
// The clients run over the forwarded ports. Releasing the forward from under
// a client that is still running tears down the thing it is talking through,
// and leaves that client to be waited for afterwards -- with the local port
// already reported as free.
func TestForwardAndTunnel_StopsTheClientsBeforeReleasingTheForward(t *testing.T) {
	quietLogger(t)

	forwarder := newFakeForwarder([]string{"18000", "18001"}, nil)

	// One client fails immediately, which is what ends the attempt. The other
	// serves until it is cancelled, which is what the teardown has to wait
	// for.
	clientFailure := errors.New("tunnel from source port 8000 failed")
	blockingClientReturned := make(chan struct{})
	run := func(ctx context.Context, localPort int, tunnels []string, established func()) error {
		if localPort == 18000 {
			return clientFailure
		}
		<-ctx.Done()
		close(blockingClientReturned)
		return nil
	}

	attempt := forwardAndTunnel(forwarder, run, "default", "proxy", 28688, []string{"8000"})
	done := make(chan error, 1)
	go func() { done <- attempt(context.Background(), func() {}) }()

	forwarder.awaitRelease(t, 5*time.Second, "the forward was never released after the tunnel failed; its local port stays held for the life of the process")

	select {
	case <-blockingClientReturned:
	default:
		t.Fatal("the forward was released while a tunnel client was still running over it; " +
			"the client is torn down by the forward disappearing underneath it, and the local port is reported free before the client that used it has gone")
	}

	close(forwarder.errChan)
	select {
	case err := <-done:
		if !errors.Is(err, clientFailure) {
			t.Fatalf("the attempt returned %v, want the client failure %v", err, clientFailure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the attempt never returned once its forwarders were gone")
	}
}

// TestForwardAndTunnel_NilPortsIsAnError covers the deployment scaled to zero.
// PortForward can report success with a nil port list, and the emptiness check
// that exists for exactly this case was the thing dereferencing it.
func TestForwardAndTunnel_NilPortsIsAnError(t *testing.T) {
	quietLogger(t)

	forwarder := newFakeForwarder(nil, nil)
	close(forwarder.errChan) // nothing was ever launched

	attempt := forwardAndTunnel(forwarder, unusedClientRunner(t), "default", "proxy", 28688, []string{"8000"})

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("the attempt panicked (%v) on a deployment with no pods to forward to; "+
					"inside the retry loop that is a crash that recurs, on a deployment someone merely scaled down", r)
				done <- nil
			}
		}()
		done <- attempt(context.Background(), func() {})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the attempt reported success with no pods to forward to; it would block on a select nothing can wake, and the supervisor waits for the attempt")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the attempt never returned with no pods to forward to; the tunnel hangs instead of being retried")
	}
}

// TestForwardAndTunnel_ReleasesTheForwardOnShutdown: Ctrl+C is the one case
// where waiting for the port is pointless -- there is no next attempt whose
// bind it protects -- but the forwarders must still be told to stop.
func TestForwardAndTunnel_ReleasesTheForwardOnShutdown(t *testing.T) {
	quietLogger(t)

	forwarder := newFakeForwarder([]string{"18002"}, nil)
	run := func(ctx context.Context, localPort int, tunnels []string, established func()) error {
		established()
		<-ctx.Done()
		return nil
	}

	attempt := forwardAndTunnel(forwarder, run, "default", "proxy", 28688, []string{"8000"})
	ctx, cancel := context.WithCancel(context.Background())

	established := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- attempt(ctx, func() { established <- struct{}{} })
	}()

	select {
	case <-established:
	case <-time.After(5 * time.Second):
		t.Fatal("the attempt never reported itself established with every client up")
	}

	// The forwarders are deliberately never reported as gone, standing in for
	// one stuck in a dial that cannot be cancelled.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the attempt reported %v on shutdown; Ctrl+C would exit non-zero and the supervisor would treat it as a tunnel to rebuild", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the attempt did not return on shutdown while a forwarder was stuck; Ctrl+C hangs, which is the failure class this feature exists to remove")
	}

	forwarder.awaitRelease(t, 5*time.Second, "the forwarders were never told to stop on shutdown")
}

// unusedClientRunner fails the test if the attempt reaches the client layer.
func unusedClientRunner(t *testing.T) clientRunner {
	t.Helper()
	return func(ctx context.Context, localPort int, tunnels []string, established func()) error {
		t.Errorf("a tunnel client was started on port %d although the forward never came up", localPort)
		return nil
	}
}

// --- the client command's attempt -------------------------------------------

// TestTunnelClientAttempt_ReconnectsAfterTheConnectionDrops is the feature, end
// to end, over real TCP and with the closure the `client` command actually
// runs: a tunnel that loses its connection comes back on its own, and carries
// traffic again, without anybody restarting ktunnel.
//
// The forward-rebuilding attempt that expose and inject use cannot be
// exercised without a real API server -- that gap is recorded in the design
// doc -- but everything above the forward is the same in all three commands.
func TestTunnelClientAttempt_ReconnectsAfterTheConnectionDrops(t *testing.T) {
	quietLogger(t)
	withReconnectFlags(t, false, 0)

	echoPort := startEchoServer(t)
	grpcPort := freePort(t)
	startTunnelServer(t, grpcPort)
	proxyPort, cut := startCuttableProxy(t, grpcPort)
	tunnelPort := freePort(t)

	var established atomic.Int32
	attempt := tunnelClientAttempt("127.0.0.1", proxyPort, []string{fmt.Sprintf("%d:127.0.0.1:%d", tunnelPort, echoPort)})
	counted := func(ctx context.Context, report func()) error {
		return attempt(ctx, func() {
			established.Add(1)
			report()
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup := newSupervisor(counted)
	// Retrying on the scale a test can wait for. The sequence itself is the
	// supervisor package's to test.
	sup.Backoff = supervisor.Backoff{Base: 50 * time.Millisecond, Max: 200 * time.Millisecond}
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()

	if got, want := roundTripEventually(t, tunnelPort, "hello", 30*time.Second), "HELLO"; got != want {
		t.Fatalf("round trip returned %q, want %q -- the tunnel was not up before the test broke it", got, want)
	}
	before := established.Load()

	// The network goes away: sockets vanish with nobody saying goodbye.
	cut()

	// The assertion that matters is not that we reconnected but that the
	// tunnel works again; a re-established stream over a server that never
	// let go of its listener would carry nothing.
	if got, want := roundTripEventually(t, tunnelPort, "again", 60*time.Second), "AGAIN"; got != want {
		t.Fatalf("round trip returned %q, want %q after the connection was cut -- this is #114: the tunnel never comes back and the user restarts ktunnel by hand", got, want)
	}
	if after := established.Load(); after <= before {
		t.Fatalf("the tunnel reported itself established %d time(s), the same as before the connection was cut; "+
			"a reconnect that is never reported never resets the backoff, so the delays keep doubling on a link that recovers", after)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("the supervisor reported %v after Ctrl+C; the command would exit non-zero on a shutdown the user asked for", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor did not return after its context was cancelled; Ctrl+C would hang")
	}
}

// TestTunnelClientAttempt_GivesUpWhenAskedTo covers --max-reconnect-attempts
// reaching the behaviour it names, against a server that is never there.
func TestTunnelClientAttempt_GivesUpWhenAskedTo(t *testing.T) {
	quietLogger(t)
	withReconnectFlags(t, false, 3)

	echoPort := startEchoServer(t)
	// Nothing is listening here, and nothing ever will be.
	grpcPort := freePort(t)

	var attempts atomic.Int32
	attempt := tunnelClientAttempt("127.0.0.1", grpcPort, []string{fmt.Sprintf("%d:127.0.0.1:%d", freePort(t), echoPort)})
	counted := func(ctx context.Context, report func()) error {
		attempts.Add(1)
		return attempt(ctx, report)
	}

	sup := newSupervisor(counted)
	sup.Backoff = supervisor.Backoff{Base: 10 * time.Millisecond, Max: 50 * time.Millisecond}

	done := make(chan error, 1)
	go func() { done <- sup.Run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("giving up returned nil, so the command would exit 0; a process supervisor would treat a tunnel that never connected as a clean shutdown and not restart it")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("--max-reconnect-attempts=3 never gave up against a server that does not exist")
	}

	if got := attempts.Load(); got != 3 {
		t.Fatalf("made %d attempts, want 3", got)
	}
}

// --- helpers ----------------------------------------------------------------

// quietLogger keeps the command's own logger out of the test output.
//
// Through SetOutput rather than the field, because tunnel goroutines from this
// test are still logging when the cleanup restores it, and only SetOutput
// takes the lock that logrus writes under.
func quietLogger(t *testing.T) {
	t.Helper()
	prev := logger.Out
	t.Cleanup(func() { logger.SetOutput(prev) })
	logger.SetOutput(io.Discard)
}

func freePort(t *testing.T) int {
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

// startEchoServer stands in for the service on the developer's machine.
func startEchoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed starting echo server: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 1024)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write([]byte(strings.ToUpper(string(buf[:n])))); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

// startTunnelServer runs the real tunnel server, standing in for the pod.
func startTunnelServer(t *testing.T, grpcPort int) {
	t.Helper()

	quiet := log.New()
	quiet.SetOutput(io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = server.RunServer(ctx, server.WithPort(grpcPort), server.WithLogger(quiet))
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", grpcPort), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tunnel server never started listening on %d: %v", grpcPort, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// startCuttableProxy forwards TCP to targetPort. cut drops every connection it
// is carrying, which is what a lost network looks like from the client's side:
// the socket goes away with nobody saying goodbye. It keeps listening
// afterwards, so a reconnect has something to come back to.
func startCuttableProxy(t *testing.T, targetPort int) (port int, cut func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed starting proxy: %v", err)
	}

	var mu sync.Mutex
	var conns []net.Conn
	stopped := false

	cut = func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		conns = nil
	}

	t.Cleanup(func() {
		mu.Lock()
		stopped = true
		_ = ln.Close()
		mu.Unlock()
		cut()
	})

	go func() {
		for {
			downstream, err := ln.Accept()
			if err != nil {
				return
			}
			upstream, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", targetPort))
			if err != nil {
				_ = downstream.Close()
				continue
			}
			mu.Lock()
			if stopped {
				mu.Unlock()
				_ = downstream.Close()
				_ = upstream.Close()
				return
			}
			conns = append(conns, downstream, upstream)
			mu.Unlock()
			go func() { _, _ = io.Copy(upstream, downstream) }()
			go func() { _, _ = io.Copy(downstream, upstream) }()
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port, cut
}

// roundTripEventually keeps trying a full round trip until one succeeds. A
// reconnect is not instant -- the server has to notice its stream is gone and
// release the listener before a new attempt can bind it -- so a single attempt
// would be testing the timing rather than the recovery.
func roundTripEventually(t *testing.T, tunnelPort int, payload string, timeout time.Duration) string {
	t.Helper()

	addr := fmt.Sprintf("127.0.0.1:%d", tunnelPort)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		got, err := roundTripOnce(addr, payload)
		if err == nil {
			return got
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("no round trip through %s succeeded within %s: %v", addr, timeout, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func roundTripOnce(addr, payload string) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte(payload)); err != nil {
		return "", err
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return "", err
	}
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
