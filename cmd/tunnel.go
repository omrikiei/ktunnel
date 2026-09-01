// Package cmd implements the command line interface for ktunnel
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/omrikiei/ktunnel/pkg/client"
	"github.com/omrikiei/ktunnel/pkg/creds"
	"github.com/omrikiei/ktunnel/pkg/k8s"
	"github.com/omrikiei/ktunnel/pkg/supervisor"
	"github.com/spf13/cobra"
)

// ExitOnDisconnect and MaxReconnectAttempts back the reconnect flags. The
// defaults keep the interactive behaviour users expect -- a tunnel that just
// keeps working -- while letting process supervisors opt into an exit code.
var (
	ExitOnDisconnect     bool
	MaxReconnectAttempts int
)

func addReconnectFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&ExitOnDisconnect, "exit-on-disconnect", false, "Exit with a non-zero status the first time the tunnel drops, instead of reconnecting")
	cmd.Flags().IntVar(&MaxReconnectAttempts, "max-reconnect-attempts", 0, "Give up after this many consecutive failed connection attempts; 0 keeps retrying forever")
}

// newSupervisor builds the supervisor described by the reconnect flags.
func newSupervisor(attempt supervisor.Attempt) *supervisor.Supervisor {
	return &supervisor.Supervisor{
		Attempt:     attempt,
		MaxAttempts: MaxReconnectAttempts,
		ExitOnFirst: ExitOnDisconnect,
		Log:         &logger,
	}
}

// tunnelSession is the shutdown plumbing expose and inject share: a context
// the tunnel runs under, a signal handler that cancels it, and a teardown of
// the cluster resources that runs exactly once however the command ends.
//
// The teardown runs on the goroutine that owns the session rather than on the
// signal handler, so it happens after the supervisor has returned and every
// forward and stream is already down. That ordering used to be arranged by
// hand with a wait group and a `done` channel that each early-exit path had to
// remember to read from; getting it wrong left the command hanging.
type tunnelSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	once     sync.Once
	teardown func()
}

// newTunnelSession starts watching for signals and returns the session. reason
// is what to log when one arrives; teardown may be nil for a command that
// created nothing in the cluster.
func newTunnelSession(ctx context.Context, cancel context.CancelFunc, reason string, teardown func()) *tunnelSession {
	s := &tunnelSession{ctx: ctx, cancel: cancel, teardown: teardown}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		select {
		case <-sigs:
			logger.Info(reason)
			// Hand signals back to the runtime, so a second Ctrl+C kills the
			// process outright. Every signal after the first used to be
			// swallowed, which left a user watching a slow teardown -- or an
			// attempt stuck in a call to an unreachable API server -- with no
			// way out but another terminal.
			signal.Stop(sigs)
			if teardown != nil {
				logger.Info("press Ctrl+C again to exit without cleaning up")
			}
			s.cancel()
		case <-ctx.Done():
			signal.Stop(sigs)
		}
	}()

	return s
}

// finish cancels the session and tears down whatever it created. It is
// idempotent, so it can be deferred for the early-return paths and still be
// called explicitly before os.Exit, which skips deferred functions.
func (s *tunnelSession) finish() {
	s.once.Do(func() {
		s.cancel()
		if s.teardown != nil {
			s.teardown()
		}
	})
}

// supervise runs attempt until the user stops it or the give-up policy is
// reached, and then ends the process: 0 for Ctrl+C, 1 for a tunnel that ended
// on its own.
//
// It does not return, so it is the last thing a command does and no deferred
// function above it runs -- which is why the teardown lives in
// superviseAndReport rather than being left to the caller's defer.
func supervise(sess *tunnelSession, attempt supervisor.Attempt) {
	os.Exit(superviseAndReport(sess, attempt))
}

// superviseAndReport is supervise without the exit, so that the ordering that
// protects the cluster resources -- tear down first, decide the exit code
// after -- is something a test can assert rather than something os.Exit hides.
func superviseAndReport(sess *tunnelSession, attempt supervisor.Attempt) int {
	err := newSupervisor(attempt).Run(sess.ctx)
	// Before returning a code the caller exits on: os.Exit runs no deferred
	// functions, and the resources this created are still in the cluster.
	sess.finish()
	if err != nil {
		// The error already says what was given up on and after how many
		// attempts; this line says what the process is about to do about it.
		logger.WithError(err).Error("exiting")
		return 1
	}
	return 0
}

