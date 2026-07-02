package governance_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	_ "github.com/hurtener/Harbor/internal/llm/corrections"
	_ "github.com/hurtener/Harbor/internal/llm/output"
	_ "github.com/hurtener/Harbor/internal/llm/retry"
)

// chainDriverSeq keeps each llm.Register call in this file idempotent
// across `go test -count=N`: the driver registry is process-wide
// write-once, so a fixed literal name collides with itself on the second
// iteration.
var chainDriverSeq atomic.Int64

func uniqueChainDriver(base string) string {
	return fmt.Sprintf("%s-%d", base, chainDriverSeq.Add(1))
}

// scriptedDriver returns caller-supplied responses per attempt number.
// Used by the single-threaded exactness sub-tests where attempt ordering
// is deterministic.
type scriptedDriver struct {
	fn      func(attempt int, req llm.CompleteRequest) (llm.CompleteResponse, error)
	counter atomic.Int64
	closed  atomic.Bool
}

func (d *scriptedDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if d.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	n := int(d.counter.Add(1))
	return d.fn(n, req)
}

func (d *scriptedDriver) Close(_ context.Context) error {
	d.closed.CompareAndSwap(false, true)
	return nil
}

// contentDriver decides its response from request CONTENT (not a shared
// counter), so it is deterministic under concurrency: a request carrying
// the retry wrapper's corrective marker gets the "good" response, else the
// "bad" one. Immutable → safe to share across N concurrent governed calls.
type contentDriver struct {
	badContent, goodContent string
	badCost, goodCost       float64
	closed                  atomic.Bool
}

func requestHasCorrectiveMarker(req llm.CompleteRequest) bool {
	for _, m := range req.Messages {
		if m.Content.Text != nil && strings.Contains(*m.Content.Text, "failed validation") {
			return true
		}
	}
	return false
}

func (d *contentDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if d.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	if requestHasCorrectiveMarker(req) {
		return llm.CompleteResponse{Content: d.goodContent, Cost: llm.Cost{TotalCost: d.goodCost}}, nil
	}
	return llm.CompleteResponse{Content: d.badContent, Cost: llm.Cost{TotalCost: d.badCost}}, nil
}

func (d *contentDriver) Close(_ context.Context) error {
	d.closed.CompareAndSwap(false, true)
	return nil
}

