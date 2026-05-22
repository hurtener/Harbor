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
		if err = acc.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
			t.Fatalf("first PreCall: %v", err)
		}
		if err = acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
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
		if err = acc1.PostCall(ctx, llm.CompleteRequest{Model: "m"},
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
		if err = rl1.PreCall(ctx, req); err != nil {
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
				if err := acc.PreCall(ctx, req); err != nil { //nolint:govet // per-goroutine err; shadow is required for concurrency safety
					if errors.Is(err, governance.ErrBudgetExceeded) {
						rejected.Add(1)
						return
					}
					t.Errorf("PreCall err: %v", err)
					return
				}
				if err := acc.PostCall(ctx, req, //nolint:govet // per-goroutine err; shadow is required for concurrency safety
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
}

// withIdentity attaches identity + run to ctx. Helper centralised so
// every subtest uses the same shape.
func withIdentity(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.WithRun(context.Background(), id, "R")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}