// sessionCredentials is what this run generated, and how much of it the
// in-cluster server can actually use.
//
// A package-level value, like the flags around it: it is set once, before the
// supervisor starts, and read on every reconnect attempt -- a reconnect has
// to present the same credentials as the first connection, because the pod on
// the other end is still running with them.
type sessionCredentials struct {
	bundle *creds.Bundle
	// encrypted is false for `inject`, whose sidecar gets a token but no
	// certificate: the client must authenticate over plaintext there, and
	// turning TLS on would fail the handshake instead.
	encrypted bool
}

var tunnelCreds sessionCredentials

// clientOptions returns the client half of these credentials.
func (s sessionCredentials) clientOptions() []client.Option {
	if s.bundle == nil {
		return nil
	}
	opts := []client.Option{client.WithToken(s.bundle.Token)}
	if s.encrypted {
		// The CA never touches disk: it was generated moments ago and lives
		// only in this process, which is why a SIGKILL leaves nothing
		// behind to clean up.
		opts = append(opts, client.WithTLSFromPEM(s.bundle.CACert, ""))
	}
	return opts
}

// tunnelClientOptions builds the client configuration shared by every command.
func tunnelClientOptions(host string, grpcPort int, tunnels []string, established func()) []client.Option {
	opts := []client.Option{
		client.WithServer(host, grpcPort),
		client.WithTunnels(Scheme, tunnels...),
		client.WithLogger(&logger),
		// No session store is set: RunClient makes its own, and these options
		// are built once per attempt, so a reconnect never inherits sessions
		// keyed by IDs the new server has never issued.
		client.WithEstablishedCallback(established),
	}
	if tls && CaFile != "" {
		// Bring-your-own CA, on the standalone client and on the
		// expose/inject runs that passed --ca-file instead of letting
		// ktunnel generate credentials.
		opts = append(opts, client.WithTLS(CaFile, ServerHostOverride))
	}
	return append(opts, tunnelCreds.clientOptions()...)
}

// tunnelClientAttempt returns an Attempt that runs a tunnel client against an
// already-reachable server and blocks until its tunnels stop carrying traffic.
// This is the whole of `ktunnel client`: there is no forward underneath it.
func tunnelClientAttempt(host string, grpcPort int, tunnels []string) supervisor.Attempt {
	return func(ctx context.Context, established func()) error {
		return client.RunClient(ctx, tunnelClientOptions(host, grpcPort, tunnels, established)...)
	}
}

// portForwarder is the seam k8s.KubeService sits behind, so that a forwarding
// attempt's teardown ordering -- release on every return path, clients stopped
// before the forward -- can be tested without an API server behind it. The one
// production implementation is *k8s.KubeService.
type portForwarder interface {
	PortForward(ctx context.Context, kind k8s.WorkloadKind, namespace, name, targetPort string, stopChan <-chan struct{}) ([]string, <-chan error, error)
}

// clientRunner runs one tunnel client over a forwarded local port and blocks
// until it stops carrying traffic. The other half of the same seam: the
// ordering being asserted is between these returning and the forward being
// released, and a real gRPC client makes that a race to observe rather than a
// fact to assert.
type clientRunner func(ctx context.Context, localPort int, tunnels []string, established func()) error

// runTunnelClient is the production clientRunner.
func runTunnelClient(ctx context.Context, localPort int, tunnels []string, established func()) error {
	return client.RunClient(ctx, tunnelClientOptions(Host, localPort, tunnels, established)...)
}

// forwardAndTunnelAttempt returns an Attempt that rebuilds the entire local
// half of the tunnel: it re-resolves the tunnel server's pods, forwards a
// local port to each of them, and runs a tunnel client over every forward. It
// reports itself established once every one of those clients has its streams
// open, and returns as soon as any layer fails.
//
// Pod names are resolved on every attempt rather than captured once, because a
// port-forward is bound to the pod name it was built with. A rescheduled pod
// has a different name, so reconnecting only the gRPC stream would rebuild a
// tunnel to a pod that no longer exists -- which is the case #114 is actually
// about, and would have looked like a fix while failing it.
//
// Cluster resources are never recreated here. If the deployment has been
// deleted, resolving it fails and that is one failed attempt like any other;
// silently recreating what the user removed is not a recovery path's business.
func forwardAndTunnelAttempt(svc portForwarder, kind k8s.WorkloadKind, namespace, name string, remotePort int, tunnels []string) supervisor.Attempt {
	return forwardAndTunnel(svc, runTunnelClient, kind, namespace, name, remotePort, tunnels)
}

