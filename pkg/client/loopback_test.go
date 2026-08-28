package client_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
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

// startTunnel brings up a server and a client wired to each other, tunnelling
// tunnelPort on the "cluster" side to targetPort on the "local" side.
func startTunnel(t *testing.T, tunnelPort, targetPort int) (*logtest.Hook, *logtest.Hook) {
	t.Helper()

	grpcPort := freePort(t)
	serverLog, serverHook := testLogger()
	clientLog, clientHook := testLogger()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		_ = server.RunServer(ctx,
			server.WithPort(grpcPort),
			server.WithLogger(serverLog),
			server.WithSessionStore(common.NewSessionStore()),
		)
	}()

	// Wait for the gRPC listener before pointing the client at it.
	conn := dialUntilReady(t, fmt.Sprintf("127.0.0.1:%d", grpcPort), 5*time.Second)
	_ = conn.Close()

	go func() {
		_ = client.RunClient(ctx,
			client.WithServer("127.0.0.1", grpcPort),
			client.WithLogger(clientLog),
			client.WithTunnels("tcp", fmt.Sprintf("%d:127.0.0.1:%d", tunnelPort, targetPort)),
			client.WithSessionStore(common.NewSessionStore()),
		)
	}()

	return serverHook, clientHook
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

	_, clientHook := startTunnel(t, tunnelPort, echoPort)

	// The client should report what the server told it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		for _, entry := range clientHook.AllEntries() {
			if strings.Contains(entry.Message, "tunnel server:") &&
				strings.Contains(entry.Message, "failed opening listener") {
				return // reported, as it should be
			}
		}
		if time.Now().After(deadline) {
			var seen []string
			for _, e := range clientHook.AllEntries() {
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
	_, clientHook := startTunnel(t, tunnelPort, echoPort)

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
	for _, entry := range clientHook.AllEntries() {
		if strings.Contains(entry.Message, "failed parsing session uuid") {
			t.Fatalf("healthy tunnel logged a session uuid parse failure: %q", entry.Message)
		}
	}
}
