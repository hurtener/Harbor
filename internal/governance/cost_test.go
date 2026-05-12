package governance_test

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
)

func TestCostAccumulator_PostCallAccumulates(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "T", "U", "S", "R")
	q := identity.MustQuadrupleFrom(ctx)
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	for i, cost := range []float64{0.10, 0.25, 0.05} {
		if err := acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
			llm.CompleteResponse{Cost: llm.Cost{TotalCost: cost}}, nil); err != nil {
			t.Fatalf("PostCall[%d]: %v", i, err)
		}
	}
	total, byModel, err := acc.Snapshot(ctx, q)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	want := 0.10 + 0.25 + 0.05
	if !floatNear(total, want, 1e-9) {
		t.Errorf("total = %v want %v", total, want)
	}
	if !floatNear(byModel["m"], want, 1e-9) {
		t.Errorf("byModel[m] = %v want %v", byModel["m"], want)
	}
}

func TestCostAccumulator_CrossIdentityIsolation(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 1.0}},
	})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	// Tenant A: spends 0.90 (under 1.0 ceiling).
	ctxA := ctxWith(t, "A", "uA", "sA", "rA")
	if err := acc.PostCall(ctxA, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.90}}, nil); err != nil {
		t.Fatalf("PostCall A: %v", err)
	}
	// Tenant B: spends 0.10 — must NOT be affected by A's 0.90.
	ctxB := ctxWith(t, "B", "uB", "sB", "rB")
	if err := acc.PreCall(ctxB, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Fatalf("Tenant B PreCall blocked by A's spending: %v", err)
	}
	if err := acc.PostCall(ctxB, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.10}}, nil); err != nil {
		t.Fatalf("PostCall B: %v", err)
	}
	qA := identity.MustQuadrupleFrom(ctxA)
	qB := identity.MustQuadrupleFrom(ctxB)
	tA, _, _ := acc.Snapshot(ctxA, qA)
	tB, _, _ := acc.Snapshot(ctxB, qB)
	if !floatNear(tA, 0.90, 1e-9) || !floatNear(tB, 0.10, 1e-9) {
		t.Errorf("cross-identity bleed: A=%v B=%v", tA, tB)
	}
}

func TestCostAccumulator_LatentZeroCost_NoOp(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "T", "U", "S", "R")
	q := identity.MustQuadrupleFrom(ctx)
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())
	// A failed call with zero cost + zero usage + zero content — the
	// accumulator should skip the persist path entirely.
	if err := acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{}, errors.New("upstream failed")); err != nil {
		t.Errorf("PostCall zero-cost zero-usage: %v", err)
	}
	total, _, err := acc.Snapshot(ctx, q)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %v want 0", total)
	}
}

func TestCostAccumulator_BudgetEventCarriesIdentityAndCeiling(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	ctx := ctxWith(t, "T", "U", "S", "R")
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 0.25}},
	}
	acc, err := governance.NewCostAccumulator(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "T", User: "U", Session: "S",
		Types: []events.EventType{governance.EventTypeBudgetExceeded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	if err := acc.PostCall(ctx, llm.CompleteRequest{Model: "m"},
		llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.30}}, nil); err != nil {
		t.Fatalf("PostCall: %v", err)
	}
	err = acc.PreCall(ctx, llm.CompleteRequest{Model: "m"})
	if !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("PreCall: want ErrBudgetExceeded, got %v", err)
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(governance.BudgetExceededPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		if p.Tier != "free" {
			t.Errorf("tier = %q", p.Tier)
		}
		if p.Identity.TenantID != "T" {
			t.Errorf("identity tenant = %q", p.Identity.TenantID)
		}
		if p.Ceiling != 0.25 {
			t.Errorf("ceiling = %v", p.Ceiling)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not observe BudgetExceeded event within 2s")
	}
}

func TestCostAccumulator_ConcurrentReuse_D025(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	const N = 128
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 1000.0}},
	}
	acc, err := governance.NewCostAccumulator(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var errs atomic.Int64
	// Each goroutine works under its own identity → per-identity
	// accumulator state. Asserts D-025 across keys.
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := "T"
			if i%2 == 0 {
				tenant = "U"
			}
			session := "s"
			user := "u"
			run := "r"
			id := identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
			ctx, err := identity.WithRun(context.Background(), id, run)
			if err != nil {
				errs.Add(1)
				return
			}
			req := llm.CompleteRequest{Model: "m"}
			if err := acc.PreCall(ctx, req); err != nil {
				errs.Add(1)
				return
			}
			if err := acc.PostCall(ctx, req,
				llm.CompleteResponse{Cost: llm.Cost{TotalCost: 0.001}}, nil); err != nil {
				errs.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := errs.Load(); got != 0 {
		t.Errorf("D-025 stress: %d errors across %d calls", got, N)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
}

func TestCostAccumulator_ClosedSubsystem(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	_ = acc.Close(context.Background())
	if err := acc.PreCall(ctxWith(t, "T", "U", "S", "R"), llm.CompleteRequest{}); !errors.Is(err, governance.ErrClosed) {
		t.Errorf("PreCall after Close: want ErrClosed, got %v", err)
	}
	if err := acc.PostCall(ctxWith(t, "T", "U", "S", "R"), llm.CompleteRequest{}, llm.CompleteResponse{}, nil); !errors.Is(err, governance.ErrClosed) {
		t.Errorf("PostCall after Close: want ErrClosed, got %v", err)
	}
}

func TestNewCostAccumulator_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	if _, err := governance.NewCostAccumulator(nil, nil, governance.Config{}); err == nil {
		t.Errorf("nil state.StateStore: want error, got nil")
	}
	_, st, cleanup := busAndState(t)
	defer cleanup()
	if _, err := governance.NewCostAccumulator(st, nil, governance.Config{}); err == nil {
		t.Errorf("nil events.EventBus: want error, got nil")
	}
}

func floatNear(a, b, epsilon float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= epsilon
}