// forwardAndTunnel is forwardAndTunnelAttempt with both of its dependencies
// passed in.
func forwardAndTunnel(svc portForwarder, run clientRunner, kind k8s.WorkloadKind, namespace, name string, remotePort int, tunnels []string) supervisor.Attempt {
	strPort := strconv.FormatInt(int64(remotePort), 10)

	label := fmt.Sprintf("%s %s/%s port %s", kind, namespace, name, strPort)

	return func(ctx context.Context, established func()) error {
		// Per attempt: a stop channel that has already been closed would tear
		// the new forward down the instant it was created.
		stopChan := make(chan struct{})

		sourcePorts, fwdErrChan, err := svc.PortForward(ctx, kind, namespace, name, strPort, stopChan)
		forward := watchForward(stopChan, fwdErrChan, forwardReleaseTimeout, label)
		defer forward.release(ctx)

		if err != nil {
			return fmt.Errorf("failed forwarding to %s %s/%s: %w", kind, namespace, name, err)
		}

		// A workload scaled to zero has no pods to forward to, and
		// PortForward reports that as success with no ports rather than as
		// an error.
		if len(sourcePorts) == 0 {
			// Nothing to run a client over. Without this the attempt would
			// block on a select no goroutine can ever wake, and the
			// supervisor -- which waits for the attempt -- would never
			// retry: a hang that reads as a healthy tunnel.
			return fmt.Errorf("no running pods to forward to for %s %s/%s", kind, namespace, name)
		}

		// The clients share a context of their own so that the first failure
		// brings the rest down with it: a client serving some of the replicas
		// is not a working tunnel, and the whole of it is about to be rebuilt.
		clientCtx, endClients := context.WithCancel(ctx)
		clients := &sync.WaitGroup{}
		defer func() {
			endClients()
			// Waiting rather than walking away, so a reconnect does not
			// accumulate a set of clients per attempt. This waits for
			// RunClient to return, not for every goroutine it started --
			// those unwind on their own once its gRPC connection closes, and
			// none of them holds the forwarded local port.
			clients.Wait()
		}()

		// established fires when the last client reports its streams open, so
		// a partly-connected tunnel is never announced as up.
		// int64 rather than int32: widening from len()'s int cannot overflow.
		var pending atomic.Int64
		pending.Store(int64(len(sourcePorts)))
		up := func() {
			if pending.Add(-1) == 0 {
				established()
			}
		}

		clientErrs := make(chan error, len(sourcePorts))
		for _, srcPort := range sourcePorts {
			localPort, err := strconv.Atoi(srcPort)
			if err != nil {
				return fmt.Errorf("failed to parse the forwarded local port %q: %w", srcPort, err)
			}
			clients.Add(1)
			go func() {
				defer clients.Done()
				clientErrs <- run(clientCtx, localPort, tunnels, up)
			}()
		}

		select {
		case <-ctx.Done():
			return nil
		case err := <-forward.failed:
			return err
		case err := <-clientErrs:
			// RunClient returns nil only when its context was cancelled, and
			// clientCtx outlives this select unless ctx was cancelled -- which
			// the case above would have caught. Reported either way, since a
			// tunnel that ended is a tunnel that ended.
			return err
		}
	}
}

// forwardReleaseTimeout bounds how long an attempt waits for its forwarders to
// let go of their local ports.
//
// Generous, because waiting is the whole point and a forwarder past its dial
// returns within milliseconds. It exists because one is not guaranteed to be
// past its dial: client-go's ForwardPorts does not look at stopChan until the
// SPDY dial completes, and spdy.RoundTripperFor builds a round tripper with no
// dialer we can put a deadline on. Waiting forever on that would park the
// Attempt, and Supervisor.Run waits for the Attempt, so the command would hang
// -- the failure class this feature exists to remove. Giving up costs at worst
// a retry that cannot bind, which is loud.
const forwardReleaseTimeout = 30 * time.Second

