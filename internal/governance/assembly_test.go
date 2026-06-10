package governance_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// assemblyTier builds the one-tier Config the assembly tests share.
func assemblyTier(tier governance.TierConfig) governance.Config {
	return governance.Config{
		DefaultTier:   "t",
		IdentityTiers: map[string]governance.TierConfig{"t": tier},
	}
}

// TestNewSubsystemFromConfig_EmptyTiers_ReturnsNilNil pins the D-044
// latent default: an empty (or nil) IdentityTiers map yields no
// Subsystem and no error — the ONE sanctioned "no enforcement" state.
func TestNewSubsystemFromConfig_EmptyTiers_ReturnsNilNil(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	for _, cfg := range []governance.Config{
		{},
		{IdentityTiers: map[string]governance.TierConfig{}},
		{DefaultTier: "ghost"}, // default tier without a tier map is still latent
	} {
		sub, err := governance.NewSubsystemFromConfig(cfg, st, bus)
		if err != nil {
			t.Fatalf("empty tiers must not error, got %v", err)
		}
		if sub != nil {
			t.Fatalf("empty tiers must return a nil Subsystem (D-044 latent default), got %T", sub)
		}
	}

	// Latent also tolerates nil deps — nothing is constructed.
	sub, err := governance.NewSubsystemFromConfig(governance.Config{}, nil, nil)
	if err != nil || sub != nil {
		t.Fatalf("empty tiers + nil deps: got (%v, %v), want (nil, nil)", sub, err)
	}
}

// TestNewSubsystemFromConfig_NilDeps_FailLoud — enforcement without
// persistence or observability is a misconfiguration, not a degraded
// mode (CLAUDE.md §13).
func TestNewSubsystemFromConfig_NilDeps_FailLoud(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	cfg := assemblyTier(governance.TierConfig{BudgetCeilingUSD: 1})

	if _, err := governance.NewSubsystemFromConfig(cfg, nil, bus); !errors.Is(err, governance.ErrInvalidConfig) {
		t.Errorf("nil store: got %v, want ErrInvalidConfig", err)
	}
	if _, err := governance.NewSubsystemFromConfig(cfg, st, nil); !errors.Is(err, governance.ErrInvalidConfig) {
		t.Errorf("nil bus: got %v, want ErrInvalidConfig", err)
	}
}

// TestNewSubsystemFromConfig_ComposeOrder pins the documented
// cheapest-reject-first member order (MaxTokens → RateLimiter →
// CostAccumulator) behaviourally:
//
//   - A request whose MaxTokens exceeds BOTH the tier cap AND the rate
//     bucket capacity fails with ErrMaxTokensExceeded — if the rate
//     limiter ran first, the 100-token drain against a capacity-1
//     bucket would have produced ErrRateLimited.
//   - Once the bucket is empty AND the budget is exhausted, the next
//     call fails with ErrRateLimited — if the cost accumulator ran
//     first, it would have produced ErrBudgetExceeded.
func TestNewSubsystemFromConfig_ComposeOrder(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	cfg := assemblyTier(governance.TierConfig{
		MaxTokens:        10,
		RateLimit:        governance.RateLimitConfig{Capacity: 1}, // one-shot bucket
		BudgetCeilingUSD: 0.01,
	})
	sub, err := governance.NewSubsystemFromConfig(cfg, st, bus)
	if err != nil {
		t.Fatalf("NewSubsystemFromConfig: %v", err)
	}
	ctx := ctxWith(t, "T", "U", "S-order", "R")
	over := 100

	// 1. MaxTokens rejects before the rate limiter ever sees the drain.
	err = sub.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &over})
	if !errors.Is(err, governance.ErrMaxTokensExceeded) {
		t.Fatalf("over-cap request: got %v, want ErrMaxTokensExceeded (MaxTokens must precede RateLimiter)", err)
	}

	// 2. The rejected call drained nothing: a plain call (drain=1)
	// still passes the capacity-1 bucket and the not-yet-exceeded
	// budget.
	if err := sub.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Fatalf("first plain PreCall: %v", err)
	}
	// Accumulate cost past the ceiling.
	if err := sub.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.02}}, nil); err != nil {
		t.Fatalf("PostCall: %v", err)
	}

	// 3. Bucket empty AND budget exceeded → the rate limiter's error
	// wins (RateLimiter precedes CostAccumulator).
	err = sub.PreCall(ctx, llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, governance.ErrRateLimited) {
		t.Fatalf("exhausted bucket + exhausted budget: got %v, want ErrRateLimited (RateLimiter must precede CostAccumulator)", err)
	}
}

