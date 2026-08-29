package client_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/client"
	"github.com/omrikiei/ktunnel/pkg/common"
	"github.com/omrikiei/ktunnel/pkg/server"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// These tests run a real tunnel server and a real tunnel client in one
// process, over real TCP, and push bytes through end to end. They are the
// regression net for the tunnel data path, which had no test coverage at all.
//
// Running both sides in one process is only possible because each holds its
// own SessionStore. The two sides address sessions by the same UUIDs, so with
// a single shared store each would find the other's session instead of its
// own.

// freePort returns a TCP port that was free a moment ago. Inherently racy, but
// adequate for tests and the only option when the code under test binds a port
// number rather than accepting a listener.
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

// startEchoServer stands in for the service running on the developer's
// machine. It echoes back whatever it is sent, upper-cased, so the test can
// tell the echo apart from its own request.
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

// testLogger returns a logger that captures entries for assertions.
func testLogger() (*log.Logger, *logtest.Hook) {
	l := log.New()
	l.SetOutput(io.Discard)
	l.SetLevel(log.DebugLevel)
	hook := logtest.NewLocal(l)
	return l, hook
}

// dialUntilReady retries until something is listening, or the deadline passes.
func dialUntilReady(t *testing.T, addr string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing listening on %s after %s: %v", addr, timeout, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// tunnel is a running server/client pair, plus the handles a test needs to
// break it and to see what the client made of that.
type tunnel struct {
	serverHook *logtest.Hook
	clientHook *logtest.Hook
	// clientErr receives RunClient's return value when it returns. It never
	// receives twice, so a test may leave it unread.
	clientErr <-chan error
	// stop cancels the client's context, the way Ctrl+C does.
	stop func()
	// cut drops the TCP connections between client and server without
	// closing anything gracefully, the way a dropped VPN does. The proxy
	// keeps listening, so a reconnect can come back through it. Only set by
	// startTunnelThroughProxy.
	cut func()
	// closeProxy stops the proxy accepting at all, so a reconnect finds
	// nothing there. Only set by startTunnelThroughProxy.
	closeProxy func()
}

// startTunnel brings up a server and a client wired to each other, tunnelling
// tunnelPort on the "cluster" side to targetPort on the "local" side.
func startTunnel(t *testing.T, tunnelPort, targetPort int) *tunnel {
	t.Helper()

	grpcPort := freePort(t)
	serverHook := startServer(t, grpcPort)
	tn := startClient(t, grpcPort, tunnelPort, targetPort)
	tn.serverHook = serverHook
	return tn
}

// startTunnelThroughProxy is startTunnel with a cuttable TCP proxy between the
// two sides, so a test can take the network away from underneath the client.
func startTunnelThroughProxy(t *testing.T, tunnelPort, targetPort int) *tunnel {
	t.Helper()

	grpcPort := freePort(t)
	serverHook := startServer(t, grpcPort)
	proxyPort, cut, closeProxy := startCuttableProxy(t, grpcPort)
	tn := startClient(t, proxyPort, tunnelPort, targetPort)
	tn.serverHook = serverHook
	tn.cut = cut
	tn.closeProxy = closeProxy
	return tn
}

func startServer(t *testing.T, grpcPort int) *logtest.Hook {
	t.Helper()

	serverLog, serverHook := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_ = server.RunServer(ctx,
			server.WithPort(grpcPort),
			server.WithLogger(serverLog),
			server.WithSessionStore(common.NewSessionStore()),
		)
	}()

	// Wait for the gRPC listener before pointing anything at it.
	conn := dialUntilReady(t, fmt.Sprintf("127.0.0.1:%d", grpcPort), 5*time.Second)
	_ = conn.Close()

	return serverHook
}

func startClient(t *testing.T, grpcPort, tunnelPort, targetPort int) *tunnel {
	t.Helper()

	clientLog, clientHook := testLogger()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	clientErr := make(chan error, 1)
	go func() {
		clientErr <- client.RunClient(ctx,
			client.WithServer("127.0.0.1", grpcPort),
			client.WithLogger(clientLog),
			client.WithTunnels("tcp", fmt.Sprintf("%d:127.0.0.1:%d", tunnelPort, targetPort)),
			client.WithSessionStore(common.NewSessionStore()),
		)
	}()

	return &tunnel{clientHook: clientHook, clientErr: clientErr, stop: cancel}
}

