package client

import (
	"testing"
)

func configFrom(t *testing.T, opts ...Option) *Config {
	t.Helper()
	c := &Config{}
	for _, o := range opts {
		if err := o(c); err != nil {
			t.Fatalf("applying option: %v", err)
		}
	}
	return c
}

// expose mounts a certificate into the tunnel server, so its client both
// verifies TLS and presents the token.
func TestWithTLSFromPEMAndTokenConfigureBothHalves(t *testing.T) {
	c := configFrom(t, WithTLSFromPEM([]byte("-----BEGIN CERTIFICATE-----"), ""), WithToken("s3cret"))

	if !c.TLS {
		t.Error("WithTLSFromPEM did not turn TLS on")
	}
	if len(c.caPEM) == 0 {
		t.Error("the CA was not kept")
	}
	if c.certFile != "" {
		t.Errorf("a --ca-file path was set to %q; the generated CA never touches disk", c.certFile)
	}
	if c.token != "s3cret" {
		t.Errorf("token is %q, want s3cret", c.token)
	}
}

// inject has no certificate to mount, so its client authenticates over a
// plaintext connection. Turning TLS on here would fail the handshake against
// a sidecar that serves plaintext, which is the failure v2.3 refused the flag
// to avoid.
func TestWithTokenAloneLeavesTLSOff(t *testing.T) {
	c := configFrom(t, WithToken("s3cret"))

	if c.TLS {
		t.Error("a token-only client turned TLS on, and will fail its handshake against a plaintext sidecar")
	}
	if c.token != "s3cret" {
		t.Errorf("token is %q, want s3cret", c.token)
	}
}
