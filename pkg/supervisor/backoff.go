// Package supervisor runs an attempt that blocks until it fails and retries it
// with exponential backoff, until the caller's give-up policy is reached or the
// context is cancelled.
//
// It knows nothing about tunnels, gRPC or Kubernetes: callers supply an Attempt
// closure that establishes whatever it needs and blocks.
package supervisor

import (
	"math/rand"
	"time"
)

const (
	// DefaultBase is the delay before the first retry.
	DefaultBase = time.Second
	// DefaultMax is the ceiling the doubling delay is clamped to.
	DefaultMax = 30 * time.Second

	// jitterFraction is how much of a delay is randomised, either way. Two
	// clients that lost the same cluster should not come back at the same
	// instant, and a user watching the log should not see a delay so exact
	// it looks like a stuck timer.
	jitterFraction = 0.2
)

// Backoff computes how long to wait before a retry: Base, doubling with each
// consecutive failure, clamped to Max, then spread by +/-20% jitter.
//
// The zero value is usable. Delay is pure and the caller owns the failure
// count, so resetting the backoff is just counting from one again.
type Backoff struct {
	// Base is the delay after a single failure. DefaultBase when zero.
	Base time.Duration
	// Max is the ceiling for the doubling delay. DefaultMax when zero.
	// Jitter is applied after clamping, so a returned delay can exceed Max
	// by up to jitterFraction.
	Max time.Duration

	// rand yields a value in [0,1). Injected by tests so a delay sequence is
	// exact rather than approximately right.
	rand func() float64
}

// Delay returns how long to wait before the retry that follows `failures`
// consecutive failures. failures is 1-based; anything lower is treated as the
// first failure.
func (b Backoff) Delay(failures int) time.Duration {
	base := b.Base
	if base <= 0 {
		base = DefaultBase
	}
	ceiling := b.Max
	if ceiling <= 0 {
		ceiling = DefaultMax
	}
	if ceiling < base {
		ceiling = base
	}

	// Doubling in a loop rather than shifting by failures-1: retrying forever
	// means the failure count really does keep climbing, and a shift past 63
	// wraps to a negative duration -- which would stop the supervisor waiting
	// at all, exactly when it is backing off hardest.
	delay := base
	for i := 1; i < failures && delay < ceiling; i++ {
		delay *= 2
	}
	if delay > ceiling {
		delay = ceiling
	}

	draw := b.rand
	if draw == nil {
		// Spreading retries, not generating secrets.
		draw = rand.Float64 // #nosec G404
	}

	// draw is in [0,1), so the multiplier lands in [0.8, 1.2) and the result
	// stays positive.
	return time.Duration(float64(delay) * (1 + jitterFraction*(2*draw()-1)))
}
