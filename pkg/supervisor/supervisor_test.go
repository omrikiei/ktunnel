package supervisor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

// errAttemptFailed stands in for whatever kills a tunnel.
var errAttemptFailed = errors.New("rpc error: code = Unavailable")

// fakeClock replaces time.After so tests drive the supervisor's waits instead
// of sleeping through them. Waits are kept in creation order and addressed by
// (duration, nth) because a supervisor has two kinds in flight at once: the
// stability timer of the running attempt and the backoff delay of the last
// failure.
type fakeClock struct {
	mu    sync.Mutex
	waits []fakeWait
}

type fakeWait struct {
	d  time.Duration
	ch chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, fakeWait{d: d, ch: ch})
	return ch
}

// fire releases the nth (1-based) wait of duration d, blocking until the
// supervisor has started it.
func (c *fakeClock) fire(t *testing.T, d time.Duration, nth int) {
	t.Helper()
	c.await(t, d, nth) <- time.Time{}
}

// await blocks until the supervisor has asked to wait for d at least nth
// times, and returns that wait's channel without releasing it.
func (c *fakeClock) await(t *testing.T, d time.Duration, nth int) chan time.Time {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		matched := 0
		for _, w := range c.waits {
			if w.d != d {
				continue
			}
			matched++
			if matched == nth {
				c.mu.Unlock()
				return w.ch
			}
		}
		c.mu.Unlock()

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for wait #%d of %s; waits so far: %v", nth, d, c.durations(0))
		}
		time.Sleep(time.Millisecond)
	}
}

// durations returns the waits requested so far, in order, skipping `except` so
// a test can assert the backoff sequence without the stability timers.
func (c *fakeClock) durations(except time.Duration) []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, 0, len(c.waits))
	for _, w := range c.waits {
		if except != 0 && w.d == except {
			continue
		}
		out = append(out, w.d)
	}
	return out
}

// syncBuffer collects log output written from the supervisor's goroutine while
// the test reads it from its own.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestLogger() (*log.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return &log.Logger{
		Out:       buf,
		Formatter: &log.TextFormatter{DisableColors: true, DisableTimestamp: true},
		Level:     log.InfoLevel,
	}, buf
}

// start runs the supervisor on its own goroutine and reports what Run returned.
func start(ctx context.Context, s *Supervisor) <-chan error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(ctx) }()
	return errCh
}

