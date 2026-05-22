package strategy_test

// Additional executor tests aimed at coverage for paths the higher-
// level conformance suite + integration tests don't reach directly:
//
//   - The `none` executor in isolation (the conformance suite
//     exercises it transitively via the inmem driver, but coverage
//     is attributed to the inmem package not this one).
//   - Truncation Health / EstimateTokens / etc.
//   - Rolling-summary Flush / Restore / recover paths.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/strategy"
)

// TestNone_FullSurface exercises every noneExec method against a
// fresh state. The conformance suite covers semantics; this test
// re-exercises them inside the strategy package so per-package
// coverage credits the lines correctly.
func TestNone_FullSurface(t *testing.T) {
	deps := buildDeps(t, nil)
	exec, err := strategy.New(memory.StrategyNone, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())
	ctx := context.Background()
	id := tripleA()

	if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "x"}); err != nil {
		t.Errorf("AddTurn: %v", err)
	}
	patch, err := exec.GetLLMContext(ctx, id)
	if err != nil {
		t.Errorf("GetLLMContext: %v", err)
	}
	if patch.Strategy != memory.StrategyNone {
		t.Errorf("patch.Strategy=%q, want %q", patch.Strategy, memory.StrategyNone)
	}
	got, err := exec.EstimateTokens(ctx, id)
	if err != nil {
		t.Errorf("EstimateTokens: %v", err)
	}
	if got != 0 {
		t.Errorf("EstimateTokens=%d, want 0", got)
	}
	h, err := exec.Health(ctx, id)
	if err != nil {
		t.Errorf("Health: %v", err)
	}
	if h != memory.HealthHealthy {
		t.Errorf("Health=%q, want %q", h, memory.HealthHealthy)
	}
	if err := exec.Flush(ctx, id); err != nil {
		t.Errorf("Flush: %v", err)
	}
	snap, err := exec.Snapshot(ctx, id)
	if err != nil {
		t.Errorf("Snapshot: %v", err)
	}
	if err := exec.Restore(ctx, id, snap); err != nil {
		t.Errorf("Restore (returned snap): %v", err)
	}
	// Empty snapshot round-trip.
	if err := exec.Restore(ctx, id, memory.Snapshot{}); err != nil {
		t.Errorf("Restore empty: %v", err)
	}
}

// TestNone_RejectsInvalidSnapshot covers the strategy-mismatch +
// turns-on-none-record paths.
func TestNone_RejectsInvalidSnapshot(t *testing.T) {
	deps := buildDeps(t, nil)
	exec, err := strategy.New(memory.StrategyNone, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())
	ctx := context.Background()
	id := tripleA()

	// Strategy mismatch.
	err = exec.Restore(ctx, id, memory.Snapshot{
		Strategy: memory.StrategyTruncation,
		Bytes:    []byte(`{"strategy":"truncation"}`),
	})
	if err == nil {
		t.Error("Restore mismatch: err=nil, want non-nil")
	}

	// Turns on a strategy=none record.
	err = exec.Restore(ctx, id, memory.Snapshot{
		Strategy: memory.StrategyNone,
		Bytes:    []byte(`{"strategy":"none","turns":[{"user_message":"x"}]}`),
	})
	if err == nil {
		t.Error("Restore non-empty: err=nil, want non-nil")
	}

	// Malformed bytes.
	err = exec.Restore(ctx, id, memory.Snapshot{
		Strategy: memory.StrategyNone,
		Bytes:    []byte("not-json"),
	})
	if err == nil {
		t.Error("Restore malformed: err=nil, want non-nil")
	}
}

// TestTruncation_HealthAndEstimateTokens covers the auxiliary
// methods on the truncation executor in isolation.
func TestTruncation_HealthAndEstimateTokens(t *testing.T) {
	deps := buildDeps(t, nil)
	deps.BudgetTokens = 64
	exec, err := strategy.New(memory.StrategyTruncation, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())
	ctx := context.Background()
	id := tripleA()

	h, err := exec.Health(ctx, id)
	if err != nil {
		t.Errorf("Health: %v", err)
	}
	if h != memory.HealthHealthy {
		t.Errorf("Health=%q, want %q", h, memory.HealthHealthy)
	}

	if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "hello", AssistantResponse: "world"}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	tokens, err := exec.EstimateTokens(ctx, id)
	if err != nil {
		t.Errorf("EstimateTokens: %v", err)
	}
	if tokens == 0 {
		t.Error("EstimateTokens=0 after AddTurn, want non-zero")
	}
}

// TestRollingSummary_Flush wipes the executor's per-key state.
func TestRollingSummary_Flush(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())
	ctx := context.Background()
	id := tripleA()

	for i := range 6 {
		if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"}); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	if err := exec.Flush(ctx, id); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// After Flush: no recent turns, no summary, healthy.
	patch, err := exec.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if len(patch.RecentTurns) != 0 {
		t.Errorf("RecentTurns after Flush=%d, want 0", len(patch.RecentTurns))
	}
	if patch.Summary != "" {
		t.Errorf("Summary after Flush=%q, want empty", patch.Summary)
	}
}

