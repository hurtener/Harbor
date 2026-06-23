package governance

// Test-only white-box accessors. Let the _test package assert the
// per-identity cache is bounded (one entry per identity, not per run)
// after the RFC §6.15 identity-scoping fix.

// CostKeysLen returns the number of distinct cache keys the accumulator
// holds. Identity-scoped keying means this counts identities, not runs.
// Delegates to the production CacheLen so the test asserts the same
// accessor the runtime gauge reads.
func CostKeysLen(a *CostAccumulator) int {
	return a.CacheLen()
}

// RateKeysLen returns the number of distinct cache keys the limiter
// holds. Identity-scoped keying means this counts identities, not runs.
// Delegates to the production CacheLen so the test asserts the same
// accessor the runtime gauge reads.
func RateKeysLen(r *RateLimiter) int {
	return r.CacheLen()
}
