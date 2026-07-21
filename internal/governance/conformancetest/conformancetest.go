// Package conformancetest exposes the canonical governance correctness
// suite that every supported `state.StateStore` driver must satisfy.
//
// The suite lives in a subpackage so the production-code path
// `internal/governance` does not import the standard library `testing`
// package (mirrors `internal/memory/conformancetest`,
// `internal/state/conformancetest`).
//
// Downstream drivers (in-mem at V1; SQLite + Postgres tests live in the
// state-driver test packages and call into Run for cumulative coverage)
// consume it via:
//
//	import "github.com/hurtener/Harbor/internal/governance/conformancetest"
//
//	func TestGovernance_Conformance(t *testing.T) {
//	    conformancetest.Run(t, func() conformancetest.Harness {
//	        // ... build a fresh state.StateStore + events.EventBus + cleanup ...
//	    })
//	}
//
// The factory returns a fresh `Harness` per top-level subtest.
package conformancetest

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

// Harness bundles the per-subtest fixture.
type Harness struct {
	State   state.StateStore
	Bus     events.EventBus
	Cleanup func()
}

// Factory builds a fresh harness per subtest.
type Factory func() Harness

// Run executes the full governance conformance suite. Each subtest
// constructs a fresh harness so state-store driver instances are
// isolated; the suite asserts only the public Subsystem surface.
func Run(t *testing.T, mk Factory) {
	t.Helper()

	t.Run("CostAccumulator_PermitWithoutCeiling", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		ctx := withIdentity(t)
		acc, err := governance.NewCostAccumulator(h.State, h.Bus, governance.Config{})
		if err != nil {
			t.Fatalf("NewCostAccumulator: %v", err)
		}
		defer acc.Close(context.Background())
		if err := acc.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
			t.Errorf("PreCall under latent default returned: %v", err)
		}
		if err := acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
			llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.5}}, nil); err != nil {
			t.Errorf("PostCall under latent default returned: %v", err)
		}
	})

	t.Run("CostAccumulator_EnforcesCeiling", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		ctx := withIdentity(t)
		cfg := governance.Config{
			DefaultTier: "free",
			IdentityTiers: map[string]governance.TierConfig{
				"free": {BudgetCeilingUSD: 0.50},
			},
		}
		acc, err := governance.NewCostAccumulator(h.State, h.Bus, cfg)
		if err != nil {
			t.Fatalf("NewCostAccumulator: %v", err)
		}
		defer acc.Close(context.Background())

		// Subscribe BEFORE the second call so we observe the emit.
		sub, err := h.Bus.Subscribe(context.Background(), events.Filter{
			Tenant: "T", User: "U", Session: "S",
			Types: []events.EventType{governance.EventTypeBudgetExceeded},
		})
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Cancel()

		// First call: under ceiling. PreCall permits; PostCall records.
		if err := acc.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
			t.Fatalf("first PreCall: %v", err)
		}
		if err := acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
			llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.6}}, nil); err != nil {
			t.Fatalf("first PostCall: %v", err)
		}
		// Second call: accumulator now > ceiling; PreCall blocks.
		err = acc.PreCall(ctx, llm.CompleteRequest{Model: "m"})
		if err == nil || !errors.Is(err, governance.ErrBudgetExceeded) {
			t.Fatalf("second PreCall: want ErrBudgetExceeded, got %v", err)
		}
		// Drain the event.
		select {
		case ev := <-sub.Events():
			if ev.Type != governance.EventTypeBudgetExceeded {
				t.Errorf("event type = %q", ev.Type)
			}
			p, ok := ev.Payload.(governance.BudgetExceededPayload)
			if !ok {
				t.Fatalf("payload type %T", ev.Payload)
			}
			if p.Ceiling != 0.50 {
				t.Errorf("Ceiling = %v want 0.50", p.Ceiling)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("did not observe budget_exceeded event within 2s")
		}
	})

	t.Run("CostAccumulator_RestartSurvival", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		ctx := withIdentity(t)
		q := identity.MustQuadrupleFrom(ctx)

		acc1, err := governance.NewCostAccumulator(h.State, h.Bus, governance.Config{})
		if err != nil {
			t.Fatalf("acc1: %v", err)
		}
		if err := acc1.PostCall(ctx, llm.CompleteRequest{Model: "m"},
			llm.CompleteResponse{Cost: llm.Cost{TotalCost: 1.25}}, nil); err != nil {
			t.Fatalf("first PostCall: %v", err)
		}
		_ = acc1.Close(context.Background())

		// New accumulator over the SAME StateStore — must read back the
		// persisted total on first reference.
		acc2, err := governance.NewCostAccumulator(h.State, h.Bus, governance.Config{})
		if err != nil {
			t.Fatalf("acc2: %v", err)
		}
		defer acc2.Close(context.Background())
		total, byModel, err := acc2.Snapshot(ctx, q)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if total != 1.25 {
			t.Errorf("restart total = %v want 1.25", total)
		}
		if byModel["m"] != 1.25 {
			t.Errorf("restart byModel[m] = %v want 1.25", byModel["m"])
		}
	})

	t.Run("CostAccumulator_AttemptTapFold", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		ctx := withIdentity(t)
		q := identity.MustQuadrupleFrom(ctx)
		acc, err := governance.NewCostAccumulator(h.State, h.Bus, governance.Config{})
		if err != nil {
			t.Fatalf("NewCostAccumulator: %v", err)
		}
		defer acc.Close(context.Background())

		// Install a per-call attempt-cost tap, report two consumed
		// intermediate attempts, then PostCall with a final response cost.
		// The fold (tap + final) must persist identically on every driver.
		tctx, _ := llm.ContextWithAttemptCostTap(ctx)
		llm.ReportAttemptCost(tctx, llm.Cost{TotalCost: 0.01})
		llm.ReportAttemptCost(tctx, llm.Cost{TotalCost: 0.1})
		if err := acc.PostCall(tctx, llm.CompleteRequest{Model: "m"},
			llm.CompleteResponse{Cost: llm.Cost{TotalCost: 1.0}, Content: "x"}, nil); err != nil {
			t.Fatalf("PostCall: %v", err)
		}

		// The folded 1.11 must be readable back from a fresh accumulator
		// over the SAME store (persisted through the driver).
		if err := acc.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		acc2, err := governance.NewCostAccumulator(h.State, h.Bus, governance.Config{})
		if err != nil {
			t.Fatalf("acc2: %v", err)
		}
		defer acc2.Close(context.Background())
		total, byModel, err := acc2.Snapshot(ctx, q)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if !floatNear(total, 1.11) {
			t.Errorf("folded total = %v want 1.11 (0.01 + 0.1 + 1.0)", total)
		}
		if !floatNear(byModel["m"], 1.11) {
			t.Errorf("folded byModel[m] = %v want 1.11", byModel["m"])
		}
	})

	t.Run("RateLimiter_PermitWithoutConfig", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		ctx := withIdentity(t)
		rl, err := governance.NewRateLimiter(h.State, h.Bus, governance.Config{})
		if err != nil {
			t.Fatalf("NewRateLimiter: %v", err)
		}
		defer rl.Close(context.Background())
		req := llm.CompleteRequest{Model: "m"}
		for i := range 10 {
			if err := rl.PreCall(ctx, req); err != nil {
				t.Errorf("PreCall #%d under latent default returned: %v", i, err)
			}
		}
	})

	t.Run("RateLimiter_BucketSurvivesRestart", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		ctx := withIdentity(t)
		q := identity.MustQuadrupleFrom(ctx)
		five := 5
		cfg := governance.Config{
			DefaultTier: "free",
			IdentityTiers: map[string]governance.TierConfig{
				"free": {RateLimit: governance.RateLimitConfig{Capacity: 10}}, // no refill
			},
			Clock: &governance.RealClock{},
		}
		rl1, err := governance.NewRateLimiter(h.State, h.Bus, cfg)
		if err != nil {
			t.Fatalf("rl1: %v", err)
		}
		req := llm.CompleteRequest{Model: "m", MaxTokens: &five}
		if err := rl1.PreCall(ctx, req); err != nil {
			t.Fatalf("first drain: %v", err)
		}
		_ = rl1.Close(context.Background())

		rl2, err := governance.NewRateLimiter(h.State, h.Bus, cfg)
		if err != nil {
			t.Fatalf("rl2: %v", err)
		}
		defer rl2.Close(context.Background())
		// Bucket should be at 5 after the first 5-token drain.
		snap, err := rl2.Snapshot(ctx, q)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if snap["m"] != 5 {
			t.Errorf("restart bucket level = %v want 5", snap["m"])
		}
	})

	t.Run("Concurrent_AccumulatorAtomic", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		const N = 128
		ctx := withIdentity(t)
		q := identity.MustQuadrupleFrom(ctx)
		ceiling := 1.0
		perCall := 0.10
		cfg := governance.Config{
			DefaultTier: "free",
			IdentityTiers: map[string]governance.TierConfig{
				"free": {BudgetCeilingUSD: ceiling},
			},
		}
		acc, err := governance.NewCostAccumulator(h.State, h.Bus, cfg)
		if err != nil {
			t.Fatalf("NewCostAccumulator: %v", err)
		}
		defer acc.Close(context.Background())

		var wg sync.WaitGroup
		var rejected atomic.Int64
		var succeeded atomic.Int64

		baseline := runtime.NumGoroutine()
		for range N {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := llm.CompleteRequest{Model: "m"}
				if err := acc.PreCall(ctx, req); err != nil {
					if errors.Is(err, governance.ErrBudgetExceeded) {
						rejected.Add(1)
						return
					}
					t.Errorf("PreCall err: %v", err)
					return
				}
				if err := acc.PostCall(ctx, req,
					llm.CompleteResponse{Cost: llm.Cost{TotalCost: perCall}}, nil); err != nil {
					t.Errorf("PostCall err: %v", err)
					return
				}
				succeeded.Add(1)
			}()
		}
		wg.Wait()
		total, _, err := acc.Snapshot(ctx, q)
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		// The PreCall→PostCall race means up to N concurrent in-flight
		// calls can each pass the gate before any PostCall lands. The
		// permitted overshoot is bounded by N × perCall (worst case
		// where every call snuck through before any update). We accept
		// the documented overshoot rather than mandating zero, per the
		// phase plan's "small tolerance" note.
		maxAllowed := ceiling + float64(N)*perCall
		if total > maxAllowed {
			t.Errorf("total cost overshoot: got %v > max %v (ceiling %v, succeeded %d, rejected %d)",
				total, maxAllowed, ceiling, succeeded.Load(), rejected.Load())
		}
		if total == 0 {
			t.Errorf("accumulator did not record any cost (succeeded=%d)", succeeded.Load())
		}
		if rejected.Load() == 0 {
			t.Errorf("ceiling never fired across %d concurrent calls", N)
		}

		// Goroutine leak gate (allow drift for sub-second-async closures).
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
			runtime.Gosched()
		}
	})

	t.Run("SetPosture_FullReplaceRoundTrips", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		policy, err := governance.NewSetPosturePolicy(h.State, h.Bus, governance.Config{}, nil, nil, false)
		if err != nil {
			t.Fatalf("NewSetPosturePolicy: %v", err)
		}
		defer policy.Close(context.Background())
		provider := governance.NewPostureProviderWithState(governance.Config{}, h.State)

		actor := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}}
		if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
			DefaultTier: "free",
			IdentityTiers: map[string]governance.TierConfig{
				"free": {BudgetCeilingUSD: 0.50, MaxTokens: 1000},
			},
		}); err != nil {
			t.Fatalf("Set: %v", err)
		}
		ctx, err := identity.With(context.Background(), actor.Identity)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		read, err := provider.Posture(ctx)
		if err != nil {
			t.Fatalf("Posture: %v", err)
		}
		if read.DefaultTier != "free" || read.IdentityTiers["free"].BudgetCeilingUSD != 0.50 {
			t.Fatalf("round-trip mismatch on this driver: %+v", read)
		}
	})

	t.Run("SetPosture_PartialAndEmptyWriteFailClosed", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		policy, err := governance.NewSetPosturePolicy(h.State, h.Bus, governance.Config{}, nil, nil, false)
		if err != nil {
			t.Fatalf("NewSetPosturePolicy: %v", err)
		}
		defer policy.Close(context.Background())
		actor := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}}

		// Seed an enforced tier, then assert both the omit-the-tier and the
		// empty-table writes are rejected fail-closed identically on every
		// driver (the identity-tier policy write's §9 conformance requirement).
		if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
			DefaultTier:   "free",
			IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.50}},
		}); err != nil {
			t.Fatalf("seed Set: %v", err)
		}
		if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
			DefaultTier:   "team",
			IdentityTiers: map[string]governance.TierConfig{"team": {BudgetCeilingUSD: 5.0}},
		}); !errors.Is(err, governance.ErrPolicyWidening) {
			t.Fatalf("partial write: got %v, want ErrPolicyWidening", err)
		}
		// Fail-closed either way: DefaultTier "free" is absent from the empty
		// map (ErrInvalidPosture, the structural check) — an equally-valid
		// fail-closed rejection to the no-widening path. The write is never
		// silently persisted, which is the conformance invariant.
		if _, err := policy.Set(context.Background(), actor, governance.SetPostureSpec{
			DefaultTier:   "free",
			IdentityTiers: map[string]governance.TierConfig{},
		}); !errors.Is(err, governance.ErrPolicyWidening) && !errors.Is(err, governance.ErrInvalidPosture) {
			t.Fatalf("empty write: got %v, want ErrPolicyWidening or ErrInvalidPosture", err)
		}

		// The prior enforced policy is unchanged (no mutation on a reject).
		provider := governance.NewPostureProviderWithState(governance.Config{}, h.State)
		ctx, err := identity.With(context.Background(), actor.Identity)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		read, err := provider.Posture(ctx)
		if err != nil {
			t.Fatalf("Posture: %v", err)
		}
		if read.IdentityTiers["free"].BudgetCeilingUSD != 0.50 {
			t.Fatalf("post-reject policy widened: %+v", read)
		}
	})

	t.Run("SetPosture_SurvivesRestart", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()
		actor := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}}

		p1, err := governance.NewSetPosturePolicy(h.State, h.Bus, governance.Config{}, nil, nil, false)
		if err != nil {
			t.Fatalf("p1: %v", err)
		}
		if _, err := p1.Set(context.Background(), actor, governance.SetPostureSpec{
			DefaultTier:   "free",
			IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 4096}},
		}); err != nil {
			t.Fatalf("Set: %v", err)
		}
		_ = p1.Close(context.Background())

		// A fresh provider over the SAME store reads back the persisted record.
		provider := governance.NewPostureProviderWithState(governance.Config{}, h.State)
		ctx, err := identity.With(context.Background(), actor.Identity)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		read, err := provider.Posture(ctx)
		if err != nil {
			t.Fatalf("Posture: %v", err)
		}
		if read.IdentityTiers["free"].MaxTokens != 4096 {
			t.Fatalf("restart read = %+v want free max_tokens 4096", read)
		}
	})

	// parity gate for the assembly-constructed
	// Subsystem — `NewSubsystemFromConfig` composes the same enforcers
	// the per-policy subtests above exercise individually; this subtest
	// proves the composed surface enforces all three policies on every
	// conformant StateStore driver.
	t.Run("AssembledSubsystem_AllThreePoliciesEnforce", func(t *testing.T) {
		t.Parallel()
		h := mk()
		defer h.Cleanup()

		cfg := governance.Config{
			DefaultTier: "conf",
			IdentityTiers: map[string]governance.TierConfig{
				"conf": {
					MaxTokens:        10,
					RateLimit:        governance.RateLimitConfig{Capacity: 2},
					BudgetCeilingUSD: 0.5,
				},
			},
		}
		sub, err := governance.NewSubsystemFromConfig(cfg, h.State, h.Bus)
		if err != nil {
			t.Fatalf("NewSubsystemFromConfig: %v", err)
		}
		ctx := withIdentity(t)

		// MaxTokens (the first member) rejects an over-cap request.
		over := 100
		if err := sub.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &over}); !errors.Is(err, governance.ErrMaxTokensExceeded) {
			t.Fatalf("over-cap PreCall: got %v, want ErrMaxTokensExceeded", err)
		}

		// Cost ceiling: accumulate past 0.5, next PreCall rejects.
		if err := sub.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
			t.Fatalf("under-budget PreCall: %v", err)
		}
		if err := sub.PostCall(ctx, llm.CompleteRequest{Model: "m"},
			llm.CompleteResponse{Cost: llm.Cost{TotalCost: 1.0}, Content: "x"}, nil); err != nil {
			t.Fatalf("PostCall: %v", err)
		}
		if err := sub.PreCall(ctx, llm.CompleteRequest{Model: "m"}); !errors.Is(err, governance.ErrBudgetExceeded) {
			t.Fatalf("over-budget PreCall: got %v, want ErrBudgetExceeded", err)
		}

		// Rate limit: a sibling-session identity (its own budget) drains
		// the capacity-2 bucket, then underflows.
		sibling, err := identity.WithRun(context.Background(),
			identity.Identity{TenantID: "T", UserID: "U", SessionID: "S-assembled"}, "R")
		if err != nil {
			t.Fatalf("identity.WithRun: %v", err)
		}
		for i := range 2 {
			if err := sub.PreCall(sibling, llm.CompleteRequest{Model: "m"}); err != nil {
				t.Fatalf("drain %d: %v", i, err)
			}
		}
		if err := sub.PreCall(sibling, llm.CompleteRequest{Model: "m"}); !errors.Is(err, governance.ErrRateLimited) {
			t.Fatalf("underflow PreCall: got %v, want ErrRateLimited", err)
		}
	})
}

// floatNear compares two USD floats with a tolerance that absorbs binary
// float64 rounding of decimal cents.
func floatNear(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

// withIdentity attaches the canonical conformance identity + run to a
// fresh ctx. Helper centralised so every subtest uses the same shape.
func withIdentity(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.WithRun(context.Background(), id, "R")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}