// TestNewSubsystemFromConfig_RejectShortCircuits_InnerNeverCalled pins
// that a governance rejection emits NO provider-side request: the
// wrapped inner client's call counter does not move on the rejected
// call (the Phase 111a plan's double-enforcement risk note).
func TestNewSubsystemFromConfig_RejectShortCircuits_InnerNeverCalled(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	sub, err := governance.NewSubsystemFromConfig(
		assemblyTier(governance.TierConfig{BudgetCeilingUSD: 0.5}), st, bus)
	if err != nil {
		t.Fatalf("NewSubsystemFromConfig: %v", err)
	}
	inner := &stubClient{response: llm.CompleteResponse{
		Content: "ok",
		Cost:    llm.Cost{TotalCost: 1.0}, // one call blows the 0.5 ceiling
	}}
	client := governance.Wrap(inner, sub)
	ctx := ctxWith(t, "T", "U", "S-short", "R")

	if _, err := client.Complete(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls after permitted Complete = %d, want 1", inner.calls)
	}
	_, err = client.Complete(ctx, llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("second Complete: got %v, want ErrBudgetExceeded", err)
	}
	if inner.calls != 1 {
		t.Errorf("inner calls after governance reject = %d, want 1 (no provider-side request)", inner.calls)
	}
}

// TestNewSubsystemFromConfig_SharedStore_PersistsAcrossInstances —
// two Subsystems built over the SAME StateStore share persisted
// accumulator state (the restart shape): a budget exhausted through
// instance 1 rejects on instance 2's first PreCall. Independent
// instances over the same store are how per-`llm.Open` factories keep
// continuity without sharing in-memory state.
func TestNewSubsystemFromConfig_SharedStore_PersistsAcrossInstances(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	cfg := assemblyTier(governance.TierConfig{BudgetCeilingUSD: 0.5})
	ctx := ctxWith(t, "T", "U", "S-persist", "R")

	sub1, err := governance.NewSubsystemFromConfig(cfg, st, bus)
	if err != nil {
		t.Fatalf("NewSubsystemFromConfig(1): %v", err)
	}
	if err := sub1.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 1.0}}, nil); err != nil {
		t.Fatalf("PostCall: %v", err)
	}

	sub2, err := governance.NewSubsystemFromConfig(cfg, st, bus)
	if err != nil {
		t.Fatalf("NewSubsystemFromConfig(2): %v", err)
	}
	if sub2 == sub1 {
		t.Fatalf("each call must build an independent Subsystem instance")
	}
	err = sub2.PreCall(ctx, llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Errorf("fresh instance over the shared store: got %v, want ErrBudgetExceeded", err)
	}
}

