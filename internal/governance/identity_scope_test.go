package governance_test

// Identity-scoping (RFC §6.15): cost ceilings and rate limits are
// per-identity (tenant,user,session), never per-run. Keying by RunID
// both leaked (one cache entry per run, never reaped) and — worse —
// let a caller bypass a ceiling by spreading calls across runs, since
// each run started a fresh per-run total. These tests pin the fix:
// the aggregate accumulates across runs, the cache stays bounded, and
// the persisted total survives a restart.

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/llm"
)

// TestCost_CeilingAggregatesAcrossRuns is the ceiling-bypass regression.
// Two runs under one identity each spend 0.6 against a 1.0 ceiling; the
// third run's PreCall must see the aggregate 1.2 and reject — pre-fix it
// would see a fresh 0 for its own RunID and pass.
func TestCost_CeilingAggregatesAcrossRuns(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 1.0}},
	}
	acc, err := governance.NewCostAccumulator(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	spend := func(run string, cost float64) {
		t.Helper()
		ctx := ctxWith(t, "T", "U", "S", run)
		if err := acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
			llm.CompleteResponse{Cost: llm.Cost{TotalCost: cost}}, nil); err != nil {
			t.Fatalf("PostCall(%s): %v", run, err)
		}
	}
	spend("R1", 0.6)
	spend("R2", 0.6) // same identity, different run

	// A third run's PreCall must observe the aggregate and reject.
	if err := acc.PreCall(ctxWith(t, "T", "U", "S", "R3"), llm.CompleteRequest{Model: "m"}); !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("PreCall(R3): want ErrBudgetExceeded (aggregate across runs), got %v", err)
	}

	// And the cache is bounded to the single identity, not one-per-run.
	if n := governance.CostKeysLen(acc); n != 1 {
		t.Errorf("CostKeysLen = %d across 3 runs of one identity, want 1 (identity-scoped, no per-run leak)", n)
	}
}

// TestCost_AggregateSurvivesRestart proves the persisted record is
// identity-keyed: a fresh accumulator over the same StateStore reloads
// the across-runs total and still enforces the ceiling.
func TestCost_AggregateSurvivesRestart(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 1.0}},
	}
	acc1, err := governance.NewCostAccumulator(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewCostAccumulator(1): %v", err)
	}
	for _, run := range []string{"R1", "R2"} {
		if err := acc1.PostCall(ctxWith(t, "T", "U", "S", run), llm.CompleteRequest{Model: "m"},
			llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.6}}, nil); err != nil {
			t.Fatalf("PostCall(%s): %v", run, err)
		}
	}
	_ = acc1.Close(context.Background())

	// Restart: new accumulator, same StateStore.
	acc2, err := governance.NewCostAccumulator(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewCostAccumulator(2): %v", err)
	}
	defer acc2.Close(context.Background())
	if err := acc2.PreCall(ctxWith(t, "T", "U", "S", "R3"), llm.CompleteRequest{Model: "m"}); !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("PreCall after restart: want ErrBudgetExceeded (reloaded aggregate 1.2), got %v", err)
	}
}

// TestRateLimit_KeysBoundedAcrossRuns proves the rate-limiter cache is
// identity-scoped too: many runs under one identity share one bucket
// entry rather than accreting one per run.
func TestRateLimit_KeysBoundedAcrossRuns(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {RateLimit: governance.RateLimitConfig{Capacity: 1000}}},
	}
	rl, err := governance.NewRateLimiter(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	defer rl.Close(context.Background())

	for _, run := range []string{"R1", "R2", "R3", "R4"} {
		if err := rl.PreCall(ctxWith(t, "T", "U", "S", run), llm.CompleteRequest{Model: "m"}); err != nil {
			t.Fatalf("PreCall(%s): %v", run, err)
		}
	}
	if n := governance.RateKeysLen(rl); n != 1 {
		t.Errorf("RateKeysLen = %d across 4 runs of one identity, want 1 (identity-scoped, no per-run leak)", n)
	}
}