// forwardHandle owns one attempt's port-forward: the first failure it saw, and
// the release that has to complete before another attempt may bind the same
// local port.
type forwardHandle struct {
	// failed carries the first post-startup forwarder failure, if any. A
	// forward that dies takes the tunnel above it with it.
	failed <-chan error
	// release stops the forwards and waits for them to let go of their local
	// ports, for the reason given on watchForward. It is idempotent, and
	// bounded by forwardReleaseTimeout. Its context is the attempt's: once
	// that is cancelled there is no next attempt whose bind this is
	// protecting, so it stops waiting.
	release func(ctx context.Context)
}

// watchForward takes ownership of a port-forward's error channel.
//
// There is exactly one reader, whether or not startup succeeded: PortForward
// hands the channel back even when it fails, because forwarders it already
// launched are holding local ports either way. The first error ends the
// attempt; the rest are drained so that no forwarder is left blocked on a send
// nobody is taking.
//
// The channel closing is the point of all this. PortForward closes it once
// every forwarder has returned, and client-go's ForwardPorts closes its
// listeners before returning, so waiting for that close is how an attempt
// knows the local port is bindable again. An attempt that returns without
// waiting hands the next one a race against its own dying forwarder, and the
// loser gets "address already in use" -- forever, because every attempt after
// it loses the same race. That failure mode looks exactly like a feature that
// works: the reconnect logs scroll past, and nothing ever reconnects.
// label names the forward in the one message this can log, which fires exactly
// when the user is about to see "address already in use" and is therefore the
// line they will grep.
func watchForward(stopChan chan struct{}, fwdErrChan <-chan error, releaseTimeout time.Duration, label string) *forwardHandle {
	failed := make(chan error, 1)
	forwardersDone := make(chan struct{})
	if fwdErrChan == nil {
		// PortForward failed before launching anything, so there is nothing
		// holding a port and nothing to wait for.
		close(forwardersDone)
	} else {
		go func() {
			defer close(forwardersDone)
			for err := range fwdErrChan {
				select {
				case failed <- err:
				default:
				}
			}
		}()
	}

	var stopOnce sync.Once
	return &forwardHandle{
		failed: failed,
		release: func(ctx context.Context) {
			stopOnce.Do(func() { close(stopChan) })
			select {
			case <-forwardersDone:
			case <-ctx.Done():
				// Shutting down. There is no next attempt whose bind this
				// wait is protecting, and the user asked to be let go.
			case <-time.After(releaseTimeout):
				logger.Warnf("the port forward to %s has not let go of its local port after %s; "+
					"the next connection attempt may fail to bind it", label, releaseTimeout)
			}
		},
	}
}

// noteDeprecatedTLS accepts --tls on the in-cluster commands and does
// nothing with it.
//
// Through v2.3 these commands rejected the flag outright, because there was
// nothing behind it: no volume mounted a certificate into the tunnel server
// container, and --cert/--key reached it as a single unparsed argument cobra
// never split. v2.4 provisions credentials per run and turns TLS on with no
// flag at all, so the flag now asks for what is already happening. Refusing
// it would break every command line written against that error message, and
// honouring it would imply the default is something less.
func noteDeprecatedTLS(*cobra.Command, []string) error {
	if tls {
		logger.Warn("--tls is deprecated and does nothing: expose and inject encrypt the tunnel by default. " +
			"Use --insecure to turn encryption and authentication off.")
	}
	return nil
}

// generateCredentials mints the credentials for one run, or returns nil when
// the user asked for none.
//
// --cert/--key mean the user brought their own server credentials, and
// --ca-file means they will verify against their own CA; in that case nothing
// is generated and the files are used as given.
func generateCredentials(name, namespace string) (*creds.Bundle, error) {
	if Insecure {
		logger.Warn("--insecure: the tunnel is unencrypted and unauthenticated. " +
			"Anything in the cluster that can reach it can reach this machine.")
		return nil, nil
	}
	if CertFile != "" || KeyFile != "" || CaFile != "" {
		// Bring-your-own. Generating a bundle as well would put two
		// certificates in play and mount the wrong one.
		return nil, nil
	}
	bundle, err := creds.Generate(name, namespace)
	if err != nil {
		return nil, fmt.Errorf("failed generating tunnel credentials: %w\n"+
			"pass --insecure to run without them", err)
	}
	return bundle, nil
}
