package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultStableAfter is how long an attempt has to stay up before it counts as
// established rather than lucky.
const DefaultStableAfter = time.Minute

// Attempt establishes something and blocks until it fails. Returning nil means
// the attempt ended without an error, which still counts as an end.
//
// An Attempt must return once ctx is cancelled. Run waits for it, so whatever
// it holds -- a listener, a port-forward on a local port -- is released before
// the next attempt starts or before Run returns.
type Attempt func(ctx context.Context) error

// Supervisor runs an Attempt, and runs it again when it ends.
type Supervisor struct {
	// Attempt is the work being supervised. Required.
	Attempt Attempt
	// Backoff decides how long to wait between attempts.
	Backoff Backoff
	// MaxAttempts gives up once this many consecutive attempts have failed.
	// 0 retries forever.
	MaxAttempts int
	// ExitOnFirst returns the first failure instead of retrying it.
	ExitOnFirst bool
	// StableAfter is how long an attempt must stay up before its eventual
	// failure is treated as a fresh one rather than a continuation of the
	// streak. DefaultStableAfter when zero.
	StableAfter time.Duration
	// Log receives the state transitions. Output is discarded when nil.
	Log log.FieldLogger

	// after is time.After unless a test replaced it, so waits can be driven
	// rather than slept through.
	after func(d time.Duration) <-chan time.Time
}

// Run blocks, running Attempt and retrying it, until ctx is cancelled or the
// give-up policy is reached. Cancelling ctx is a clean shutdown and returns
// nil; giving up returns an error wrapping the last failure.
func (s *Supervisor) Run(ctx context.Context) error {
	if s.Attempt == nil {
		return errors.New("supervisor: no Attempt to run")
	}

	logger := s.Log
	if logger == nil {
		logger = &log.Logger{Out: io.Discard}
	}
	after := s.after
	if after == nil {
		after = time.After
	}
	stableAfter := s.StableAfter
	if stableAfter <= 0 {
		stableAfter = DefaultStableAfter
	}

	failures := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		done := make(chan error, 1)
		go func() { done <- s.Attempt(ctx) }()

		// An attempt that stays up this long is a working tunnel rather
		// than a lucky dial, so its eventual failure starts the backoff
		// over. Without this a link that drops every few minutes creeps to
		// a permanent 30-second delay and never comes back promptly again.
		stable := after(stableAfter)

		var err error
	running:
		for {
			select {
			case err = <-done:
				break running
			case <-stable:
				// Only once: a nil channel blocks forever.
				stable = nil
				if failures > 0 {
					logger.Info("tunnel re-established")
				}
				failures = 0
			}
		}

		if ctx.Err() != nil {
			// The attempt ended because we cancelled it. That is a clean
			// shutdown, not something to report or retry.
			return nil
		}

		failures++
		if err != nil {
			logger.Infof("tunnel lost: %v", err)
		} else {
			logger.Info("tunnel lost: the attempt ended without an error")
		}

		if s.ExitOnFirst {
			return err
		}
		if s.MaxAttempts > 0 && failures >= s.MaxAttempts {
			if err == nil {
				return fmt.Errorf("giving up after %d attempts: the last attempt ended without an error", failures)
			}
			return fmt.Errorf("giving up after %d attempts: %w", failures, err)
		}

		delay := s.Backoff.Delay(failures)
		// Rounded because the jitter would otherwise print as 1.0873s, and
		// this line exists to be read.
		logger.Infof("reconnecting in %s (attempt %d)", delay.Round(100*time.Millisecond), failures+1)

		select {
		case <-ctx.Done():
			return nil
		case <-after(delay):
		}
	}
}
