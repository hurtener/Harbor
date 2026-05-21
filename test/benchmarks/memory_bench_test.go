package benchmarks

import (
	"context"
	"fmt"
	"testing"
	"time"

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

// memBenchDeps assembles the strategy.Deps the memory benchmarks
// run against: a real `inmem` StateStore (the persistence floor),
// a real `inmem` EventBus, a real `audit` redactor, and the
// EchoSummarizer for the rolling-summary path. EchoSummarizer is a
// test-grade Summarizer (CLAUDE.md §13) — using it here is correct
// because this file is `_test.go` and the LLM-backed Summarizer
// (Phase 32+) is not part of the benchmarked memory subsystem.
func memBenchDeps(b *testing.B) strategy.Deps {
	b.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		b.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		b.Fatalf("events.Open: %v", err)
	}
	b.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		b.Fatalf("state.Open: %v", err)
	}
	b.Cleanup(func() { _ = store.Close(context.Background()) })

	return strategy.Deps{
		State:      store,
		Bus:        bus,
		Summarizer: strategy.EchoSummarizer{},
	}
}

func memBenchTurn(i int) memory.ConversationTurn {
	return memory.ConversationTurn{
		UserMessage:       fmt.Sprintf("user message %d with some representative length", i),
		AssistantResponse: fmt.Sprintf("assistant response %d, also of representative length", i),
		Timestamp:         time.Unix(int64(i), 0),
	}
}

// BenchmarkMemoryStrategy measures memory-strategy AddTurn latency
// for the `truncation` and `rolling_summary` executors — the
// master-plan's "memory-strategy latency (truncation vs
// rolling_summary)" axis. Each sub-benchmark drives AddTurn against
// a real strategy executor wired with real drivers; the
// rolling-summary path additionally exercises the EchoSummarizer
// fold-into-summary edge once turns spill past FullZoneTurns.
//
// Identity is propagated end-to-end: every AddTurn call carries an
// identity.Quadruple, and the executor scopes its per-key state by
// the triple — the §17 identity-propagation obligation discharged
// inside the benchmark.
func BenchmarkMemoryStrategy(b *testing.B) {
	cases := []struct {
		name  string
		strat memory.Strategy
	}{
		{"truncation", memory.StrategyTruncation},
		{"rolling_summary", memory.StrategyRollingSummary},
	}

	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			deps := memBenchDeps(b)
			// A small per-key budget so truncation actually evicts
			// and rolling_summary actually summarises — the
			// benchmark must hit the hot path, not the trivial
			// "buffer not yet full" path.
			deps.BudgetTokens = 64
			exec, err := strategy.New(tc.strat, deps)
			if err != nil {
				b.Fatalf("strategy.New(%s): %v", tc.strat, err)
			}
			b.Cleanup(func() { _ = exec.Close(context.Background()) })

			id := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  "bench-tenant",
					UserID:    "bench-user",
					SessionID: "bench-session",
				},
				RunID: "bench-run",
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := exec.AddTurn(context.Background(), id, memBenchTurn(i)); err != nil {
					b.Fatalf("AddTurn: %v", err)
				}
			}
			b.StopTimer()

			secs := b.Elapsed().Seconds()
			if secs > 0 {
				b.ReportMetric(float64(b.N)/secs, "turns/sec")
			}
		})
	}
}

// BenchmarkMemoryGetLLMContext measures the read path —
// GetLLMContext — for both strategies. A planner runtime calls this
// once per LLM round-trip, so its latency is on the agent's hot
// path. The benchmark pre-loads a window of turns, then times the
// context-patch construction.
func BenchmarkMemoryGetLLMContext(b *testing.B) {
	cases := []struct {
		name  string
		strat memory.Strategy
	}{
		{"truncation", memory.StrategyTruncation},
		{"rolling_summary", memory.StrategyRollingSummary},
	}

	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			deps := memBenchDeps(b)
			deps.BudgetTokens = 64
			exec, err := strategy.New(tc.strat, deps)
			if err != nil {
				b.Fatalf("strategy.New(%s): %v", tc.strat, err)
			}
			b.Cleanup(func() { _ = exec.Close(context.Background()) })

			id := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  "bench-tenant",
					UserID:    "bench-user",
					SessionID: "bench-session",
				},
				RunID: "bench-run",
			}
			for i := 0; i < 16; i++ {
				if err := exec.AddTurn(context.Background(), id, memBenchTurn(i)); err != nil {
					b.Fatalf("AddTurn(setup): %v", err)
				}
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := exec.GetLLMContext(context.Background(), id); err != nil {
					b.Fatalf("GetLLMContext: %v", err)
				}
			}
		})
	}
}
