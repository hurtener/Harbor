package engine

import "time"

// nextBackoff computes the sleep duration before retry attempt
// `attempt` (1-indexed: attempt 1 is the first retry, attempt 0 is
// the initial invocation and never sleeps).
//
// Formula: min(base * mult^attempt + jitter, max)
//
// jitter is uniform on [0, base*0.1) — the rand argument supplies a
// float64 in [0, 1) (math/rand.Float64 in production, deterministic
// in tests). base*0.1 was chosen because it's small enough to avoid
// distorting the geometric growth but large enough to break
// synchronized-retry storms.
//
// Edge cases:
//   - attempt <= 0: returns 0 (no sleep — caller should not invoke).
//   - base <= 0: returns 0 (no sleep configured).
//   - mult <= 0: treated as mult = 1 (no growth — linear retries at base).
//   - max <= 0: no cap (geometric growth uncapped; operators who
//     forget to set Max get exponential blowup, which is loud
//     enough).
//   - rand == nil: jitter is zero (deterministic — useful for tests).
func nextBackoff(attempt int, base, max time.Duration, mult float64, rand func() float64) time.Duration {
	if attempt <= 0 || base <= 0 {
		return 0
	}
	if mult <= 0 {
		mult = 1
	}

	// Geometric growth: base * mult^attempt
	growth := float64(base)
	for range attempt {
		growth *= mult
	}
	// Saturating add to int64 nanoseconds to avoid float overflow at
	// extreme attempt counts.
	if growth > float64(time.Duration(1<<62)) {
		growth = float64(time.Duration(1 << 62))
	}
	d := time.Duration(growth)

	// Jitter: 0..base*0.1
	if rand != nil {
		jitter := time.Duration(float64(base) * 0.1 * rand())
		d += jitter
	}

	if max > 0 && d > max {
		return max
	}
	return d
}
