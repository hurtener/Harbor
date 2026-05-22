package strategy_test

// Recovery-loop tests. Exercises the `drainBacklogs` / `recoverOne`
// paths without waiting on the 10s ticker — `DrainBacklogsForTest`
// is a same-package test helper.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/strategy"
)

// flakySummarizer fails the first N calls, then succeeds. After
// the recover() switches it back, calls succeed deterministically.
type flakySummarizer struct {
	failCount int32
	calls     atomic.Int32
}

func (s *flakySummarizer) Summarize(_ context.Context, _ identity.Quadruple, req memory.SummarizeRequest) (memory.SummarizeResponse, error) {
	if s.calls.Add(1) <= s.failCount {
		return memory.SummarizeResponse{}, errSummarizerOutage
	}
	return memory.SummarizeResponse{Summary: req.PreviousSummary + "|ok"}, nil
}

// TestRollingSummary_RecoveryLoop_DrainsBacklog forces a transient
// summariser outage, then flips the summariser to "always
// succeed", and triggers a synchronous drain via the test helper.
// Asserts the backlog drains and Health transitions back to healthy.
func TestRollingSummary_RecoveryLoop_DrainsBacklog(t *testing.T) {
	flaky := &flakySummarizer{failCount: 100}
	deps := buildDeps(t, flaky)
	deps.RecoveryBacklogMax = 4
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())

	r, ok := strategy.AsRollingSummary(exec)
	if !ok {
		t.Fatalf("AsRollingSummary: not a rolling-summary executor")
	}

	ctx := context.Background()
	id := tripleA()
	// Push enough turns to fill the backlog.
	for range 30 {
		_ = exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"})
	}
	if got := r.HealthForTest(id); got != memory.HealthDegraded {
		t.Fatalf("Health=%q, want degraded", got)
	}
	if r.BacklogSize(id) == 0 {
		t.Fatal("backlog empty after forced failures")
	}

	// Flip the summariser to success.
	flaky.failCount = -1

	// Subscribe to health_changed so we observe the recovery
	// transitions.
	sub, err := deps.Bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{memory.EventTypeMemoryHealthChanged},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Drain repeatedly until backlog is empty (each drain
	// processes one batch per key).
	for range 16 {
		_ = r.DrainBacklogsForTest()
		if r.BacklogSize(id) == 0 {
			break
		}
	}
	if got := r.BacklogSize(id); got != 0 {
		t.Errorf("backlog after drains=%d, want 0", got)
	}
	if got := r.HealthForTest(id); got != memory.HealthHealthy {
		t.Errorf("Health after recovery=%q, want healthy", got)
	}

	// At least one degraded → recovering and one recovering →
	// healthy observable on the bus.
	gotRecovering := false
	gotHealthy := false
	deadline := time.After(2 * time.Second)
loop:
	for !gotRecovering || !gotHealthy {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				break loop
			}
			payload, ok := ev.Payload.(memory.HealthChangedPayload)
			if !ok {
				continue
			}
			if payload.NewHealth == memory.HealthRecovering {
				gotRecovering = true
			}
			if payload.NewHealth == memory.HealthHealthy && payload.PriorHealth == memory.HealthRecovering {
				gotHealthy = true
			}
		case <-deadline:
			break loop
		}
	}
	if !gotRecovering {
		t.Error("did not observe degraded → recovering transition")
	}
	if !gotHealthy {
		t.Error("did not observe recovering → healthy transition")
	}
}

// TestRollingSummary_RecoveryLoop_RetriesBatchOnFailure asserts
// that a failing recovery batch transitions back to degraded
// (the batch stays at the head; next tick retries).
func TestRollingSummary_RecoveryLoop_RetriesBatchOnFailure(t *testing.T) {
	flaky := &flakySummarizer{failCount: 1000} // keep failing forever
	deps := buildDeps(t, flaky)
	deps.RecoveryBacklogMax = 4
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())

	r, ok := strategy.AsRollingSummary(exec)
	if !ok {
		t.Fatalf("AsRollingSummary: not a rolling-summary executor")
	}

	ctx := context.Background()
	id := tripleA()
	for range 12 {
		_ = exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"})
	}
	if r.BacklogSize(id) == 0 {
		t.Fatal("backlog empty after forced failures")
	}
	preBacklog := r.BacklogSize(id)
	// One drain attempt — summariser still fails, batch should
	// stay at head; transition recovering → degraded.
	r.DrainBacklogsForTest()
	if r.HealthForTest(id) != memory.HealthDegraded {
		t.Errorf("Health after failed recovery=%q, want degraded", r.HealthForTest(id))
	}
	if r.BacklogSize(id) != preBacklog {
		t.Errorf("backlog after failed recovery=%d, want %d (unchanged)", r.BacklogSize(id), preBacklog)
	}
}

// TestRollingSummary_RecoveryLoop_NoOpWhenHealthy covers the
// guard at the top of recoverOne — a healthy executor's drain
// call should be a no-op.
func TestRollingSummary_RecoveryLoop_NoOpWhenHealthy(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())

	r, ok := strategy.AsRollingSummary(exec)
	if !ok {
		t.Fatalf("AsRollingSummary: not a rolling-summary executor")
	}

	ctx := context.Background()
	id := tripleA()
	for range 4 {
		_ = exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"})
	}
	// Drain on a healthy executor should be a no-op.
	_ = r.DrainBacklogsForTest()
	if r.HealthForTest(id) != memory.HealthHealthy {
		t.Errorf("Health after drain on healthy=%q, want healthy", r.HealthForTest(id))
	}
}
