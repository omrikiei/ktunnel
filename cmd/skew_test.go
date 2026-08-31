package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/omrikiei/ktunnel/pkg/creds"
)

func TestLooksLikePlaintextServer(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			// What gRPC reports when a TLS client meets a plaintext server:
			// it reads the HTTP/2 preface where a handshake should be.
			name: "server preface",
			err:  errors.New(`connection error: desc = "transport: authentication handshake failed: tls: first record does not look like a TLS handshake"`),
			want: true,
		},
		{
			name: "handshake failure",
			err:  errors.New("rpc error: code = Unavailable desc = transport: authentication handshake failed: EOF"),
			want: true,
		},
		{
			// An ordinary outage. Downgrading here would turn a dropped VPN
			// into a silently unencrypted tunnel for the rest of the run.
			name: "connection refused",
			err:  errors.New("connection error: desc = \"transport: Error while dialing: dial tcp 127.0.0.1:28688: connect: connection refused\""),
			want: false,
		},
		{
			name: "rejected credentials",
			err:  errors.New("rpc error: code = Unauthenticated desc = invalid tunnel token"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikePlaintextServer(tc.err); got != tc.want {
				t.Errorf("looksLikePlaintextServer(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A server image older than this binary serves plaintext and ignores the
// token. Per the release decision this does not abort -- it degrades and says
// so -- but it degrades exactly once, and only for that reason.
func TestTLSDowngradeHappensOnceAndOnlyForAHandshakeFailure(t *testing.T) {
	prev := tunnelCreds
	t.Cleanup(func() { tunnelCreds = prev })
	tunnelCreds = sessionCredentials{bundle: &creds.Bundle{Token: "t"}, encrypted: true}

	handshakeFailed := func(context.Context, func()) error {
		return errors.New("transport: authentication handshake failed: tls: first record does not look like a TLS handshake")
	}
	attempt := withTLSDowngrade(handshakeFailed)

	if err := attempt(context.Background(), func() {}); err == nil {
		t.Fatal("the failure was swallowed; the supervisor has nothing to retry on")
	}
	if tunnelCreds.encrypted {
		t.Fatal("the tunnel did not fall back to plaintext, so every retry fails the same way forever")
	}

	// Second time round it is already plaintext, and an unrelated failure
	// must not touch anything.
	tunnelCreds.encrypted = true
	refused := func(context.Context, func()) error {
		return errors.New("dial tcp 127.0.0.1:28688: connect: connection refused")
	}
	_ = withTLSDowngrade(refused)(context.Background(), func() {})
	if !tunnelCreds.encrypted {
		t.Error("a refused connection dropped encryption; a dropped VPN would silently unencrypt the tunnel")
	}
}
