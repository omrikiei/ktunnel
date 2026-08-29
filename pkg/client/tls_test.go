package client_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/client"
	"github.com/omrikiei/ktunnel/pkg/common"
	"github.com/omrikiei/ktunnel/pkg/server"
)

// TestRunClient_TLSActuallyEncryptsTheTunnel is the regression test for a
// --tls flag that has never done anything.
//
// WithTLS set opt.TLS from opt.certFile *before* assigning it -- that is, from
// the certificate of an earlier WithTLS on the same config, of which there is
// never one. opt.TLS therefore stayed false, RunClient took the insecure
// branch, and every ktunnel that has ever been run with --tls and a --ca-file
// sent its traffic in the clear while reporting nothing unusual.
//
// The assertion is a round trip against a server that speaks only TLS: a
// client that quietly fell back to plaintext cannot complete it.
func TestRunClient_TLSActuallyEncryptsTheTunnel(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)

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
			// The host override is the second half of the flag pair
			// (--ca-file and --server-host-override): the certificate names
			// localhost, and the client dials 127.0.0.1.
			client.WithTLS(certFile, "localhost"),
			client.WithSessionStore(common.NewSessionStore()),
		)
	}()

	awaitTunnelOrFailure(t, tunnelPort, clientErr, 20*time.Second)

	if got, want := roundTrip(t, tunnelPort, "hello tls"), "HELLO TLS"; got != want {
		t.Fatalf("round trip over a TLS tunnel returned %q, want %q", got, want)
	}
}

// awaitTunnelOrFailure waits for the tunnelled port to start accepting
// connections, and fails as soon as the client gives up instead.
//
// A client that does not speak the server's protocol never opens a stream, so
// the server never binds the port -- waiting for it alone would report that as
// nothing more than a timeout on an empty port, when the client already knows
// exactly what went wrong.
func awaitTunnelOrFailure(t *testing.T, tunnelPort int, clientErr <-chan error, timeout time.Duration) {
	t.Helper()

	addr := fmt.Sprintf("127.0.0.1:%d", tunnelPort)
	deadline := time.Now().Add(timeout)
	for {
		select {
		case err := <-clientErr:
			t.Fatalf("the client stopped before its tunnel was up: %v\n"+
				"a client that ignores --tls dials a TLS server in plaintext, so the tunnel never comes up at all", err)
		default:
		}

		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the TLS tunnel never started serving %s within %s: %v", addr, timeout, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// writeSelfSignedCert generates a certificate for localhost/127.0.0.1 and
// writes it, with its key, into the test's temporary directory. Generated
// rather than checked in, so that nothing here expires or has to be rotated.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed generating a test key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed generating a test certificate: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "tls.crt")
	keyFile = filepath.Join(dir, "tls.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("failed writing the test certificate: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("failed encoding the test key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("failed writing the test key: %v", err)
	}

	return certFile, keyFile
}
