package supervisor

import (
	"testing"
	"time"
)

// fixedRand returns a randomness source that always yields v, so that a delay
// sequence is exact rather than approximately right. 0.5 is the midpoint of
// [0,1), i.e. no jitter at all.
func fixedRand(v float64) func() float64 {
	return func() float64 { return v }
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	b := Backoff{rand: fixedRand(0.5)}

	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}

	for i, w := range want {
		failures := i + 1
		if got := b.Delay(failures); got != w {
			t.Errorf("Delay(%d) = %s, want %s; the retry sequence users see must double from 1s and stop at 30s", failures, got, w)
		}
	}
}

func TestBackoffDoesNotOverflowAfterALongOutage(t *testing.T) {
	b := Backoff{rand: fixedRand(0.5)}

	// "Retry forever" means the failure count really does keep climbing.
	// Shifting by failures-1 would wrap to a negative duration here and the
	// supervisor would stop waiting at all.
	for _, failures := range []int{64, 65, 1000, 1 << 30} {
		if got := b.Delay(failures); got != DefaultMax {
			t.Errorf("Delay(%d) = %s, want %s", failures, got, DefaultMax)
		}
	}
}

func TestBackoffTreatsNonPositiveFailureCountAsTheFirst(t *testing.T) {
	b := Backoff{rand: fixedRand(0.5)}

	for _, failures := range []int{0, -1} {
		if got := b.Delay(failures); got != DefaultBase {
			t.Errorf("Delay(%d) = %s, want the base delay %s", failures, got, DefaultBase)
		}
	}
}

func TestBackoffJitterStaysWithinTwentyPercent(t *testing.T) {
	tests := []struct {
		name string
		rand float64
		want time.Duration
	}{
		{name: "lowest draw is 20% below", rand: 0, want: 3200 * time.Millisecond},
		{name: "midpoint draw is unchanged", rand: 0.5, want: 4 * time.Second},
		{name: "highest draw is just under 20% above", rand: 0.999999, want: 4800 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := Backoff{rand: fixedRand(tt.rand)}
			got := b.Delay(3) // 4s before jitter

			// The endpoints are what matter: a draw outside +/-20% would
			// mean the spread is wrong, not merely unlucky.
			const tolerance = time.Millisecond
			if diff := got - tt.want; diff > tolerance || diff < -tolerance {
				t.Errorf("Delay(3) with rand=%v = %s, want ~%s", tt.rand, got, tt.want)
			}
		})
	}
}

func TestBackoffDefaultRandomnessIsAppliedAndBounded(t *testing.T) {
	b := Backoff{}
	low, high := 3200*time.Millisecond, 4800*time.Millisecond

	seen := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		got := b.Delay(3)
		if got < low || got > high {
			t.Fatalf("Delay(3) = %s, outside the +/-20%% band [%s, %s]", got, low, high)
		}
		seen[got] = true
	}

	// A zero-value Backoff must still jitter. If the default source were
	// forgotten every client that lost the same tunnel would reconnect in
	// lockstep.
	if len(seen) < 2 {
		t.Errorf("500 delays produced %d distinct value(s); the default randomness source is not being applied", len(seen))
	}
}

func TestBackoffHonoursCustomBaseAndMax(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: 250 * time.Millisecond, rand: fixedRand(0.5)}

	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond}
	for i, w := range want {
		if got := b.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %s, want %s", i+1, got, w)
		}
	}
}

func TestBackoffMaxBelowBaseDoesNotInvertTheDelay(t *testing.T) {
	b := Backoff{Base: 5 * time.Second, Max: time.Second, rand: fixedRand(0.5)}

	if got := b.Delay(1); got != 5*time.Second {
		t.Errorf("Delay(1) = %s, want the base delay 5s; a Max below Base must not shorten the first wait", got)
	}
}