// startCuttableProxy forwards TCP to targetPort. cut drops every connection
// it is carrying, which is what a lost network looks like from the client's
// side: the socket goes away with nobody saying goodbye. The proxy keeps
// listening afterwards, so that a reconnect has something to come back to --
// closeProxy is the separate, harsher case where it does not.
func startCuttableProxy(t *testing.T, targetPort int) (port int, cut func(), closeProxy func()) {
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

	closeProxy = func() {
		mu.Lock()
		stopped = true
		_ = ln.Close()
		mu.Unlock()
		cut()
	}
	t.Cleanup(closeProxy)

	track := func(cs ...net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return false
		}
		conns = append(conns, cs...)
		return true
	}

	go func() {
		for {
			downstream, err := ln.Accept()
			if err != nil {
				return // the listener is closed; nothing left to accept
			}
			upstream, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", targetPort))
			if err != nil {
				_ = downstream.Close()
				continue
			}
			if !track(downstream, upstream) {
				_ = downstream.Close()
				_ = upstream.Close()
				return
			}
			go func() { _, _ = io.Copy(upstream, downstream) }()
			go func() { _, _ = io.Copy(downstream, upstream) }()
		}
	}()

	return ln.Addr().(*net.TCPAddr).Port, cut, closeProxy
}

// waitForLog waits for an entry at the given level whose message contains
// substring. It polls rather than reading once, because the client logs from
// its own goroutines and RunClient can return before they have unwound.
func waitForLog(t *testing.T, hook *logtest.Hook, level log.Level, substring string, timeout time.Duration, consequence string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		for _, entry := range hook.AllEntries() {
			if entry.Level == level && strings.Contains(entry.Message, substring) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s entry containing %q was logged. %s\nclient logged:\n  %s",
				level, substring, consequence, strings.Join(loggedLines(hook), "\n  "))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// assertNotLogged fails if any entry contains substring.
func assertNotLogged(t *testing.T, hook *logtest.Hook, substring string, consequence string) {
	t.Helper()

	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, substring) {
			t.Fatalf("%q was logged at %s. %s\nclient logged:\n  %s",
				entry.Message, entry.Level, consequence, strings.Join(loggedLines(hook), "\n  "))
		}
	}
}

func loggedLines(hook *logtest.Hook) []string {
	var lines []string
	for _, e := range hook.AllEntries() {
		lines = append(lines, fmt.Sprintf("[%s] %s", e.Level, e.Message))
	}
	return lines
}

// roundTrip sends payload through the tunnel and returns the echo, failing the
// test if it does not come back.
func roundTrip(t *testing.T, tunnelPort int, payload string) string {
	t.Helper()

	conn := dialUntilReady(t, fmt.Sprintf("127.0.0.1:%d", tunnelPort), 10*time.Second)
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("failed writing through the tunnel: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("failed setting read deadline: %v", err)
	}
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed reading the echo back through the tunnel: %v", err)
	}
	return string(buf[:n])
}

// TestLoopback_EchoRoundTrip is the core assertion: a connection made on the
// cluster side reaches the local service and its reply comes back.
func TestLoopback_EchoRoundTrip(t *testing.T) {
	echoPort := startEchoServer(t)
	tunnelPort := freePort(t)
	startTunnel(t, tunnelPort, echoPort)

	conn := dialUntilReady(t, fmt.Sprintf("127.0.0.1:%d", tunnelPort), 10*time.Second)
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte("hello tunnel")); err != nil {
		t.Fatalf("failed writing through the tunnel: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("failed setting read deadline: %v", err)
	}
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed reading the echo back through the tunnel: %v", err)
	}

	if got, want := string(buf[:n]), "HELLO TUNNEL"; got != want {
		t.Fatalf("round trip returned %q, want %q", got, want)
	}
}

// TestLoopback_MultipleSequentialConnections asserts that sessions do not
// leak into each other -- each connection gets its own.
func TestLoopback_MultipleSequentialConnections(t *testing.T) {
	echoPort := startEchoServer(t)
	tunnelPort := freePort(t)
	startTunnel(t, tunnelPort, echoPort)

	addr := fmt.Sprintf("127.0.0.1:%d", tunnelPort)
	for i := 0; i < 3; i++ {
		conn := dialUntilReady(t, addr, 10*time.Second)

		payload := fmt.Sprintf("message %d", i)
		if _, err := conn.Write([]byte(payload)); err != nil {
			t.Fatalf("connection %d: failed writing: %v", i, err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			t.Fatalf("connection %d: failed setting deadline: %v", i, err)
		}
		buf := make([]byte, 128)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("connection %d: failed reading: %v", i, err)
		}
		if got, want := string(buf[:n]), strings.ToUpper(payload); got != want {
			t.Fatalf("connection %d returned %q, want %q", i, got, want)
		}
		_ = conn.Close()
	}
}