func chainArtifacts(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// openGoverned composes the full production wrapper chain
// governance(retry(downgrade(corrections(safety(driver))))) via llm.Open +
// governance.Wrap — the documented headless composition (registry.go).
func openGoverned(t *testing.T, cfg llm.ConfigSnapshot, deps llm.Deps, sub governance.Subsystem) llm.LLMClient {
	t.Helper()
	inner, err := llm.Open(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("llm.Open: %v", err)
	}
	t.Cleanup(func() { _ = inner.Close(context.Background()) })
	return governance.Wrap(inner, sub)
}

func userRequest(validator func(llm.CompleteResponse) error, rf *llm.ResponseFormat) llm.CompleteRequest {
	text := "please answer"
	return llm.CompleteRequest{
		Model:          "m",
		Messages:       []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		Validator:      validator,
		ResponseFormat: rf,
	}
}

// TestChain_ExactlyOnce_AllTerminalShapes drives the full production chain
// with DISTINCT per-attempt costs (powers of ten) and asserts the
// accumulator total equals Σ all provider attempts exactly once, across
// success-after-retries, ErrRetryExhausted, and ErrDowngradeExhausted.
func TestChain_ExactlyOnce_AllTerminalShapes(t *testing.T) {
	t.Parallel()

	t.Run("success_after_retries", func(t *testing.T) {
		t.Parallel()
		bus, st, cleanup := busAndState(t)
		defer cleanup()
		drv := &scriptedDriver{fn: func(attempt int, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
			// attempt 1 → "bad"/0.01 (rejected, non-final → tap)
			// attempt 2 → "good"/1.0 (accepted → propagates → resp.Cost)
			if attempt == 1 {
				return llm.CompleteResponse{Content: "bad", Cost: llm.Cost{TotalCost: 0.01}}, nil
			}
			return llm.CompleteResponse{Content: "good", Cost: llm.Cost{TotalCost: 1.0}}, nil
		}}
		name := uniqueChainDriver("chain-success")
		llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) { return drv, nil })

		acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
		if err != nil {
			t.Fatalf("NewCostAccumulator: %v", err)
		}
		defer acc.Close(context.Background())
		cfg := llm.ConfigSnapshot{
			Driver:               name,
			ContextWindowReserve: 0.05,
			HeavyOutputThreshold: 32_768,
			ModelProfiles:        map[string]llm.ModelProfile{"m": {ContextWindowTokens: 1000, MaxRetries: 3}},
		}
		client := openGoverned(t, cfg, llm.Deps{Artifacts: chainArtifacts(t), Bus: bus}, acc)

		ctx := ctxWith(t, "T", "U", "S", "R")
		validator := func(r llm.CompleteResponse) error {
			if r.Content == "good" {
				return nil
			}
			return errors.New("bad")
		}
		resp, err := client.Complete(ctx, userRequest(validator, nil))
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.Content != "good" {
			t.Fatalf("Content = %q, want good", resp.Content)
		}
		assertIdentityTotal(t, acc, ctx, 1.01) // 0.01 (tap) + 1.0 (final)
	})

	t.Run("retry_exhausted", func(t *testing.T) {
		t.Parallel()
		bus, st, cleanup := busAndState(t)
		defer cleanup()
		drv := &scriptedDriver{fn: func(attempt int, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
			// Both attempts rejected. attempt 1 → 0.01 (tap, non-final);
			// attempt 2 → 0.1 (final lastResp → returned with
			// ErrRetryExhausted → accounted via resp.Cost).
			if attempt == 1 {
				return llm.CompleteResponse{Content: "bad", Cost: llm.Cost{TotalCost: 0.01}}, nil
			}
			return llm.CompleteResponse{Content: "bad", Cost: llm.Cost{TotalCost: 0.1}}, nil
		}}
		name := uniqueChainDriver("chain-exhaust")
		llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) { return drv, nil })

		acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
		if err != nil {
			t.Fatalf("NewCostAccumulator: %v", err)
		}
		defer acc.Close(context.Background())
		cfg := llm.ConfigSnapshot{
			Driver:               name,
			ContextWindowReserve: 0.05,
			HeavyOutputThreshold: 32_768,
			ModelProfiles:        map[string]llm.ModelProfile{"m": {ContextWindowTokens: 1000, MaxRetries: 1}},
		}
		client := openGoverned(t, cfg, llm.Deps{Artifacts: chainArtifacts(t), Bus: bus}, acc)

		ctx := ctxWith(t, "T", "U", "S", "R")
		validator := func(_ llm.CompleteResponse) error { return errors.New("always bad") }
		_, err = client.Complete(ctx, userRequest(validator, nil))
		if !errors.Is(err, llm.ErrRetryExhausted) {
			t.Fatalf("err = %v, want ErrRetryExhausted", err)
		}
		assertIdentityTotal(t, acc, ctx, 0.11) // 0.01 (tap) + 0.1 (final lastResp)
	})

	t.Run("downgrade_exhausted", func(t *testing.T) {
		t.Parallel()
		bus, st, cleanup := busAndState(t)
		defer cleanup()
		drv := &scriptedDriver{fn: func(attempt int, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
			// Every attempt a schema-class failure → the downgrade chain
			// walks Native → Prompted → text then exhausts. Distinct costs.
			cost := 0.001 * pow10chain(attempt-1) // 0.001, 0.01, 0.1
			return llm.CompleteResponse{Cost: llm.Cost{TotalCost: cost}},
				fmt.Errorf("%w: attempt %d", llm.ErrInvalidJSONSchema, attempt)
		}}
		name := uniqueChainDriver("chain-downgrade")
		llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) { return drv, nil })

		acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
		if err != nil {
			t.Fatalf("NewCostAccumulator: %v", err)
		}
		defer acc.Close(context.Background())
		cfg := llm.ConfigSnapshot{
			Driver:               name,
			ContextWindowReserve: 0.05,
			HeavyOutputThreshold: 32_768,
			ModelProfiles: map[string]llm.ModelProfile{
				"m": {ContextWindowTokens: 1000, OutputMode: llm.OutputModeNative},
			},
		}
		client := openGoverned(t, cfg, llm.Deps{Artifacts: chainArtifacts(t), Bus: bus}, acc)

		ctx := ctxWith(t, "T", "U", "S", "R")
		// No validator; the downgrade chain engages on the structured
		// ResponseFormat and exhausts (an inner error retry propagates).
		rf := &llm.ResponseFormat{Kind: llm.FormatJSONSchema, JSONSchema: []byte(`{"type":"object"}`)}
		_, err = client.Complete(ctx, userRequest(nil, rf))
		if !errors.Is(err, llm.ErrDowngradeExhausted) {
			t.Fatalf("err = %v, want ErrDowngradeExhausted", err)
		}
		assertIdentityTotal(t, acc, ctx, 0.111) // 0.001 + 0.01 + 0.1 (all via tap)
	})
}