// TestRollingSummary_Restore exercises the rolling-summary
// Restore path: a Snapshot returned by the executor must Restore
// cleanly, including the rolling summary state.
func TestRollingSummary_Restore(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())
	ctx := context.Background()
	id := tripleA()

	for i := range 6 {
		if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"}); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	snap, err := exec.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Round-trip the same snapshot.
	if err := exec.Restore(ctx, id, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	patch, err := exec.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Summary == "" {
		t.Error("rolling_summary restore lost the summary")
	}

	// Empty snapshot round-trip.
	if err := exec.Restore(ctx, id, memory.Snapshot{}); err != nil {
		t.Errorf("Restore empty: %v", err)
	}
}

// TestRollingSummary_RejectsInvalidSnapshot covers the
// strategy-mismatch + malformed-bytes paths on the rolling-summary
// executor.
func TestRollingSummary_RejectsInvalidSnapshot(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())
	ctx := context.Background()
	id := tripleA()

	// Strategy mismatch.
	err = exec.Restore(ctx, id, memory.Snapshot{
		Strategy: memory.StrategyTruncation,
		Bytes:    []byte(`{"strategy":"truncation"}`),
	})
	if err == nil {
		t.Error("Restore mismatch: err=nil, want non-nil")
	}

	// Malformed bytes.
	err = exec.Restore(ctx, id, memory.Snapshot{
		Strategy: memory.StrategyRollingSummary,
		Bytes:    []byte("not-json"),
	})
	if err == nil {
		t.Error("Restore malformed: err=nil, want non-nil")
	}
}

// recoveringSummarizer fails for the first `failuresUntilRecover`
// calls then succeeds, simulating a transient outage. Used to
// drive the rolling-summary FSM through `healthy → retry →
// degraded → recovering → healthy` end-to-end.
type recoveringSummarizer struct {
	calls                int
	failuresUntilRecover int
}

func (s *recoveringSummarizer) Summarize(_ context.Context, _ identity.Quadruple, req memory.SummarizeRequest) (memory.SummarizeResponse, error) {
	s.calls++
	if s.calls <= s.failuresUntilRecover {
		return memory.SummarizeResponse{}, errSummarizerOutage
	}
	return memory.SummarizeResponse{Summary: req.PreviousSummary + "|recovered"}, nil
}

var errSummarizerOutage = errSummarizerOutageType{}

type errSummarizerOutageType struct{}

func (errSummarizerOutageType) Error() string { return "test: summarizer outage" }

// TestRollingSummary_HealthChangedEvents pins the sequence of
// transitions observable on the bus during a forced failure run.
// Used here primarily to cover `transitionHealth` and the
// `EmitHealthChanged` helper.
func TestRollingSummary_HealthChangedEvents(t *testing.T) {
	deps := buildDeps(t, &recoveringSummarizer{failuresUntilRecover: 100})
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())

	sub, err := deps.Bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{memory.EventTypeMemoryHealthChanged},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	ctx := context.Background()
	id := tripleA()
	for range 12 {
		_ = exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"})
	}
	// Drain a few events; assert at least one healthy → retry and
	// one retry → degraded.
	gotRetry := false
	gotDegraded := false
	deadline := time.After(2 * time.Second)
loop:
	for !gotRetry || !gotDegraded {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				break loop
			}
			payload, ok := ev.Payload.(memory.HealthChangedPayload)
			if !ok {
				continue
			}
			if payload.NewHealth == memory.HealthRetry {
				gotRetry = true
			}
			if payload.NewHealth == memory.HealthDegraded {
				gotDegraded = true
			}
		case <-deadline:
			break loop
		}
	}
	if !gotRetry {
		t.Error("did not observe healthy → retry transition")
	}
	if !gotDegraded {
		t.Error("did not observe retry → degraded transition")
	}
}

// TestEmitHealthChanged_RejectsBadTransition pins the helper's
// fail-loud contract: an invalid transition returns wrapped
// ErrInvalidHealthTransition and does NOT publish.
func TestEmitHealthChanged_RejectsBadTransition(t *testing.T) {
	deps := buildDeps(t, nil)
	id := tripleA()
	// healthy → recovering is NOT in the FSM table.
	err := memory.EmitHealthChanged(context.Background(), deps.Bus, id,
		memory.HealthHealthy, memory.HealthRecovering, "bogus")
	if err == nil {
		t.Fatal("err=nil, want non-nil for invalid transition")
	}
}

