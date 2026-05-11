package strategy_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/strategy"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestValidateHealthTransition_Matrix exercises the full
// (Health × Health) matrix and asserts each pair matches the
// documented FSM table. The matrix is the load-bearing way to pin
// the transition shape; a future edit that adds / drops an edge
// MUST update this test in the same PR.
func TestValidateHealthTransition_Matrix(t *testing.T) {
	allHealths := []memory.Health{
		memory.HealthHealthy,
		memory.HealthRetry,
		memory.HealthDegraded,
		memory.HealthRecovering,
	}
	// validEdges enumerates the legal `(prior → next)` pairs. Keep
	// in sync with the `healthTransitions` table in
	// `internal/memory/memory.go`. Self-loops are valid.
	validEdges := map[[2]memory.Health]struct{}{
		{memory.HealthHealthy, memory.HealthHealthy}:       {},
		{memory.HealthHealthy, memory.HealthRetry}:         {},
		{memory.HealthRetry, memory.HealthRetry}:           {},
		{memory.HealthRetry, memory.HealthHealthy}:         {},
		{memory.HealthRetry, memory.HealthDegraded}:        {},
		{memory.HealthDegraded, memory.HealthDegraded}:     {},
		{memory.HealthDegraded, memory.HealthRecovering}:   {},
		{memory.HealthRecovering, memory.HealthRecovering}: {},
		{memory.HealthRecovering, memory.HealthHealthy}:    {},
		{memory.HealthRecovering, memory.HealthDegraded}:   {},
	}
	for _, p := range allHealths {
		for _, n := range allHealths {
			p, n := p, n
			t.Run(string(p)+"_to_"+string(n), func(t *testing.T) {
				_, ok := validEdges[[2]memory.Health{p, n}]
				err := memory.ValidateHealthTransition(p, n)
				if ok {
					if err != nil {
						t.Errorf("ValidateHealthTransition(%q,%q)=%v, want nil", p, n, err)
					}
				} else {
					if !errors.Is(err, memory.ErrInvalidHealthTransition) {
						t.Errorf("ValidateHealthTransition(%q,%q)=%v, want errors.Is ErrInvalidHealthTransition", p, n, err)
					}
				}
			})
		}
	}
}

// TestValidateHealthTransition_EmptyHealthMeansHealthy pins the
// "empty Health{} is treated as HealthHealthy" rule documented on
// the function. A freshly-constructed executor's first transition
// should be observable as `HealthHealthy → next`.
func TestValidateHealthTransition_EmptyHealthMeansHealthy(t *testing.T) {
	if err := memory.ValidateHealthTransition("", memory.HealthRetry); err != nil {
		t.Errorf("ValidateHealthTransition(\"\", retry)=%v, want nil", err)
	}
	if err := memory.ValidateHealthTransition(memory.HealthHealthy, ""); err != nil {
		t.Errorf("ValidateHealthTransition(healthy, \"\")=%v, want nil", err)
	}
}

func TestNew_RoutesByStrategy(t *testing.T) {
	deps := buildDeps(t, nil)
	cases := map[string]struct {
		s     memory.Strategy
		want  bool // expect success
		canon bool // expect canonical-strategy panic / error
	}{
		"none":            {s: memory.StrategyNone, want: true},
		"truncation":      {s: memory.StrategyTruncation, want: true},
		"rolling_summary": {s: memory.StrategyRollingSummary, want: false}, // needs summariser
		"unknown":         {s: memory.Strategy("bogus"), want: false},
	}
	for name, tc := range cases {
		name, tc := name, tc
		t.Run(name, func(t *testing.T) {
			exec, err := strategy.New(tc.s, deps)
			if tc.want {
				if err != nil {
					t.Fatalf("New(%q): %v", tc.s, err)
				}
				_ = exec.Close(context.Background())
			} else {
				if err == nil {
					_ = exec.Close(context.Background())
					t.Fatalf("New(%q): err=nil, want non-nil", tc.s)
				}
			}
		})
	}
}

func TestNew_RollingSummary_AcceptsSummarizer(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New rolling_summary: %v", err)
	}
	defer exec.Close(context.Background())
	got, err := exec.Health(context.Background(), tripleA())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got != memory.HealthHealthy {
		t.Errorf("Health=%q, want %q", got, memory.HealthHealthy)
	}
}