func waitForRun(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return; it is stuck instead of retrying or giving up")
		return nil
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunExitOnFirstReturnsTheFailureWithoutRetrying(t *testing.T) {
	c := newFakeClock()
	calls := 0
	s := &Supervisor{
		Attempt:     func(context.Context) error { calls++; return errAttemptFailed },
		ExitOnFirst: true,
		after:       c.After,
	}

	err := s.Run(context.Background())

	if !errors.Is(err, errAttemptFailed) {
		t.Errorf("Run returned %v, want the attempt's own error so the caller can exit non-zero on it", err)
	}
	if calls != 1 {
		t.Errorf("attempt ran %d times, want 1: ExitOnFirst must not retry", calls)
	}
	if got := c.durations(0); len(got) > 1 {
		t.Errorf("supervisor waited %v; ExitOnFirst must not wait out a backoff", got)
	}
}

func TestRunExitOnFirstReturnsNilWhenTheAttemptEndsCleanly(t *testing.T) {
	c := newFakeClock()
	s := &Supervisor{
		Attempt:     func(context.Context) error { return nil },
		ExitOnFirst: true,
		after:       c.After,
	}

	if err := s.Run(context.Background()); err != nil {
		t.Errorf("Run returned %v, want nil: an attempt that ended without an error is not a failure", err)
	}
}

func TestRunGivesUpAfterMaxAttempts(t *testing.T) {
	c := newFakeClock()
	calls := 0
	s := &Supervisor{
		Attempt:     func(context.Context) error { calls++; return errAttemptFailed },
		Backoff:     Backoff{rand: fixedRand(0.5)},
		MaxAttempts: 3,
		StableAfter: time.Minute,
		after:       c.After,
	}

	errCh := start(context.Background(), s)
	c.fire(t, time.Second, 1)
	c.fire(t, 2*time.Second, 1)
	err := waitForRun(t, errCh)

	if !errors.Is(err, errAttemptFailed) {
		t.Errorf("Run returned %v, want an error wrapping the last failure so the cause survives", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("Run returned %q, want the number of attempts in the message", err)
	}
	if calls != 3 {
		t.Errorf("attempt ran %d times, want 3 (MaxAttempts)", calls)
	}
	if got, want := c.durations(time.Minute), []time.Duration{time.Second, 2 * time.Second}; !equalDurations(got, want) {
		t.Errorf("backoff delays were %v, want %v", got, want)
	}
}

func TestRunRetriesForeverUntilTheContextIsCancelled(t *testing.T) {
	c := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	s := &Supervisor{
		Attempt: func(context.Context) error {
			calls++
			if calls == 4 {
				cancel()
			}
			return errAttemptFailed
		},
		Backoff:     Backoff{rand: fixedRand(0.5)},
		StableAfter: time.Minute,
		after:       c.After,
	}

	errCh := start(ctx, s)
	c.fire(t, time.Second, 1)
	c.fire(t, 2*time.Second, 1)
	c.fire(t, 4*time.Second, 1)
	err := waitForRun(t, errCh)

	if err != nil {
		t.Errorf("Run returned %v, want nil: cancelling the context is a clean shutdown", err)
	}
	if calls != 4 {
		t.Errorf("attempt ran %d times, want 4: MaxAttempts 0 must keep retrying", calls)
	}
	if got, want := c.durations(time.Minute), []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !equalDurations(got, want) {
		t.Errorf("backoff delays were %v, want %v", got, want)
	}
}

func TestRunReturnsPromptlyWhenCancelledDuringBackoff(t *testing.T) {
	c := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Supervisor{
		Attempt:     func(context.Context) error { return errAttemptFailed },
		Backoff:     Backoff{rand: fixedRand(0.5)},
		StableAfter: time.Minute,
		after:       c.After,
	}

	errCh := start(ctx, s)
	c.await(t, time.Second, 1) // the supervisor is now waiting out the backoff
	cancel()

	// waitForRun never fires the backoff timer: if Run only woke on the
	// delay elapsing, Ctrl+C during a 30-second wait would hang for it.
	if err := waitForRun(t, errCh); err != nil {
		t.Errorf("Run returned %v, want nil on context cancellation", err)
	}
}

func TestRunResetsBackoffAfterAStableAttempt(t *testing.T) {
	c := newFakeClock()
	logger, out := newTestLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan error)
	calls := 0
	s := &Supervisor{
		Attempt: func(context.Context) error {
			calls++
			switch calls {
			case 2:
				// Stays up until the test says otherwise, long enough to
				// cross the stability threshold.
				return <-release
			case 3:
				cancel()
			}
			return errAttemptFailed
		},
		Backoff:     Backoff{rand: fixedRand(0.5)},
		StableAfter: time.Minute,
		Log:         logger,
		after:       c.After,
	}

	errCh := start(ctx, s)
	c.fire(t, time.Second, 1) // first failure: back off 1s, start attempt 2
	c.fire(t, time.Minute, 2) // attempt 2 has now been up long enough to count
	release <- errAttemptFailed
	c.fire(t, time.Second, 2) // must be 1s again, not 2s
	if err := waitForRun(t, errCh); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}

	if got, want := c.durations(time.Minute), []time.Duration{time.Second, time.Second}; !equalDurations(got, want) {
		t.Errorf("backoff delays were %v, want %v: a tunnel that stayed up must not keep growing the delay", got, want)
	}

	logged := out.String()
	for _, want := range []string{
		"tunnel lost: rpc error: code = Unavailable",
		"reconnecting in 1s (attempt 2)",
		"tunnel re-established",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log is missing %q; these lines are what replaces users' wrapper scripts.\nlog:\n%s", want, logged)
		}
	}
}

