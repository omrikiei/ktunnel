package cmd

import (
	"context"
	"strings"

	"github.com/omrikiei/ktunnel/pkg/supervisor"
)

// looksLikePlaintextServer reports whether an error is the one a TLS client
// gets from a server that is not speaking TLS.
//
// It matches on the transport's own wording rather than a typed error because
// gRPC does not expose one: the handshake failure is wrapped in a connection
// error by the time a caller sees it. The strings are narrow on purpose --
// anything vaguer would catch an ordinary outage, and downgrading on a dropped
// VPN would leave the rest of the run unencrypted for no reason.
func looksLikePlaintextServer(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "authentication handshake failed") ||
		strings.Contains(msg, "does not look like a TLS handshake")
}

// withTLSDowngrade lets a run survive a tunnel server image older than this
// binary.
//
// An older image does not read KTUNNEL_TOKEN and does not serve TLS, so the
// client's handshake fails and every retry fails identically. Rather than
// abort -- a pinned --image is a legitimate thing to have, and this is a
// development tool -- the run continues without encryption, and says so once,
// in terms that name the cause and the flag that controls it.
//
// It fires at most once: after the first downgrade the tunnel is already
// plaintext, so the condition cannot hold again.
func withTLSDowngrade(attempt supervisor.Attempt) supervisor.Attempt {
	return func(ctx context.Context, established func()) error {
		err := attempt(ctx, established)
		if err != nil && tunnelCreds.encrypted && looksLikePlaintextServer(err) {
			logger.Warnf("the tunnel server at image %q does not speak TLS, which means it predates ktunnel v2.4", ServerImage)
			logger.Warn("continuing WITHOUT encryption or authentication; " +
				"pass --image with a v2.4 or newer tag for a secured tunnel")
			tunnelCreds.encrypted = false
			tunnelCreds.bundle = nil
		}
		return err
	}
}
