package creds

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"
)

// parseLeaf is the assertion helper the other tests lean on: a bundle is only
// useful if its PEM actually decodes to a certificate.
func parseLeaf(t *testing.T, b *Bundle) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(b.ServerCert)
	if block == nil {
		t.Fatal("server certificate is not valid PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parsing server certificate: %v", err)
	}
	return leaf
}

func TestGenerateLeafCarriesTheSANsTheClientDials(t *testing.T) {
	b, err := Generate("myapp", "dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	leaf := parseLeaf(t, b)

	// The client reaches the server through a port-forward on loopback, so
	// without these two the handshake fails hostname verification and --tls
	// is no more real than it was before this release.
	wantIPs := []string{"127.0.0.1"}
	for _, want := range wantIPs {
		found := false
		for _, ip := range leaf.IPAddresses {
			if ip.Equal(net.ParseIP(want)) {
				found = true
			}
		}
		if !found {
			t.Errorf("leaf is missing IP SAN %s, has %v", want, leaf.IPAddresses)
		}
	}

	wantDNS := []string{"localhost", "myapp.dev.svc"}
	for _, want := range wantDNS {
		found := false
		for _, name := range leaf.DNSNames {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("leaf is missing DNS SAN %s, has %v", want, leaf.DNSNames)
		}
	}
}

func TestGenerateLeafVerifiesAgainstItsOwnCA(t *testing.T) {
	b, err := Generate("myapp", "dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CACert) {
		t.Fatal("CA certificate is not valid PEM")
	}

	leaf := parseLeaf(t, b)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		DNSName:     "localhost",
		CurrentTime: time.Now(),
	}); err != nil {
		t.Fatalf("leaf does not verify against the bundle's own CA: %v", err)
	}
}

func TestGenerateTokenIsUnpredictable(t *testing.T) {
	first, err := Generate("myapp", "dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	second, err := Generate("myapp", "dev")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if first.Token == "" {
		t.Fatal("token is empty")
	}
	// 32 random bytes, base64: anything materially shorter is not a secret.
	if len(first.Token) < 40 {
		t.Errorf("token is %d characters, want at least 40", len(first.Token))
	}
	if first.Token == second.Token {
		t.Error("two Generate calls produced the same token")
	}
}