// TestNewSubsystemFromConfig_ConcurrentReuse — D-025: ONE
// assembly-built Subsystem behind ONE wrapped client shared by N=128
// concurrent goroutines across 8 sessions. No data races (the -race
// gate), no cross-session bleed (a deliberately exhausted session
// rejects while its siblings keep permitting).
func TestNewSubsystemFromConfig_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	sub, err := governance.NewSubsystemFromConfig(assemblyTier(governance.TierConfig{
		BudgetCeilingUSD: 1_000,
		RateLimit:        governance.RateLimitConfig{Capacity: 1_000_000},
		MaxTokens:        1_000_000,
	}), st, bus)
	if err != nil {
		t.Fatalf("NewSubsystemFromConfig: %v", err)
	}
	inner := &concurrentStubClient{response: llm.CompleteResponse{
		Content: "ok",
		Cost:    llm.Cost{TotalCost: 0.0001},
	}}
	client := governance.Wrap(inner, sub)

	const (
		goroutines = 128
		sessions   = 8
	)
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := ctxWith(t, "T", "U", fmt.Sprintf("S-%d", i%sessions), fmt.Sprintf("R-%d", i))
			if _, cErr := client.Complete(ctx, llm.CompleteRequest{Model: "m"}); cErr != nil {
				errs <- fmt.Errorf("goroutine %d: %w", i, cErr)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := inner.calls.Load(); got != goroutines {
		t.Errorf("inner calls = %d, want %d", got, goroutines)
	}

	// Cross-session isolation under the same shared instance: exhaust
	// ONE session's budget; siblings stay clean.
	exhausted := ctxWith(t, "T", "U", "S-0", "R-exhaust")
	if err := sub.PostCall(exhausted, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 2_000}}, nil); err != nil {
		t.Fatalf("PostCall (exhaust): %v", err)
	}
	if _, err := client.Complete(exhausted, llm.CompleteRequest{Model: "m"}); !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Errorf("exhausted session: got %v, want ErrBudgetExceeded", err)
	}
	sibling := ctxWith(t, "T", "U", "S-1", "R-sibling")
	if _, err := client.Complete(sibling, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Errorf("sibling session gated by another session's budget: %v", err)
	}
}

// TestNewSubsystemFromConfig_SnapshotsObserveEnforcementState — the
// inspection accessors (`CostAccumulator.Snapshot`,
// `RateLimiter.Snapshot`) reflect what the assembled enforcement
// recorded. Built from the same constructors the assembly composes.
func TestNewSubsystemFromConfig_SnapshotsObserveEnforcementState(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	cfg := assemblyTier(governance.TierConfig{
		BudgetCeilingUSD: 10,
		RateLimit:        governance.RateLimitConfig{Capacity: 5},
	})
	limiter, err := governance.NewRateLimiter(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	acc, err := governance.NewCostAccumulator(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	sub := governance.NewCompound(governance.NewMaxTokensEnforcer(bus, cfg), limiter, acc)

	ctx := ctxWith(t, "T", "U", "S-snap", "R")
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		t.Fatalf("ctx must carry the quadruple")
	}
	if err := sub.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Fatalf("PreCall: %v", err)
	}
	if err := sub.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.25}}, nil); err != nil {
		t.Fatalf("PostCall: %v", err)
	}

	levels, err := limiter.Snapshot(ctx, q)
	if err != nil {
		t.Fatalf("RateLimiter.Snapshot: %v", err)
	}
	if got := levels["m"]; got != 4 {
		t.Errorf("bucket level after one drain = %d, want 4", got)
	}
	total, byModel, err := acc.Snapshot(ctx, q)
	if err != nil {
		t.Fatalf("CostAccumulator.Snapshot: %v", err)
	}
	if total != 0.25 || byModel["m"] != 0.25 {
		t.Errorf("accumulator snapshot = (%v, %v), want (0.25, m:0.25)", total, byModel)
	}
}

// TestNewCompound_SkipsNilMembers_JoinsPostCallErrors — the Compound
// the assembly returns drops nil members (the latent shape of an
// unconfigured policy) and joins PostCall errors without letting any
// member's failure suppress its siblings.
func TestNewCompound_SkipsNilMembers_JoinsPostCallErrors(t *testing.T) {
	t.Parallel()
	postErr := errors.New("observability failure")
	var siblingRan bool
	failing := &fakeSub{
		postFn: func(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, _ error) error {
			return postErr
		},
	}
	sibling := &fakeSub{
		postFn: func(_ context.Context, _ llm.CompleteRequest, _ llm.CompleteResponse, _ error) error {
			siblingRan = true
			return nil
		},
	}
	sub := governance.NewCompound(nil, failing, nil, sibling)
	ctx := ctxWith(t, "T", "U", "S-compound", "R")

	if err := sub.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Fatalf("PreCall over nil members: %v", err)
	}
	err := sub.PostCall(ctx, llm.CompleteRequest{Model: "m"}, llm.CompleteResponse{}, nil)
	if !errors.Is(err, postErr) {
		t.Errorf("PostCall join: got %v, want wrapped %v", err, postErr)
	}
	if !siblingRan {
		t.Errorf("a failing member must not suppress its siblings' PostCall")
	}
}
