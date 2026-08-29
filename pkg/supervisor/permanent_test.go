package supervisor_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omrikiei/ktunnel/pkg/supervisor"
)

// TestRun_GivesUpImmediatelyOnAPermanentFailure is the regression test for a
// typo that retried forever.
//
// Before the supervisor, a malformed port spec was one message and exit 1.
// Under a supervisor that retries forever by default, the same typo became a
// failure every backoff interval for as long as the user left ktunnel running
// -- reconnecting, in the logs, against something that was never going to
// connect.
func TestRun_GivesUpImmediatelyOnAPermanentFailure(t *testing.T) {
	cause := errors.New("bad tunnel format: \"8000:not:a:port:spec\"")

	var attempts atomic.Int32
	s := &supervisor.Supervisor{
		// The default policy, which is the one that used to retry forever.
		Attempt: func(context.Context, func()) error {
			attempts.Add(1)
			return supervisor.Permanent(cause)
		},
		Backoff: supervisor.Backoff{Base: time.Millisecond, Max: time.Millisecond},
	}

	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a permanent failure returned nil, so the command exits 0; a process supervisor reads that as a clean shutdown and never restarts it")
		}
		if !errors.Is(err, cause) {
			t.Errorf("Run returned %v, which does not carry the reason (%v); the user cannot see which part of their command line was wrong", err, cause)
		}
		if !strings.Contains(err.Error(), "bad tunnel format") {
			t.Errorf("Run returned %q, which does not say what was wrong", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned on a failure no retry can fix; with the default policy it retries forever, logging the user's typo at them every backoff interval")
	}

	if got := attempts.Load(); got != 1 {
		t.Errorf("made %d attempts at something that cannot succeed, want 1", got)
	}
}

// TestRun_RetriesAnOrdinaryFailure keeps the mark from swallowing the feature:
// only what is marked is permanent, and a dropped connection is not.
func TestRun_RetriesAnOrdinaryFailure(t *testing.T) {
	var attempts atomic.Int32
	s := &supervisor.Supervisor{
		Attempt: func(context.Context, func()) error {
			attempts.Add(1)
			return errors.New("rpc error: code = Unavailable")
		},
		Backoff:     supervisor.Backoff{Base: time.Millisecond, Max: time.Millisecond},
		MaxAttempts: 3,
	}

	if err := s.Run(context.Background()); err == nil {
		t.Fatal("giving up after the maximum attempts returned nil")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("made %d attempts at an ordinary failure, want 3; an unmarked error must still be retried", got)
	}
}

// TestPermanent_KeepsTheMessageAndTheMark: the message a user reads is the
// cause's own. Wrapping with a sentinel prefix would put our vocabulary in
// front of their typo.
func TestPermanent_KeepsTheMessageAndTheMark(t *testing.T) {
	cause := errors.New("unsupported connection scheme UDP")
	marked := supervisor.Permanent(cause)

	if got, want := marked.Error(), cause.Error(); got != want {
		t.Errorf("marking changed the message to %q, want %q", got, want)
	}
	if !errors.Is(marked, supervisor.ErrPermanent) {
		t.Error("a marked error is not recognised as permanent, so it would be retried forever")
	}
	if !errors.Is(marked, cause) {
		t.Error("marking hid the cause, so callers can no longer match on it")
	}
	if supervisor.Permanent(nil) != nil {
		t.Error("Permanent(nil) is not nil, so a success would be reported as a permanent failure")
	}
}