// TestLoopback_ServerBindFailureIsReported is the regression test for the
// bug behind #88, #66 and #143.
//
// When the tunnel server cannot bind its listener it sends an error frame
// describing exactly why -- but that frame has no session ID, because it is
// not about a session. The client used to discard HasErr and LogMessage
// entirely, fail to parse the empty ID, log "failed parsing session uuid from
// stream, skipping" without actually skipping, and carry on with the zero
// UUID. The real diagnosis never reached the user.
func TestLoopback_ServerBindFailureIsReported(t *testing.T) {
	echoPort := startEchoServer(t)

	// Occupy the port the tunnel server will try to bind. This has to be a
	// wildcard bind, matching what InitTunnel does: on macOS binding
	// 0.0.0.0:P succeeds even while 127.0.0.1:P is held, so occupying only
	// the loopback address would not conflict.
	tunnelPort := freePort(t)
	occupied, err := net.Listen("tcp", fmt.Sprintf(":%d", tunnelPort))
	if err != nil {
		t.Fatalf("failed occupying port %d: %v", tunnelPort, err)
	}
	defer func() { _ = occupied.Close() }()

	tn := startTunnel(t, tunnelPort, echoPort)

	// The client should report what the server told it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, entry := range tn.clientHook.AllEntries() {
			if strings.Contains(entry.Message, "tunnel server:") &&
				strings.Contains(entry.Message, "failed opening listener") {
				return // reported, as it should be
			}
		}
		if time.Now().After(deadline) {
			var seen []string
			for _, e := range tn.clientHook.AllEntries() {
				seen = append(seen, e.Message)
			}
			t.Fatalf("the server's bind failure was never reported to the client.\n"+
				"client logged:\n  %s", strings.Join(seen, "\n  "))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLoopback_UnparseableSessionIDIsSkipped guards the other half of the same
// bug: a frame whose session ID cannot be parsed must be skipped, not acted on
// with the zero UUID.
func TestLoopback_UnparseableSessionIDIsSkipped(t *testing.T) {
	echoPort := startEchoServer(t)
	tunnelPort := freePort(t)
	tn := startTunnel(t, tunnelPort, echoPort)

	// Establish a working tunnel first, so we know the client is running.
	conn := dialUntilReady(t, fmt.Sprintf("127.0.0.1:%d", tunnelPort), 10*time.Second)
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("failed writing: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("failed setting deadline: %v", err)
	}
	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("failed reading: %v", err)
	}
	_ = conn.Close()

	// A healthy tunnel must never produce the parse failure.
	for _, entry := range tn.clientHook.AllEntries() {
		if strings.Contains(entry.Message, "failed parsing session uuid") {
			t.Fatalf("healthy tunnel logged a session uuid parse failure: %q", entry.Message)
		}
	}
}

// TestRunClient_ReturnsWhenTheConnectionDrops is the regression test for the
// zombie in #114.
//
// RunClient used to block on <-ctx.Done() forever, so when the connection to
// the server died the process stayed up holding a tunnel that carried nothing.
// Users worked around it by grepping ktunnel's own logs for "lost connection"
// and killing the process. Nothing can reconnect a tunnel it is never told
// about, so this return is the foundation of the whole feature.
//
// Cutting the connections closes sockets, so the client learns of it from a
// reset. The half-open case that keepalive exists for -- a suspended laptop,
// where no reset ever arrives -- is deliberately not covered here: detecting
// it honestly takes the 30s ping interval plus the 10s timeout, which is too
// long to spend on every run. Keepalive was verified by hand instead.
func TestRunClient_ReturnsWhenTheConnectionDrops(t *testing.T) {
	echoPort := startEchoServer(t)
	tunnelPort := freePort(t)
	tn := startTunnelThroughProxy(t, tunnelPort, echoPort)

	// Prove the tunnel works before breaking it, so that a return here
	// cannot be mistaken for a client that never connected in the first
	// place.
	if got, want := roundTrip(t, tunnelPort, "hello"), "HELLO"; got != want {
		t.Fatalf("round trip returned %q, want %q -- the tunnel was not up before the test broke it", got, want)
	}

	tn.cut()

	select {
	case err := <-tn.clientErr:
		if err == nil {
			t.Fatal("RunClient returned nil after the connection to the server dropped; " +
				"a caller cannot tell that from a clean shutdown, so it exits 0 instead of reconnecting")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunClient never returned after the connection to the server dropped; " +
			"this is the #114 zombie -- the process sits there holding a tunnel that no longer carries traffic")
	}

	// Returning is half of being observable; the log is the other half, and
	// it is what users actually grep. A lost tunnel must not be reported the
	// way a requested one is.
	waitForLog(t, tn.clientHook, log.WarnLevel, "error reading from stream", 10*time.Second,
		"a dropped connection was never reported as a failure at all")
	assertNotLogged(t, tn.clientHook, "closing listener",
		"a dropped connection was announced as an orderly shutdown, which is exactly what makes a dead tunnel look alive in the logs")
}