// TestEmitHealthChanged_RejectsMissingIdentity covers the helper's
// identity-required path.
func TestEmitHealthChanged_RejectsMissingIdentity(t *testing.T) {
	deps := buildDeps(t, nil)
	err := memory.EmitHealthChanged(context.Background(), deps.Bus,
		identity.Quadruple{}, memory.HealthHealthy, memory.HealthRetry, "x")
	if err == nil {
		t.Fatal("err=nil, want ErrIdentityRequired")
	}
}

// TestTruncation_AfterClose_ErrorsCleanly covers the closed-store
// error paths on truncation.
func TestTruncation_AfterClose_ErrorsCleanly(t *testing.T) {
	deps := buildDeps(t, nil)
	exec, err := strategy.New(memory.StrategyTruncation, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exec.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()
	id := tripleA()
	if err := exec.AddTurn(ctx, id, memory.ConversationTurn{}); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("AddTurn after Close: err=%v, want ErrStoreClosed", err)
	}
	if _, err := exec.GetLLMContext(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("GetLLMContext after Close: err=%v, want ErrStoreClosed", err)
	}
	if _, err := exec.EstimateTokens(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("EstimateTokens after Close: err=%v, want ErrStoreClosed", err)
	}
	if err := exec.Flush(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("Flush after Close: err=%v, want ErrStoreClosed", err)
	}
	if _, err := exec.Health(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("Health after Close: err=%v, want ErrStoreClosed", err)
	}
	if _, err := exec.Snapshot(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("Snapshot after Close: err=%v, want ErrStoreClosed", err)
	}
	if err := exec.Restore(ctx, id, memory.Snapshot{}); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("Restore after Close: err=%v, want ErrStoreClosed", err)
	}
}

// TestRollingSummary_AfterClose_ErrorsCleanly covers the closed-
// store error paths on rolling_summary.
func TestRollingSummary_AfterClose_ErrorsCleanly(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := exec.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Second Close is idempotent.
	if err := exec.Close(context.Background()); err != nil {
		t.Errorf("Close (2nd): %v", err)
	}
	ctx := context.Background()
	id := tripleA()
	if err := exec.AddTurn(ctx, id, memory.ConversationTurn{}); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("AddTurn after Close: err=%v, want ErrStoreClosed", err)
	}
	if _, err := exec.GetLLMContext(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("GetLLMContext after Close: err=%v, want ErrStoreClosed", err)
	}
	if _, err := exec.Health(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("Health after Close: err=%v, want ErrStoreClosed", err)
	}
	if _, err := exec.Snapshot(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("Snapshot after Close: err=%v, want ErrStoreClosed", err)
	}
	if err := exec.Restore(ctx, id, memory.Snapshot{}); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("Restore after Close: err=%v, want ErrStoreClosed", err)
	}
	if err := exec.Flush(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("Flush after Close: err=%v, want ErrStoreClosed", err)
	}
	if _, err := exec.EstimateTokens(ctx, id); !errors.Is(err, memory.ErrStoreClosed) {
		t.Errorf("EstimateTokens after Close: err=%v, want ErrStoreClosed", err)
	}
}

// TestTruncation_LoadFromStateStore covers the loadIfNeeded path
// where a persisted truncation record exists from a prior run —
// the new executor must rehydrate the buffer.
func TestTruncation_LoadFromStateStore(t *testing.T) {
	deps := buildDeps(t, nil)
	exec1, err := strategy.New(memory.StrategyTruncation, deps)
	if err != nil {
		t.Fatalf("New 1: %v", err)
	}
	ctx := context.Background()
	id := tripleA()
	if err := exec1.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	_ = exec1.Close(ctx)

	// Fresh executor on the same StateStore — must rehydrate.
	exec2, err := strategy.New(memory.StrategyTruncation, deps)
	if err != nil {
		t.Fatalf("New 2: %v", err)
	}
	defer exec2.Close(ctx)
	patch, err := exec2.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if len(patch.RecentTurns) != 1 {
		t.Errorf("RecentTurns after rehydrate=%d, want 1", len(patch.RecentTurns))
	}
}

// TestEmitRecoveryDropped covers the recovery-dropped emit helper.
func TestEmitRecoveryDropped(t *testing.T) {
	deps := buildDeps(t, nil)
	id := tripleA()
	sub, err := deps.Bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{memory.EventTypeMemoryRecoveryDropped},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	if err := memory.EmitRecoveryDropped(context.Background(), deps.Bus, id, "test"); err != nil {
		t.Fatalf("EmitRecoveryDropped: %v", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != memory.EventTypeMemoryRecoveryDropped {
			t.Errorf("event type=%q, want %q", ev.Type, memory.EventTypeMemoryRecoveryDropped)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovery_dropped event")
	}

	// Missing identity → ErrIdentityRequired.
	err = memory.EmitRecoveryDropped(context.Background(), deps.Bus,
		identity.Quadruple{}, "test")
	if err == nil {
		t.Fatal("err=nil, want ErrIdentityRequired")
	}
}
