package client_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/client"
	"github.com/omrikiei/ktunnel/pkg/common"
	"github.com/omrikiei/ktunnel/pkg/supervisor"
)

// TestRunClient_ConfigurationErrorsAreNotRetryable pins the difference between
// a tunnel worth reconnecting and a command line worth correcting.
//
// RunClient reports both by returning an error, and a supervisor retries what
// it is given -- forever, under the default policy. Every failure here is
// raised before the dial and would fail identically on every attempt, so the
// user would get their own typo logged back at them every backoff interval
// instead of the single message and non-zero exit they got before reconnecting
// existed.
func TestRunClient_ConfigurationErrorsAreNotRetryable(t *testing.T) {
	echoPort := startEchoServer(t)
	goodTunnel := fmt.Sprintf("%d:127.0.0.1:%d", freePort(t), echoPort)

	cases := map[string]struct {
		opts []client.Option
		// consequence describes what retrying this would do to the user.
		consequence string
	}{
		"a malformed port spec": {
			opts:        []client.Option{client.WithTunnels("tcp", "8000:not:a:port:spec")},
			consequence: "a typo in the ports argument is retried until the user notices",
		},
		"a scheme ktunnel does not speak": {
			opts:        []client.Option{client.WithTunnels("TCP", goodTunnel)},
			consequence: "an unusable scheme is retried against something no reconnect can change",
		},
		"a --ca-file that is not there": {
			opts: []client.Option{
				client.WithTunnels("tcp", goodTunnel),
				// Newly reachable: --tls did nothing at all until this
				// branch, so this path could not be hit before.
				client.WithTLS(filepath.Join(t.TempDir(), "absent.crt"), ""),
			},
			consequence: "a missing certificate file is retried as though it might appear",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			clientLog, _ := testLogger()
			opts := append([]client.Option{
				client.WithServer("127.0.0.1", freePort(t)),
				client.WithLogger(clientLog),
				client.WithSessionStore(common.NewSessionStore()),
			}, tc.opts...)

			err := client.RunClient(ctx, opts...)
			if err == nil {
				t.Fatalf("RunClient accepted %s and reported success", name)
			}
			if !errors.Is(err, supervisor.ErrPermanent) {
				t.Fatalf("RunClient reported %q without marking it permanent, so %s", err, tc.consequence)
			}
		})
	}
}

// TestRunClient_AnUnreachableServerIsStillRetryable is the other half: a
// server that is not there right now is exactly what reconnecting is for, and
// marking too much as permanent would turn the feature off.
func TestRunClient_AnUnreachableServerIsStillRetryable(t *testing.T) {
	echoPort := startEchoServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	clientLog, _ := testLogger()
	err := client.RunClient(ctx,
		// Nothing is listening here, the way a rescheduled pod is not
		// listening yet.
		client.WithServer("127.0.0.1", freePort(t)),
		client.WithLogger(clientLog),
		client.WithTunnels("tcp", fmt.Sprintf("%d:127.0.0.1:%d", freePort(t), echoPort)),
		client.WithSessionStore(common.NewSessionStore()),
	)
	if err == nil {
		t.Fatal("RunClient reported success against a server that is not there")
	}
	if errors.Is(err, supervisor.ErrPermanent) {
		t.Fatalf("a server that was not reachable yet was reported as permanent (%v); ktunnel would exit instead of waiting for the pod to come back, which is the whole feature", err)
	}
}
