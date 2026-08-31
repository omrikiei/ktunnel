package client_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/client"
	"github.com/omrikiei/ktunnel/pkg/common"
	"github.com/omrikiei/ktunnel/pkg/server"
)

// TestTunnelWithMatchingTokenCarriesTraffic is the happy path of v2.4: both
// halves hold the same generated token, and the tunnel behaves exactly as an
// unauthenticated one did.
func TestTunnelWithMatchingTokenCarriesTraffic(t *testing.T) {
	const token = "a-shared-secret-for-this-run"

	echoPort := startEchoServer(t)
	tunnelPort := freePort(t)
	grpcPort := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serverLog, _ := testLogger()
	go func() {
		_ = server.RunServer(ctx,
			server.WithPort(grpcPort),
			server.WithLogger(serverLog),
			server.WithToken(token),
			server.WithSessionStore(common.NewSessionStore()),
		)
	}()
	conn := dialUntilReady(t, fmt.Sprintf("127.0.0.1:%d", grpcPort), 10*time.Second)
	_ = conn.Close()

	clientLog, _ := testLogger()
	clientErr := make(chan error, 1)
	go func() {
		clientErr <- client.RunClient(ctx,
			client.WithServer("127.0.0.1", grpcPort),
			client.WithLogger(clientLog),
			client.WithTunnels("tcp", fmt.Sprintf("%d:127.0.0.1:%d", tunnelPort, echoPort)),
			client.WithToken(token),
			client.WithSessionStore(common.NewSessionStore()),
		)
	}()

	awaitTunnelOrFailure(t, tunnelPort, clientErr, 20*time.Second)

	if got, want := roundTrip(t, tunnelPort, "hello auth"), "HELLO AUTH"; got != want {
		t.Fatalf("round trip over an authenticated tunnel returned %q, want %q", got, want)
	}
}

// TestTunnelWithoutTokenNeverOpensThePort is the attack this release exists to
// stop: something in the cluster reaches the gRPC port and attaches as a
// client, and is handed the traffic meant for the developer's machine.
//
// The assertion is on the tunnelled port, not on the error. An unauthenticated
// caller must not merely be refused data -- it must not be able to make the
// server bind a port at all.
func TestTunnelWithoutTokenNeverOpensThePort(t *testing.T) {
	echoPort := startEchoServer(t)
	tunnelPort := freePort(t)
	grpcPort := freePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serverLog, _ := testLogger()
	go func() {
		_ = server.RunServer(ctx,
			server.WithPort(grpcPort),
			server.WithLogger(serverLog),
			server.WithToken("the-real-token"),
			server.WithSessionStore(common.NewSessionStore()),
		)
	}()
	conn := dialUntilReady(t, fmt.Sprintf("127.0.0.1:%d", grpcPort), 10*time.Second)
	_ = conn.Close()

	clientLog, _ := testLogger()
	go func() {
		// No WithToken at all: a v2.3 client, or an attacker who found the
		// port.
		_ = client.RunClient(ctx,
			client.WithServer("127.0.0.1", grpcPort),
			client.WithLogger(clientLog),
			client.WithTunnels("tcp", fmt.Sprintf("%d:127.0.0.1:%d", tunnelPort, echoPort)),
			client.WithSessionStore(common.NewSessionStore()),
		)
	}()

	// Long enough that a server which was going to bind the port has done so.
	deadline := time.Now().Add(3 * time.Second)
	addr := fmt.Sprintf("127.0.0.1:%d", tunnelPort)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
			_ = c.Close()
			t.Fatalf("%s accepted a connection: an unauthenticated client opened a tunnel", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