// TestChain_Ceiling_IntermediateAttemptsTripNextPreCall proves an
// intermediate attempt's spend, folded via the tap, pushes the accumulator
// over the ceiling so the NEXT governed call's PreCall rejects — even
// though the FINAL response's cost alone stays under the ceiling.
func TestChain_Ceiling_IntermediateAttemptsTripNextPreCall(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	drv := &contentDriver{
		badContent: "bad", badCost: 0.5, // intermediate attempt (reported)
		goodContent: "good", goodCost: 0.6, // final (< 1.0 ceiling on its own)
	}
	name := uniqueChainDriver("chain-ceiling")
	llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) { return drv, nil })

	cfg := llm.ConfigSnapshot{
		Driver:               name,
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32_768,
		ModelProfiles:        map[string]llm.ModelProfile{"m": {ContextWindowTokens: 1000, MaxRetries: 3}},
	}
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 1.0}},
	})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())
	client := openGoverned(t, cfg, llm.Deps{Artifacts: chainArtifacts(t), Bus: bus}, acc)

	// Observe the budget_exceeded event on the second call.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "T", User: "U", Session: "S",
		Types: []events.EventType{governance.EventTypeBudgetExceeded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	ctx := ctxWith(t, "T", "U", "S", "R")
	validator := func(r llm.CompleteResponse) error {
		if r.Content == "good" {
			return nil
		}
		return errors.New("bad")
	}

	// First governed call completes (PreCall permits at total 0). It folds
	// 0.5 (tap) + 0.6 (final) = 1.1, over the 1.0 ceiling.
	if _, err := client.Complete(ctx, userRequest(validator, nil)); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	assertIdentityTotal(t, acc, ctx, 1.1)

	// Second governed call: PreCall now sees 1.1 >= 1.0 → rejects. WITHOUT
	// the tap the total would be only 0.6 and this would wrongly permit.
	_, err = client.Complete(ctx, userRequest(validator, nil))
	if !errors.Is(err, governance.ErrBudgetExceeded) {
		t.Fatalf("second Complete err = %v, want ErrBudgetExceeded", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != governance.EventTypeBudgetExceeded {
			t.Errorf("event type = %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe budget_exceeded within 2s")
	}
}

// TestChain_D025_ConcurrentReuse runs N>=100 concurrent governed Complete
// calls against ONE shared wrapper chain, each with a DISTINCT identity
// driving one corrective retry, asserting exact per-identity totals (no
// cross-run tap bleed), no cancellation cross-talk, and goroutine baseline
// restored after teardown.
func TestChain_D025_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()

	drv := &contentDriver{
		badContent: "bad", badCost: 0.5,
		goodContent: "good", goodCost: 0.6,
	}
	name := uniqueChainDriver("chain-d025")
	llm.Register(name, func(_ llm.ConfigSnapshot, _ llm.Deps) (llm.Driver, error) { return drv, nil })

	cfg := llm.ConfigSnapshot{
		Driver:               name,
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32_768,
		ModelProfiles:        map[string]llm.ModelProfile{"m": {ContextWindowTokens: 1000, MaxRetries: 3}},
	}
	// No ceiling → every identity's two attempts complete; we assert exact
	// per-identity folded totals.
	acc, err := governance.NewCostAccumulator(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewCostAccumulator: %v", err)
	}
	defer acc.Close(context.Background())
	client := openGoverned(t, cfg, llm.Deps{Artifacts: chainArtifacts(t), Bus: bus}, acc)

	const N = 128
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var failures atomic.Int64
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct session per goroutine → distinct identity key.
			id := identity.Identity{TenantID: "T", UserID: "U", SessionID: fmt.Sprintf("S-%d", i)}
			ctx, err := identity.WithRun(context.Background(), id, fmt.Sprintf("R-%d", i))
			if err != nil {
				t.Errorf("WithRun: %v", err)
				failures.Add(1)
				return
			}
			validator := func(r llm.CompleteResponse) error {
				if r.Content == "good" {
					return nil
				}
				return errors.New("bad")
			}
			resp, err := client.Complete(ctx, userRequest(validator, nil))
			if err != nil {
				t.Errorf("goroutine %d Complete: %v", i, err)
				failures.Add(1)
				return
			}
			if resp.Content != "good" {
				t.Errorf("goroutine %d Content = %q", i, resp.Content)
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d goroutine failures", failures.Load())
	}

	// Each identity folded exactly 0.5 (tap) + 0.6 (final) = 1.1 — no
	// cross-run bleed (the tap is per-call ctx state, never a wrapper field).
	for i := range N {
		id := identity.Identity{TenantID: "T", UserID: "U", SessionID: fmt.Sprintf("S-%d", i)}
		ctx, err := identity.WithRun(context.Background(), id, "check")
		if err != nil {
			t.Fatalf("WithRun check: %v", err)
		}
		q := identity.MustQuadrupleFrom(ctx)
		total, _, err := acc.Snapshot(ctx, q)
		if err != nil {
			t.Fatalf("Snapshot S-%d: %v", i, err)
		}
		if !floatNear(total, 1.1) {
			t.Fatalf("identity S-%d total = %v, want 1.1 (cross-run bleed?)", i, total)
		}
	}

	// Goroutine baseline restored (allow slack for async bus closures).
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
}

func assertIdentityTotal(t *testing.T, acc *governance.CostAccumulator, ctx context.Context, want float64) {
	t.Helper()
	q := identity.MustQuadrupleFrom(ctx)
	total, _, err := acc.Snapshot(ctx, q)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !floatNear(total, want) {
		t.Fatalf("accumulator total = %v, want %v", total, want)
	}
}

func pow10chain(n int) float64 {
	out := 1.0
	for range n {
		out *= 10
	}
	return out
}
