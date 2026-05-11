package strategy

// Test-only exports — only compiled into _test binaries. Used by
// the same-package tests in recovery_test.go to drive the recovery
// loop's drain path without waiting on the 10s ticker.

import (
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
)

// DrainBacklogsForTest triggers one iteration of the recovery
// loop's drain step. Exposed only in _test.go builds.
//
// Returns the number of keys with non-empty backlog at the start
// of the call (post-drain count may be smaller).
func (e *rollingSummaryExec) DrainBacklogsForTest() int {
	pre := 0
	e.keys.Range(func(_, v any) bool {
		ks := v.(*rollingKeyState)
		ks.mu.Lock()
		if len(ks.backlog) > 0 {
			pre++
		}
		ks.mu.Unlock()
		return true
	})
	e.drainBacklogs()
	return pre
}

// BacklogSize returns the per-key backlog length. Exposed only in
// _test.go builds.
func (e *rollingSummaryExec) BacklogSize(id identity.Quadruple) int {
	v, ok := e.keys.Load(quadKeyFor(id))
	if !ok {
		return 0
	}
	ks := v.(*rollingKeyState)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return len(ks.backlog)
}

// HealthForTest returns the per-key health for tests.
func (e *rollingSummaryExec) HealthForTest(id identity.Quadruple) memory.Health {
	v, ok := e.keys.Load(quadKeyFor(id))
	if !ok {
		return ""
	}
	ks := v.(*rollingKeyState)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.health == "" {
		return memory.HealthHealthy
	}
	return ks.health
}

// AsRollingSummary asserts the executor is a rolling-summary
// executor and returns the concrete type. Returns nil + false
// otherwise. Exposed only in _test.go builds.
func AsRollingSummary(exec StrategyExecutor) (*rollingSummaryExec, bool) {
	r, ok := exec.(*rollingSummaryExec)
	return r, ok
}