// TestRunClient_ReturnsNilOnCleanShutdown keeps the other half of the contract:
// Ctrl+C must exit zero, and must not look to a supervisor like something to
// reconnect.
func TestRunClient_ReturnsNilOnCleanShutdown(t *testing.T) {
	echoPort := startEchoServer(t)
	tunnelPort := freePort(t)
	tn := startTunnel(t, tunnelPort, echoPort)

	if got, want := roundTrip(t, tunnelPort, "hello"), "HELLO"; got != want {
		t.Fatalf("round trip returned %q, want %q -- the tunnel was not up before the test stopped it", got, want)
	}

	tn.stop()

	select {
	case err := <-tn.clientErr:
		if err != nil {
			t.Fatalf("RunClient reported %v after its context was cancelled; "+
				"Ctrl+C would exit non-zero and a supervisor would reconnect a tunnel the user asked to close", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunClient did not return after its context was cancelled; Ctrl+C would hang")
	}

	// The other half of the same distinction: a shutdown the user asked for
	// is not something to raise the alarm about.
	waitForLog(t, tn.clientHook, log.InfoLevel, "closing listener", 10*time.Second,
		"a requested shutdown was not reported as one")
	assertNotLogged(t, tn.clientHook, "error reading from stream",
		"Ctrl+C was reported as a stream failure, which trains users to ignore the line that means the tunnel really died")
}

// TestRunClient_ReturnsWhenTheServerIsUnreachable covers the setup half of the
// same problem: the failure to open a stream used to be logged inside the
// per-tunnel goroutine and swallowed, leaving RunClient parked on a tunnel
// that had never opened.
func TestRunClient_ReturnsWhenTheServerIsUnreachable(t *testing.T) {
	echoPort := startEchoServer(t)
	// Nothing is listening here, and nothing ever will be.
	grpcPort := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	clientLog, _ := testLogger()
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.RunClient(ctx,
			client.WithServer("127.0.0.1", grpcPort),
			client.WithLogger(clientLog),
			client.WithTunnels("tcp", fmt.Sprintf("%d:127.0.0.1:%d", freePort(t), echoPort)),
			client.WithSessionStore(common.NewSessionStore()),
		)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("RunClient returned nil when it could not reach the server; the caller is told the tunnel is fine when no stream was ever opened")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunClient never returned when it could not reach the server; the user waits on a tunnel that was never opened")
	}
}

// TestRunClient_RejectsAnUnparseableTunnel asserts that a bad port spec is
// reported rather than dropped. It used to be logged and skipped, so a client
// asked for two tunnels would serve one and report itself running -- the
// remaining port silently refusing connections for the rest of the session.
func TestRunClient_RejectsAnUnparseableTunnel(t *testing.T) {
	// A deadline rather than context.Background(): a regression here means
	// RunClient starts the tunnels it could parse and blocks, and a test
	// that hangs forever tells nobody anything.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	clientLog, _ := testLogger()
	err := client.RunClient(ctx,
		client.WithServer("127.0.0.1", freePort(t)),
		client.WithLogger(clientLog),
		client.WithTunnels("tcp", "8000:8001", "8002:not-a-port"),
		client.WithSessionStore(common.NewSessionStore()),
	)
	if err == nil {
		t.Fatal("RunClient accepted an unparseable tunnel and reported success; the port it could not parse would silently never be served")
	}
	if !strings.Contains(err.Error(), "not-a-port") {
		t.Fatalf("RunClient reported %q, which does not say which tunnel spec was wrong", err)
	}
}

// TestRunClient_RejectsAnUnsupportedSchemeBeforeDialling pins where the
// scheme check lives. It used to run inside the per-tunnel goroutine, so an
// unusable scheme arrived at the caller as a tunnel failure -- which the
// supervisor retries, forever, against a configuration error that no
// reconnect can fix.
//
// "TCP" is the input the check rejects today; the condition it uses reads
// inverted, and correcting that is a separate change. Whoever makes it should
// keep this test and change the value.
func TestRunClient_RejectsAnUnsupportedSchemeBeforeDialling(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	clientLog, _ := testLogger()
	err := client.RunClient(ctx,
		client.WithServer("127.0.0.1", freePort(t)),
		client.WithLogger(clientLog),
		client.WithTunnels("TCP", "8000:8001"),
		client.WithSessionStore(common.NewSessionStore()),
	)
	if err == nil {
		t.Fatal("RunClient accepted an unsupported scheme and reported success")
	}
	if !strings.Contains(err.Error(), "unsupported connection scheme") {
		t.Fatalf("RunClient reported %q; a scheme it cannot use has to be reported as the configuration error it is, or a supervisor retries it until the user gives up", err)
	}
}
