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

// testStableAfter is an hour so that no backoff delay a test configures --
// capped at 30s by default -- can collide with it, which is what lets the fake
// clock tell a stability timer from a backoff delay.
const testStableAfter = time.Hour

type waitKind int

const (
	backoffWait waitKind = iota
	stabilityWait
)

// fakeClock replaces time.After so tests drive the supervisor's waits instead
// of sleeping through them. Waits are kept in creation order and addressed by
// (duration, nth), because a supervisor can have two in flight at once: the
// stability timer of an established attempt and the backoff delay of the last
// failure.
type fakeClock struct {
	mu    sync.Mutex
	waits []fakeWait
}

type fakeWait struct {
	kind waitKind
	d    time.Duration
	ch   chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{} }

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	kind := backoffWait
	if d == testStableAfter {
		kind = stabilityWait
	}

	ch := make(chan time.Time, 1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, fakeWait{kind: kind, d: d, ch: ch})
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
			t.Fatalf("timed out waiting for wait #%d of %s; backoff delays so far: %v", nth, d, c.backoffDelays())
		}
		time.Sleep(time.Millisecond)
	}
}

// backoffDelays returns the retry delays the supervisor waited out, in order.
func (c *fakeClock) backoffDelays() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, 0, len(c.waits))
	for _, w := range c.waits {
		if w.kind == backoffWait {
			out = append(out, w.d)
		}
	}
	return out
}

// stabilityWaits counts the stability timers started, i.e. how many attempts
// reported themselves established.
func (c *fakeClock) stabilityWaits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, w := range c.waits {
		if w.kind == stabilityWait {
			n++
		}
	}
	return n
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

