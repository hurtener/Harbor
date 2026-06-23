package governance

// Test-only white-box accessors. Let the _test package assert the
// per-identity cache is bounded (one entry per identity, not per run)
// after the RFC §6.15 identity-scoping fix.

// CostKeysLen returns the number of distinct cache keys the accumulator
// holds. Identity-scoped keying means this counts identities, not runs.
func CostKeysLen(a *CostAccumulator) int {
	n := 0
	a.keys.Range(func(_, _ any) bool { n++; return true })
	return n
}

// RateKeysLen returns the number of distinct cache keys the limiter
// holds. Identity-scoped keying means this counts identities, not runs.
func RateKeysLen(r *RateLimiter) int {
	n := 0
	r.keys.Range(func(_, _ any) bool { n++; return true })
	return n
}
