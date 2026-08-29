package server_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	pb "github.com/omrikiei/ktunnel/api"
	"github.com/omrikiei/ktunnel/pkg/common"
	"github.com/omrikiei/ktunnel/pkg/server"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

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

func quietLogger() *log.Logger {
	l := log.New()
	l.SetOutput(io.Discard)
	return l
}

// awaitListener waits until something accepts connections on port.
func awaitListener(t *testing.T, port int, what string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never started listening on %d: %v", what, port, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// assertPortIsFree fails unless the port can be bound again, which is the only
// honest way to ask whether the thing that was holding it has really let go.
//
// The bind is a wildcard one, matching what the server does: on macOS binding
// 0.0.0.0:P succeeds while 127.0.0.1:P is held, so a loopback bind would
// report a port free that the server is still serving on.
func assertPortIsFree(t *testing.T, port int, consequence string) {
	t.Helper()

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("port %d is still bound after the server was stopped: %v\n%s", port, err, consequence)
	}
	_ = ln.Close()
}

// TestRunServer_StopsWhenItsContextIsCancelled is the regression test for a
// server that could not be stopped.
//
// Cancelling the context used to close the gRPC listener and nothing else.
// Every stream already open kept running, and each open stream is an
// InitTunnel holding a listener of its own on a tunnelled port -- so a
// "stopped" server went on accepting connections on those ports and
// forwarding them to a client that had been told the server was gone. Nothing
// could take its place: a replacement server binding the same tunnelled port
// failed, and until it did the old one quietly kept serving.
//
// This is why the reconnect test in cmd had to cut a TCP proxy rather than
// restart a server, and it is the thing a real pod restart does that no proxy
// can imitate.
func TestRunServer_StopsWhenItsContextIsCancelled(t *testing.T) {
	grpcPort := freePort(t)
	tunnelPort := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- server.RunServer(ctx,
			server.WithPort(grpcPort),
			server.WithLogger(quietLogger()),
			server.WithSessionStore(common.NewSessionStore()),
		)
	}()
	awaitListener(t, grpcPort, "the tunnel server")

	// A tunnel stream, which is the only kind of RPC this server has: a
	// long-lived one that ends when the client goes away, never on its own.
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", grpcPort),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("failed creating a gRPC client: %v", err)
	}
	defer func() { _ = conn.Close() }()

	streamCtx, endStream := context.WithCancel(context.Background())
	defer endStream()
	stream, err := pb.NewTunnelClient(conn).InitTunnel(streamCtx)
	if err != nil {
		t.Fatalf("failed opening a tunnel: %v", err)
	}
	if err := stream.Send(&pb.SocketDataRequest{Port: int32(tunnelPort), Scheme: pb.TunnelScheme_TCP}); err != nil {
		t.Fatalf("failed asking the server to listen on %d: %v", tunnelPort, err)
	}
	awaitListener(t, tunnelPort, "the tunnelled port")

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunServer reported %v after its context was cancelled; "+
				"`ktunnel server` logs that as a fatal error on Ctrl+C, and a caller cannot tell a requested shutdown from a crash", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunServer never returned after its context was cancelled")
	}

	assertPortIsFree(t, tunnelPort, "the server kept serving the tunnelled port after it was stopped, so a replacement server cannot bind it "+
		"-- a restarted tunnel either fails to come up or comes up alongside a server that is still forwarding traffic")
	assertPortIsFree(t, grpcPort, "the server kept its gRPC port after it was stopped, so nothing can take its place")

	// The client's side of the same fact: a stopped server ends the streams
	// it was serving, which is how a client learns to reconnect at all.
	streamEnded := make(chan error, 1)
	go func() {
		_, err := stream.Recv()
		streamEnded <- err
	}()
	select {
	case err := <-streamEnded:
		if err == nil {
			t.Fatal("the tunnel stream was still carrying data after the server was stopped")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tunnel stream outlived the server that was serving it; the client is left holding a tunnel to a server that is gone, which is the #114 zombie")
	}
}
