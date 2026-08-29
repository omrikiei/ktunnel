package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	log "github.com/sirupsen/logrus"
)

// DefaultStableAfter is how long an attempt has to stay up, after reporting
// itself established, before it counts as established rather than lucky.
const DefaultStableAfter = time.Minute

// errAttemptEnded stands in for an Attempt that returned nil. Callers turn a
// non-nil Run error into a non-zero exit code, so a give-up must never hand
// them a nil to exit on.
var errAttemptEnded = errors.New("the attempt ended without an error")

// ErrPermanent marks a failure that no retry can fix. Run returns as soon as
// an Attempt's error wraps it, whatever the give-up policy says.
//
// Reconnecting exists for a network that comes and goes. A malformed port
// spec, a scheme ktunnel does not speak, a certificate file that is not
// there: waiting and trying again produces the identical failure forever, at
// whatever delay the backoff has crept to, and with the default policy --
// retry forever -- the user reads the same line every 30 seconds until they
// work out that ktunnel is never going to fix itself. These were fatal on the
// spot before the supervisor existed, and that is the behaviour to keep.
var ErrPermanent = errors.New("permanent failure")

// Permanent marks err as something no retry can fix. It returns nil for a nil
// error, so a call's result can be wrapped directly.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err}
}

// permanent carries the mark without changing what the error says. A wrapper
// built with fmt.Errorf("%w: %w", ErrPermanent, err) would prefix every such
// message with "permanent failure:", and these messages are read by users who
// need to see their own typo, not our taxonomy.
type permanent struct{ error }

func (p permanent) Is(target error) bool { return target == ErrPermanent }

func (p permanent) Unwrap() error { return p.error }

// Attempt establishes something and blocks until it fails. Returning nil means
// the attempt ended without an error, which still counts as an end.
//
// It must call established once the thing is actually up, before it blocks
// serving it. Until then the supervisor treats the attempt as still dialling:
// a connect to an unreachable host blocks for over a minute on default Linux
// and macOS timeouts, which is longer than StableAfter, so an attempt timed
// from its launch would report a tunnel that never existed and reset the
// backoff that is supposed to be slowing it down. established is safe to call
// repeatedly, from any goroutine, and after ctx is cancelled.
//
// An Attempt must return once ctx is cancelled. Run waits for it, so whatever
// it holds -- a listener, a port-forward on a local port -- is released before
// the next attempt starts or before Run returns.
type Attempt func(ctx context.Context, established func()) error

// Supervisor runs an Attempt, and runs it again when it ends.
type Supervisor struct {
	// Attempt is the work being supervised. Required.
	Attempt Attempt
	// Backoff decides how long to wait between attempts.
	Backoff Backoff
	// MaxAttempts gives up once this many consecutive attempts have failed.
	// 0 retries forever. Ignored when ExitOnFirst is set, which gives up
	// sooner by definition.
	MaxAttempts int
	// ExitOnFirst returns the first failure instead of retrying it. It
	// outranks MaxAttempts when both are set.
	ExitOnFirst bool
	// StableAfter is how long an established attempt must stay up before its
	// eventual failure is treated as a fresh one rather than a continuation
	// of the streak. DefaultStableAfter when zero.
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
		// log.New, not a Logger literal: a literal's zero Level happens to
		// discard Info, but leaves Formatter nil for whoever raises the
		// level later.
		discard := log.New()
		discard.Out = io.Discard
		logger = discard
	}
	after := s.after
	if after == nil {
		// The timers below are never stopped. Since Go 1.23 an unreferenced
		// timer is garbage collected whether or not it has fired, so there
		// is nothing here for NewTimer/Stop bookkeeping to fix.
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

		// Buffered by one and sent to without blocking, so an attempt may
		// call established as often as it likes -- including long after Run
		// has stopped listening -- without stalling the goroutine serving
		// the tunnel.
		up := make(chan struct{}, 1)
		established := func() {
			select {
			case up <- struct{}{}:
			default:
			}
		}
		go func() { done <- s.Attempt(ctx, established) }()

		// A separate view of `up` so that nil-ing it after the first report
		// does not disturb the closure the attempt is holding.
		upCh := (<-chan struct{})(up)

		// Nil until the attempt reports itself established: one that never
		// does never becomes stable, so it can neither reset the backoff
		// nor clear the MaxAttempts streak. Receiving from a nil channel
		// blocks forever, which is exactly "nothing to watch for here".
		var stableTimer <-chan time.Time

		var err error
	running:
		for {
			select {
			case err = <-done:
				// An attempt can report itself up and then fail before Run
				// gets back to this select. Report it anyway, so the log
				// reads in the order things happened -- but only if it was
				// not reported already, since established may be called
				// more than once.
				if upCh != nil {
					select {
					case <-upCh:
						logger.Info("tunnel established")
					default:
					}
				}
				break running

			case <-upCh:
				// Watched once: a nil channel blocks forever.
				upCh = nil
				logger.Info("tunnel established")
				stableTimer = after(stableAfter)

			case <-stableTimer:
				stableTimer = nil
				// Internal observability. The user-visible confirmation was
				// "tunnel established", a minute ago.
				logger.Debug("attempt stable, backoff reset")
				failures = 0
			}
		}

		if ctx.Err() != nil {
			// The attempt ended because we cancelled it. That is a clean
			// shutdown, not something to report or retry.
			return nil
		}

		if errors.Is(err, ErrPermanent) {
			// Retrying this changes nothing, so it is reported once, in the
			// terms of the thing that is wrong, and handed back. It is not
			// announced as a lost tunnel: nothing was ever up to lose, and
			// the user has a configuration to correct rather than a network
			// to wait for.
			logger.Errorf("cannot continue: %v", err)
			return err
		}

		failures++
		if err != nil {
			logger.Infof("tunnel lost: %v", err)
		} else {
			logger.Info("tunnel lost: the attempt ended without an error")
		}

		// Both give-up paths report the same event the same way: an attempt
		// that ended is a reason to exit non-zero, whichever flag asked us
		// to stop.
		lastFailure := err
		if lastFailure == nil {
			lastFailure = errAttemptEnded
		}
		if s.ExitOnFirst {
			return lastFailure
		}
		if s.MaxAttempts > 0 && failures >= s.MaxAttempts {
			return fmt.Errorf("giving up after %d %s: %w", failures, plural(failures, "attempt"), lastFailure)
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

// plural is here because "giving up after 1 attempts" is user-facing.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