func newTestLogger(level log.Level) (*log.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return &log.Logger{
		Out:       buf,
		Formatter: &log.TextFormatter{DisableColors: true, DisableTimestamp: true},
		Level:     level,
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

func assertLogged(t *testing.T, out *syncBuffer, want ...string) {
	t.Helper()
	logged := out.String()
	for _, w := range want {
		if !strings.Contains(logged, w) {
			t.Errorf("log is missing %q; these lines are what replaces users' wrapper scripts.\nlog:\n%s", w, logged)
		}
	}
}

func TestRunExitOnFirstReturnsTheFailureWithoutRetrying(t *testing.T) {
	c := newFakeClock()
	calls := 0
	s := &Supervisor{
		Attempt:     func(context.Context, func()) error { calls++; return errAttemptFailed },
		ExitOnFirst: true,
		StableAfter: testStableAfter,
		after:       c.After,
	}

	err := s.Run(context.Background())

	if !errors.Is(err, errAttemptFailed) {
		t.Errorf("Run returned %v, want the attempt's own error so the caller can exit non-zero on it", err)
	}
	if calls != 1 {
		t.Errorf("attempt ran %d times, want 1: ExitOnFirst must not retry", calls)
	}
	if got := c.backoffDelays(); len(got) > 0 {
		t.Errorf("supervisor waited %v; ExitOnFirst must not wait out a backoff", got)
	}
}

func TestRunExitOnFirstReportsAnAttemptThatEndedCleanly(t *testing.T) {
	c := newFakeClock()
	s := &Supervisor{
		Attempt:     func(context.Context, func()) error { return nil },
		ExitOnFirst: true,
		StableAfter: testStableAfter,
		after:       c.After,
	}

	err := s.Run(context.Background())

	// --exit-on-disconnect exists to give a process supervisor a non-zero
	// exit. A tunnel that ended is a disconnect however it ended, so this
	// must not disagree with the MaxAttempts path about the same event.
	if err == nil {
		t.Fatal("Run returned nil for an attempt that ended; the caller has nothing to exit non-zero on")
	}
	if !errors.Is(err, errAttemptEnded) {
		t.Errorf("Run returned %v, want an error explaining that the attempt ended without one", err)
	}
}

func TestRunExitOnFirstOutranksMaxAttempts(t *testing.T) {
	c := newFakeClock()
	calls := 0
	s := &Supervisor{
		Attempt:     func(context.Context, func()) error { calls++; return errAttemptFailed },
		ExitOnFirst: true,
		MaxAttempts: 5,
		StableAfter: testStableAfter,
		after:       c.After,
	}

	if err := s.Run(context.Background()); !errors.Is(err, errAttemptFailed) {
		t.Errorf("Run returned %v, want the first failure", err)
	}
	if calls != 1 {
		t.Errorf("attempt ran %d times, want 1: ExitOnFirst outranks MaxAttempts", calls)
	}
}

func TestRunGivesUpAfterMaxAttempts(t *testing.T) {
	c := newFakeClock()
	calls := 0
	s := &Supervisor{
		Attempt:     func(context.Context, func()) error { calls++; return errAttemptFailed },
		Backoff:     Backoff{rand: fixedRand(0.5)},
		MaxAttempts: 3,
		StableAfter: testStableAfter,
		after:       c.After,
	}

	errCh := start(context.Background(), s)
	c.fire(t, time.Second, 1)
	c.fire(t, 2*time.Second, 1)
	err := waitForRun(t, errCh)

	if !errors.Is(err, errAttemptFailed) {
		t.Errorf("Run returned %v, want an error wrapping the last failure so the cause survives", err)
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Errorf("Run returned %q, want the number of attempts in the message", err)
	}
	if calls != 3 {
		t.Errorf("attempt ran %d times, want 3 (MaxAttempts)", calls)
	}
	if got, want := c.backoffDelays(), []time.Duration{time.Second, 2 * time.Second}; !equalDurations(got, want) {
		t.Errorf("backoff delays were %v, want %v", got, want)
	}
}

func TestRunGiveUpMessageIsSingularForOneAttempt(t *testing.T) {
	c := newFakeClock()
	s := &Supervisor{
		Attempt:     func(context.Context, func()) error { return errAttemptFailed },
		MaxAttempts: 1,
		StableAfter: testStableAfter,
		after:       c.After,
	}

	err := s.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil after exhausting MaxAttempts")
	}
	if strings.Contains(err.Error(), "1 attempts") {
		t.Errorf("Run returned %q; this message is user-facing", err)
	}
}

func TestRunRetriesForeverUntilTheContextIsCancelled(t *testing.T) {
	c := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	s := &Supervisor{
		Attempt: func(context.Context, func()) error {
			calls++
			if calls == 4 {
				cancel()
			}
			return errAttemptFailed
		},
		Backoff:     Backoff{rand: fixedRand(0.5)},
		StableAfter: testStableAfter,
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
	if got, want := c.backoffDelays(), []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !equalDurations(got, want) {
		t.Errorf("backoff delays were %v, want %v", got, want)
	}
}

func TestRunReturnsPromptlyWhenCancelledDuringBackoff(t *testing.T) {
	c := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Supervisor{
		Attempt:     func(context.Context, func()) error { return errAttemptFailed },
		Backoff:     Backoff{rand: fixedRand(0.5)},
		StableAfter: testStableAfter,
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

func TestRunWaitsForTheAttemptToReturnBeforeShuttingDown(t *testing.T) {
	c := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	running := make(chan struct{})
	release := make(chan struct{})
	s := &Supervisor{
		Attempt: func(context.Context, func()) error {
			close(running)
			<-release
			return nil
		},
		StableAfter: testStableAfter,
		after:       c.After,
	}

	errCh := start(ctx, s)
	<-running
	cancel()

	// The attempt still holds its local port here. Returning now would let
	// the caller tear down or retry underneath it, and the next attempt
	// would fail with "address already in use" -- and so would every one
	// after it.
	select {
	case err := <-errCh:
		t.Fatalf("Run returned %v while the attempt was still running; it must wait for the attempt to release what it holds", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if err := waitForRun(t, errCh); err != nil {
		t.Errorf("Run returned %v, want nil once the attempt had returned", err)
	}
}

func TestRunResetsBackoffAfterAStableAttempt(t *testing.T) {
	c := newFakeClock()
	logger, out := newTestLogger(log.DebugLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan error)
	calls := 0
	s := &Supervisor{
		Attempt: func(_ context.Context, established func()) error {
			calls++
			switch calls {
			case 2:
				// Up, and stays up until the test says otherwise.
				established()
				return <-release
			case 3:
				cancel()
			}
			return errAttemptFailed
		},
		Backoff:     Backoff{rand: fixedRand(0.5)},
		StableAfter: testStableAfter,
		Log:         logger,
		after:       c.After,
	}

	errCh := start(ctx, s)
	c.fire(t, time.Second, 1)     // first failure: back off 1s, start attempt 2
	c.fire(t, testStableAfter, 1) // attempt 2 has now been up long enough to count
	release <- errAttemptFailed   // and only then dies
	c.fire(t, time.Second, 2)     // so the delay must be 1s again, not 2s
	if err := waitForRun(t, errCh); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}

	if got, want := c.backoffDelays(), []time.Duration{time.Second, time.Second}; !equalDurations(got, want) {
		t.Errorf("backoff delays were %v, want %v: a tunnel that stayed up must not keep growing the delay", got, want)
	}

	assertLogged(t, out,
		"tunnel lost: rpc error: code = Unavailable",
		"reconnecting in 1s (attempt 2)",
		"tunnel established",
		"attempt stable, backoff reset",
	)
}

func TestRunStableAttemptClearsTheMaxAttemptsStreak(t *testing.T) {
	c := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan error)
	calls := 0
	s := &Supervisor{
		Attempt: func(_ context.Context, established func()) error {
			calls++
			if calls == 2 {
				established()
				return <-release
			}
			return errAttemptFailed
		},
		Backoff:     Backoff{rand: fixedRand(0.5)},
		MaxAttempts: 2,
		StableAfter: testStableAfter,
		after:       c.After,
	}

	errCh := start(ctx, s)
	c.fire(t, time.Second, 1)
	c.fire(t, testStableAfter, 1)
	release <- errAttemptFailed
	c.fire(t, time.Second, 2)
	err := waitForRun(t, errCh)

	if err == nil {
		t.Fatal("Run returned nil, want a give-up error after two consecutive failures")
	}
	// Two failures either side of a tunnel that worked for a while are not
	// a streak. Without the reset the third attempt never happens and
	// --max-reconnect-attempts fires on a link that is merely flaky.
	if calls != 3 {
		t.Errorf("attempt ran %d times, want 3: a stable attempt must clear the MaxAttempts streak", calls)
	}
}

func TestRunDoesNotTreatASlowFailureAsStable(t *testing.T) {
	c := newFakeClock()
	calls := 0
	s := &Supervisor{
		// Never reports itself established: this is the attempt that spends
		// 75 seconds in connect() against a dead network before failing.
		Attempt:     func(context.Context, func()) error { calls++; return errAttemptFailed },
		Backoff:     Backoff{rand: fixedRand(0.5)},
		MaxAttempts: 3,
		StableAfter: testStableAfter,
		after:       c.After,
	}

	errCh := start(context.Background(), s)
	c.fire(t, time.Second, 1)
	c.fire(t, 2*time.Second, 1)
	err := waitForRun(t, errCh)

	// Timing an attempt from its launch would let a slow failure cross
	// StableAfter, reset the backoff to 1s and clear the streak -- a hot
	// retry loop that also logs the opposite of what happened.
	if n := c.stabilityWaits(); n != 0 {
		t.Errorf("supervisor started %d stability timer(s) for an attempt that never reported itself up; only an established attempt can become stable", n)
	}
	if got, want := c.backoffDelays(), []time.Duration{time.Second, 2 * time.Second}; !equalDurations(got, want) {
		t.Errorf("backoff delays were %v, want %v: a slow failure must not reset the backoff", got, want)
	}
	if calls != 3 || err == nil {
		t.Errorf("attempt ran %d times and Run returned %v, want 3 and a give-up error", calls, err)
	}
}

func TestRunReportsEstablishmentEvenWhenTheAttemptFailsImmediately(t *testing.T) {
	c := newFakeClock()
	logger, out := newTestLogger(log.InfoLevel)
	s := &Supervisor{
		Attempt: func(_ context.Context, established func()) error {
			established()
			return errAttemptFailed
		},
		MaxAttempts: 1,
		StableAfter: testStableAfter,
		Log:         logger,
		after:       c.After,
	}

	if err := s.Run(context.Background()); err == nil {
		t.Fatal("Run returned nil, want a give-up error")
	}

	// A tunnel that came up and died a moment later must still be reported
	// as having come up; otherwise the log makes it look like it never did.
	// Run almost always observes the establishment signal first here, so
	// this asserts the common path only. The rare opposite interleaving is
	// what TestRunReportsEstablishmentWhenTheFailureIsObservedFirst covers.
	assertLogged(t, out, "tunnel established", "tunnel lost")
	if n := strings.Count(out.String(), "tunnel established"); n != 1 {
		t.Errorf("logged establishment %d times, want once", n)
	}
}

// countingWriter tallies log lines containing a phrase. Cheaper than
// accumulating and parsing megabytes of buffer in the stress test below.
type countingWriter struct {
	phrase string
	n      int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(w.phrase)) {
		w.n++
	}
	return len(p), nil
}

func TestRunReportsEstablishmentWhenTheFailureIsObservedFirst(t *testing.T) {
	// An attempt can report itself up and then fail before Run gets back to
	// its select, leaving both signals ready and the choice between them to
	// the runtime. Run parks on the select long before that happens unless
	// it is preempted in the few statements after launching the attempt, so
	// the interleaving needs scheduling pressure to show up at all: it does
	// not occur in a single-supervisor test. Without the drain that handles
	// it, "tunnel established" goes missing from the log entirely for those
	// attempts -- silently, and only on a loaded machine.
	//
	// The width is what makes this reliable rather than decorative: with the
	// drain removed, 64x400 loses 30-39 of the 25600 establishments on every
	// run (8 of 8 measured, -race), while a single supervisor loses none in
	// hundreds. Lower the parallelism and this test stops failing when it
	// should. It costs ~0.12s.
	const (
		workers = 64
		cycles  = 400
	)

	// The stability timer is irrelevant here: every attempt fails at once.
	never := make(chan time.Time)
	after := func(time.Duration) <-chan time.Time { return never }

	counts := make([]int, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()

			// Per worker, so 64 goroutines do not serialise on one logger
			// mutex and lose the pressure this test exists to create.
			counter := &countingWriter{phrase: "tunnel established"}
			s := &Supervisor{
				Attempt: func(_ context.Context, established func()) error {
					established()
					return errAttemptFailed
				},
				ExitOnFirst: true,
				Log: &log.Logger{
					Out:       counter,
					Formatter: &log.TextFormatter{DisableColors: true, DisableTimestamp: true},
					Level:     log.InfoLevel,
				},
				after: after,
			}

			for i := 0; i < cycles; i++ {
				_ = s.Run(context.Background())
			}
			counts[w] = counter.n
		}(w)
	}
	wg.Wait()

	total := 0
	for _, n := range counts {
		total += n
	}
	if want := workers * cycles; total != want {
		t.Errorf("%d of %d attempts came up and failed without their establishment being logged; the log is what users grep instead of running wrapper scripts", want-total, want)
	}
}

func TestRunEstablishedIsSafeToCallRepeatedlyAndAfterCancellation(t *testing.T) {
	c := newFakeClock()
	logger, out := newTestLogger(log.InfoLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Supervisor{
		Attempt: func(ctx context.Context, established func()) error {
			// Real closures will be messier than "call it exactly once":
			// none of this may block, panic or double-report.
			established()
			established()
			cancel()
			<-ctx.Done()
			established()
			return errAttemptFailed
		},
		StableAfter: testStableAfter,
		Log:         logger,
		after:       c.After,
	}

	if err := waitForRun(t, start(ctx, s)); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
	if n := strings.Count(out.String(), "tunnel established"); n != 1 {
		t.Errorf("logged establishment %d times, want once however often the attempt reports it", n)
	}
}

func TestRunReportsAnAttemptThatEndedWithoutAnError(t *testing.T) {
	c := newFakeClock()
	logger, out := newTestLogger(log.InfoLevel)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	s := &Supervisor{
		Attempt: func(context.Context, func()) error {
			calls++
			if calls == 2 {
				cancel()
			}
			return nil
		},
		Backoff:     Backoff{rand: fixedRand(0.5)},
		StableAfter: testStableAfter,
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
	assertLogged(t, out, "tunnel lost")
}

func TestRunGivingUpOnANilFailureStillReportsWhy(t *testing.T) {
	c := newFakeClock()
	s := &Supervisor{
		Attempt:     func(context.Context, func()) error { return nil },
		Backoff:     Backoff{rand: fixedRand(0.5)},
		MaxAttempts: 2,
		StableAfter: testStableAfter,
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
	if !errors.Is(err, errAttemptEnded) {
		t.Errorf("Run returned %q, want it to wrap the reason the last attempt ended", err)
	}
}

func TestRunReturnsNilWhenTheContextIsAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	s := &Supervisor{
		Attempt: func(context.Context, func()) error { calls++; return errAttemptFailed },
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
		Attempt: func(_ context.Context, established func()) error {
			established()
			return errAttemptFailed
		},
		ExitOnFirst: true,
		StableAfter: testStableAfter,
		after:       newFakeClock().After,
	}

	if err := s.Run(context.Background()); !errors.Is(err, errAttemptFailed) {
		t.Errorf("Run returned %v, want the attempt's error", err)
	}
}

func TestRunWaitsOnRealTimeWhenNoClockIsInjected(t *testing.T) {
	calls := 0
	s := &Supervisor{
		Attempt: func(context.Context, func()) error { calls++; return errAttemptFailed },
		// Short enough that the real wait costs the suite nothing, long
		// enough that a supervisor which never waited would be a bug.
		Backoff:     Backoff{Base: time.Millisecond, Max: time.Millisecond},
		MaxAttempts: 2,
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