func TestRunDoesNotAnnounceReestablishmentOnTheFirstAttempt(t *testing.T) {
	c := newFakeClock()
	logger, out := newTestLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan error)
	s := &Supervisor{
		Attempt: func(context.Context) error {
			cancel()
			return <-release
		},
		StableAfter: time.Minute,
		Log:         logger,
		after:       c.After,
	}

	errCh := start(ctx, s)
	c.fire(t, time.Minute, 1)
	release <- nil
	_ = waitForRun(t, errCh)

	if strings.Contains(out.String(), "re-established") {
		t.Errorf("a first attempt that simply stayed up was reported as re-established:\n%s", out.String())
	}
}

func TestRunReportsAnAttemptThatEndedWithoutAnError(t *testing.T) {
	c := newFakeClock()
	logger, out := newTestLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	s := &Supervisor{
		Attempt: func(context.Context) error {
			calls++
			if calls == 2 {
				cancel()
			}
			return nil
		},
		Backoff:     Backoff{rand: fixedRand(0.5)},
		StableAfter: time.Minute,
		Log:         logger,
		after:       c.After,
	}

	errCh := start(ctx, s)
	c.fire(t, time.Second, 1)
	if err := waitForRun(t, errCh); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}

	if calls != 2 {
		t.Errorf("attempt ran %d times, want 2: an attempt that returns nil has still ended and must be retried", calls)
	}
	if !strings.Contains(out.String(), "tunnel lost") {
		t.Errorf("an attempt that ended without an error was not reported:\n%s", out.String())
	}
}

func TestRunGivingUpOnANilFailureStillReportsWhy(t *testing.T) {
	c := newFakeClock()
	s := &Supervisor{
		Attempt:     func(context.Context) error { return nil },
		Backoff:     Backoff{rand: fixedRand(0.5)},
		MaxAttempts: 2,
		StableAfter: time.Minute,
		after:       c.After,
	}

	errCh := start(context.Background(), s)
	c.fire(t, time.Second, 1)
	err := waitForRun(t, errCh)

	if err == nil {
		t.Fatal("Run returned nil after exhausting MaxAttempts; the caller has nothing to exit non-zero on")
	}
	if strings.Contains(err.Error(), "%!w") {
		t.Errorf("Run returned %q: a nil failure was formatted as a wrapped error", err)
	}
}

func TestRunReturnsNilWhenTheContextIsAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	s := &Supervisor{
		Attempt: func(context.Context) error { calls++; return errAttemptFailed },
		after:   newFakeClock().After,
	}

	if err := s.Run(ctx); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if calls != 0 {
		t.Errorf("attempt ran %d times, want 0: there is no point dialling for a context that is already done", calls)
	}
}

func TestRunWithoutALoggerDoesNotPanic(t *testing.T) {
	s := &Supervisor{
		Attempt:     func(context.Context) error { return errAttemptFailed },
		ExitOnFirst: true,
		after:       newFakeClock().After,
	}

	if err := s.Run(context.Background()); !errors.Is(err, errAttemptFailed) {
		t.Errorf("Run returned %v, want the attempt's error", err)
	}
}

func TestRunWaitsOnRealTimeWhenNoClockIsInjected(t *testing.T) {
	calls := 0
	s := &Supervisor{
		Attempt: func(context.Context) error { calls++; return errAttemptFailed },
		// Short enough that the real wait costs the suite nothing, long
		// enough that a supervisor which never waited would be a bug.
		Backoff:     Backoff{Base: time.Millisecond, Max: time.Millisecond},
		MaxAttempts: 2,
		StableAfter: time.Hour,
	}

	if err := s.Run(context.Background()); !errors.Is(err, errAttemptFailed) {
		t.Errorf("Run returned %v, want an error wrapping the last failure", err)
	}
	if calls != 2 {
		t.Errorf("attempt ran %d times, want 2", calls)
	}
}

func TestRunWithoutAnAttemptIsAnError(t *testing.T) {
	s := &Supervisor{}

	if err := s.Run(context.Background()); err == nil {
		t.Error("Run returned nil with no Attempt configured, which would look like a clean exit")
	}
}
