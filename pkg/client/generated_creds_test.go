package client_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/client"
	"github.com/omrikiei/ktunnel/pkg/common"
	"github.com/omrikiei/ktunnel/pkg/creds"
	"github.com/omrikiei/ktunnel/pkg/server"
)

// TestGeneratedBundleSecuresATunnelEndToEnd is the release in one test: a
// bundle from pkg/creds, the server half serving it, the client half
// verifying it and presenting the token, and traffic going through.
//
// It is also the test that would have caught the two separate reasons
// in-cluster TLS could not work before v2.4 -- a certificate the client
// cannot verify, and a token the server never checks -- because either one
// stops the round trip below.
func TestGeneratedBundleSecuresATunnelEndToEnd(t *testing.T) {
	bundle, err := creds.Generate("myapp", "dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// The server reads its certificate from the mounted Secret, which is a
	// pair of files as far as the process is concerned.
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, bundle.ServerCert, 0600); err != nil {
		t.Fatalf("writing the server certificate: %v", err)
	}
	if err := os.WriteFile(keyFile, bundle.ServerKey, 0600); err != nil {
		t.Fatalf("writing the server key: %v", err)
	}

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
			server.WithTLS(certFile, keyFile),
			server.WithToken(bundle.Token),
			server.WithSessionStore(common.NewSessionStore()),
		)
	}()
	conn := dialUntilReady(t, fmt.Sprintf("127.0.0.1:%d", grpcPort), 10*time.Second)
	_ = conn.Close()

	clientLog, _ := testLogger()
	clientErr := make(chan error, 1)
	go func() {
		// The client never writes the CA to disk: it holds the bundle it
		// generated moments ago in memory, which is why a SIGKILL leaves no
		// credential material anywhere.
		clientErr <- client.RunClient(ctx,
			client.WithServer("127.0.0.1", grpcPort),
			client.WithLogger(clientLog),
			client.WithTunnels("tcp", fmt.Sprintf("%d:127.0.0.1:%d", tunnelPort, echoPort)),
			client.WithTLSFromPEM(bundle.CACert, ""),
			client.WithToken(bundle.Token),
			client.WithSessionStore(common.NewSessionStore()),
		)
	}()

	awaitTunnelOrFailure(t, tunnelPort, clientErr, 20*time.Second)

	if got, want := roundTrip(t, tunnelPort, "end to end"), "END TO END"; got != want {
		t.Fatalf("round trip over a generated-credential tunnel returned %q, want %q", got, want)
	}
}

// The generated certificate names 127.0.0.1, so the client verifies it
// without --server-host-override. That is what lets expose and inject turn
// TLS on by default without asking the user for a hostname they should not
// have to know.
func TestGeneratedCertNeedsNoHostOverride(t *testing.T) {
	bundle, err := creds.Generate("myapp", "dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// An empty override means "verify against the address dialled".
	if _, err := client.TLSConfigFromPEM(bundle.CACert, ""); err != nil {
		t.Fatalf("building a TLS config from the generated CA: %v", err)
	}
}