func TestNew_RejectsNegativeBacklog(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	deps.RecoveryBacklogMax = -1
	_, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

func TestEchoSummarizer_Summarize(t *testing.T) {
	s := strategy.EchoSummarizer{}
	resp, err := s.Summarize(context.Background(), tripleA(), memory.SummarizeRequest{
		PreviousSummary: "prev",
		Turns: []memory.ConversationTurn{
			{UserMessage: "hello", AssistantResponse: "hi"},
			{UserMessage: "how", AssistantResponse: "well"},
		},
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	want := "prev\nU:hello A:hi\nU:how A:well"
	if resp.Summary != want {
		t.Errorf("Summary=%q, want %q", resp.Summary, want)
	}
}

func TestEchoSummarizer_NoPrevious(t *testing.T) {
	s := strategy.EchoSummarizer{}
	resp, err := s.Summarize(context.Background(), tripleA(), memory.SummarizeRequest{
		Turns: []memory.ConversationTurn{
			{UserMessage: "x", AssistantResponse: "y"},
		},
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if resp.Summary != "U:x A:y" {
		t.Errorf("Summary=%q, want %q", resp.Summary, "U:x A:y")
	}
}

// TestTruncation_AddTurn_RoundTrip exercises the truncation
// executor's happy path through Snapshot/Restore.
func TestTruncation_AddTurn_RoundTrip(t *testing.T) {
	deps := buildDeps(t, nil)
	exec, err := strategy.New(memory.StrategyTruncation, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := exec.AddTurn(ctx, tripleA(), memory.ConversationTurn{
			UserMessage:       "u",
			AssistantResponse: "a",
		}); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	patch, err := exec.GetLLMContext(ctx, tripleA())
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if len(patch.RecentTurns) != 3 {
		t.Errorf("RecentTurns=%d, want 3", len(patch.RecentTurns))
	}
	snap, err := exec.Snapshot(ctx, tripleA())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := exec.Restore(ctx, tripleA(), snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	patch2, err := exec.GetLLMContext(ctx, tripleA())
	if err != nil {
		t.Fatalf("GetLLMContext after restore: %v", err)
	}
	if len(patch2.RecentTurns) != 3 {
		t.Errorf("RecentTurns after restore=%d, want 3", len(patch2.RecentTurns))
	}
}

// TestTruncation_Concurrent_NoRace exercises D-025 against the
// truncation executor. N=128 goroutines × 8 ops each × different
// identities; the same executor instance is shared across all
// goroutines.
func TestTruncation_Concurrent_NoRace(t *testing.T) {
	deps := buildDeps(t, nil)
	deps.BudgetTokens = 64
	exec, err := strategy.New(memory.StrategyTruncation, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())

	const goroutines = 128
	const opsPerGo = 8
	var wg sync.WaitGroup
	var errCount atomic.Int64
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			ctx := context.Background()
			id := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  "t",
					UserID:    "u",
					SessionID: "s" + intToStr(i),
				},
			}
			for j := 0; j < opsPerGo; j++ {
				switch j % 4 {
				case 0:
					if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "x"}); err != nil {
						errCount.Add(1)
					}
				case 1:
					if _, err := exec.GetLLMContext(ctx, id); err != nil {
						errCount.Add(1)
					}
				case 2:
					if _, err := exec.Snapshot(ctx, id); err != nil {
						errCount.Add(1)
					}
				case 3:
					if err := exec.Flush(ctx, id); err != nil {
						errCount.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()
	if n := errCount.Load(); n != 0 {
		t.Errorf("%d concurrent operations errored", n)
	}
}

// failingSummarizer always returns an error — used to drive
// rolling_summary's failure → degraded path.
type failingSummarizer struct{}

func (failingSummarizer) Summarize(_ context.Context, _ identity.Quadruple, _ memory.SummarizeRequest) (memory.SummarizeResponse, error) {
	return memory.SummarizeResponse{}, errors.New("forced failure")
}

// TestRollingSummary_FailureDegradesAndEmits exercises the rolling-
// summary failure path: a failing summariser drives the executor
// through `healthy → retry → degraded`, with `memory.health_changed`
// observable on the bus. After degradation the recent-window
// fallback keeps the conversation usable.
func TestRollingSummary_FailureDegradesAndEmits(t *testing.T) {
	deps := buildDeps(t, failingSummarizer{})
	deps.RecoveryBacklogMax = 4
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
	// Push enough turns to spill into pending repeatedly and
	// exhaust the retry budget.
	for i := 0; i < 12; i++ {
		if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"}); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}

	// Check health: should be HealthDegraded after retries
	// exhausted.
	got, err := exec.Health(ctx, id)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got != memory.HealthDegraded {
		t.Errorf("Health=%q, want %q", got, memory.HealthDegraded)
	}

	// Degraded patch must drop the (stale) summary.
	patch, err := exec.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Summary != "" {
		t.Errorf("degraded patch carries summary: %q", patch.Summary)
	}

	// At least one health_changed event observable.
	select {
	case ev := <-sub.Events():
		if ev.Type != memory.EventTypeMemoryHealthChanged {
			t.Errorf("event type=%q, want %q", ev.Type, memory.EventTypeMemoryHealthChanged)
		}
	default:
		// The bus is async; if no event is buffered yet, fall
		// through (the AddTurn loop has already finished, so any
		// in-flight Publish has either landed or is racing).
		// To make the test deterministic we Subscribe BEFORE the
		// AddTurn loop and the events are accumulated in the
		// subscriber buffer — so the default branch should not
		// hit. If it does, that's a real bug.
		t.Error("no health_changed event observable")
	}
}

// TestRollingSummary_BacklogBound exercises the bounded recovery
// queue: with `RecoveryBacklogMax = 2`, a flood of failing
// summarisations should keep the backlog at 2 and emit
// `memory.recovery_dropped` for the rest.
func TestRollingSummary_BacklogBound(t *testing.T) {
	deps := buildDeps(t, failingSummarizer{})
	deps.RecoveryBacklogMax = 2
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())

	sub, err := deps.Bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{memory.EventTypeMemoryRecoveryDropped},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	ctx := context.Background()
	id := tripleA()
	// Push enough turns to flood the backlog. Each "batch" is the
	// `pending` queue contents at AddTurn time; we need
	// (retryAttempts + backlogMax + extra) failing AddTurns to see
	// the drop emit.
	for i := 0; i < 30; i++ {
		_ = exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"})
	}

	// Drain whatever recovery_dropped events landed on the
	// subscriber. We assert at least one drop is observable —
	// implies the backlog hit the bound.
	gotDrop := false
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				goto done
			}
			if ev.Type == memory.EventTypeMemoryRecoveryDropped {
				gotDrop = true
			}
		default:
			goto done
		}
	}
done:
	if !gotDrop {
		t.Error("no memory.recovery_dropped event observable")
	}
}

// TestRollingSummary_HappyPath exercises the success path: a
// working EchoSummarizer drives the executor through repeated
// AddTurns, summary materialises, Health stays healthy.
func TestRollingSummary_HappyPath(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())
	ctx := context.Background()
	id := tripleA()
	for i := 0; i < 8; i++ {
		if err := exec.AddTurn(ctx, id, memory.ConversationTurn{
			UserMessage:       "u" + intToStr(i),
			AssistantResponse: "a" + intToStr(i),
		}); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	got, err := exec.Health(ctx, id)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got != memory.HealthHealthy {
		t.Errorf("Health=%q, want %q", got, memory.HealthHealthy)
	}
	patch, err := exec.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Summary == "" {
		t.Error("happy-path rolling_summary returned empty summary after 8 turns")
	}
}

// TestRollingSummary_Concurrent_NoRace exercises D-025 against the
// rolling-summary executor: N=128 goroutines × 8 ops each × per-
// goroutine identity, against one shared executor.
func TestRollingSummary_Concurrent_NoRace(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer exec.Close(context.Background())

	const goroutines = 128
	const opsPerGo = 8
	var wg sync.WaitGroup
	var errCount atomic.Int64
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			ctx := context.Background()
			id := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  "t",
					UserID:    "u",
					SessionID: "s" + intToStr(i),
				},
			}
			for j := 0; j < opsPerGo; j++ {
				switch j % 5 {
				case 0:
					if err := exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "x"}); err != nil {
						errCount.Add(1)
					}
				case 1:
					if _, err := exec.GetLLMContext(ctx, id); err != nil {
						errCount.Add(1)
					}
				case 2:
					if _, err := exec.Snapshot(ctx, id); err != nil {
						errCount.Add(1)
					}
				case 3:
					if _, err := exec.Health(ctx, id); err != nil {
						errCount.Add(1)
					}
				case 4:
					if _, err := exec.EstimateTokens(ctx, id); err != nil {
						errCount.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()
	if n := errCount.Load(); n != 0 {
		t.Errorf("%d concurrent operations errored", n)
	}
}

// --- helpers ---

func buildDeps(t *testing.T, sum memory.Summarizer) strategy.Deps {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              60_000_000_000,
		DropWindow:               1_000_000_000,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return strategy.Deps{
		State:      store,
		Bus:        bus,
		Summarizer: sum,
	}
}

func tripleA() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
}

func intToStr(i int) string {
	// Cheap base-10 formatter — avoids strconv in this file's
	// import surface. The values fit in int32 always.
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
