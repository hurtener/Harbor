package engine

import (
	"testing"
	"time"
)

// TestBackoff_Math_Enumerated pins the formula across attempt counts
// 0..5 with deterministic jitter (rand returns 0). A change to the
// math has to update this table — that's the gate.
func TestBackoff_Math_Enumerated(t *testing.T) {
	t.Parallel()
	base := 100 * time.Millisecond
	mult := 2.0
	max := 5 * time.Second
	zeroJitter := func() float64 { return 0 }

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 0},                       // initial invocation never sleeps
		{1, 200 * time.Millisecond},  // 100 * 2^1
		{2, 400 * time.Millisecond},  // 100 * 2^2
		{3, 800 * time.Millisecond},  // 100 * 2^3
		{4, 1600 * time.Millisecond}, // 100 * 2^4
		{5, 3200 * time.Millisecond}, // 100 * 2^5
	}
	for _, c := range cases {
		got := nextBackoff(c.attempt, base, max, mult, zeroJitter)
		if got != c.want {
			t.Errorf("attempt=%d: got=%s want=%s", c.attempt, got, c.want)
		}
	}
}

// TestBackoff_RespectsMax caps geometric growth.
func TestBackoff_RespectsMax(t *testing.T) {
	t.Parallel()
	base := 100 * time.Millisecond
	max := 500 * time.Millisecond
	zeroJitter := func() float64 { return 0 }
	got := nextBackoff(10, base, max, 2.0, zeroJitter) // would be ~102.4s uncapped
	if got != max {
		t.Errorf("got=%s want=%s (cap)", got, max)
	}
}

func TestBackoff_ZeroAttempt_ReturnsZero(t *testing.T) {
	t.Parallel()
	if nextBackoff(0, 100*time.Millisecond, 0, 2.0, nil) != 0 {
		t.Error("attempt=0 must return 0")
	}
}

func TestBackoff_ZeroBase_ReturnsZero(t *testing.T) {
	t.Parallel()
	if nextBackoff(3, 0, 0, 2.0, nil) != 0 {
		t.Error("base=0 must return 0")
	}
}

func TestBackoff_NegativeMult_TreatedAsLinear(t *testing.T) {
	t.Parallel()
	base := 100 * time.Millisecond
	got := nextBackoff(3, base, 0, 0, func() float64 { return 0 })
	if got != base {
		t.Errorf("mult=0 → linear: got=%s want=%s", got, base)
	}
}

func TestBackoff_JitterAddsBoundedNoise(t *testing.T) {
	t.Parallel()
	base := 100 * time.Millisecond
	mult := 2.0
	// Max jitter (rand=1.0) adds base*0.1 = 10ms.
	got := nextBackoff(1, base, 0, mult, func() float64 { return 0.999999 })
	want := 200*time.Millisecond + time.Duration(float64(base)*0.1*0.999999)
	if got != want {
		t.Errorf("max-jitter: got=%s want=%s", got, want)
	}
}
