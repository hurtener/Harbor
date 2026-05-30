package inmem_test

// Driver-level tests for the InMem MemoryStore. The behavioural
// surface is covered by the conformance suite (invoked here against
// all three strategies); this file only adds driver-specific cases
// the suite cannot express (e.g. construction errors).

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/conformancetest"
	"github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory/strategy"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestInMem_ConformanceSuite invokes the canonical conformance suite
// against the inmem driver under all three strategies. Phase 25's
// SQLite + Postgres drivers MUST invoke the same suite via their
// own *_test.go files.
func TestInMem_ConformanceSuite(t *testing.T) {
	strategies := []memory.Strategy{
		memory.StrategyNone,
		memory.StrategyTruncation,
		memory.StrategyRollingSummary,
	}
	for _, s := range strategies {

		t.Run(string(s), func(t *testing.T) {
			conformancetest.Run(t, func() conformancetest.Harness {
				return newHarness(t, s)
			})
		})
	}
}

func newHarness(t *testing.T, s memory.Strategy) conformancetest.Harness {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), driverEventsConfig(), red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	opts := inmem.Options{}
	if s == memory.StrategyRollingSummary {
		opts.Summarizer = strategy.EchoSummarizer{}
	}
	mem, err := inmem.New(memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     s,
		BudgetTokens: 64, // small but non-zero so truncation has work to do
	}, memory.Deps{State: store, Bus: bus}, opts)
	if err != nil {
		t.Fatalf("inmem.New(%q): %v", s, err)
	}
	return conformancetest.Harness{
		Store:    mem,
		Bus:      bus,
		Strategy: s,
		Cleanup: func() {
			_ = mem.Close(context.Background())
			_ = bus.Close(context.Background())
			_ = store.Close(context.Background())
		},
	}
}

func TestInMem_New_RejectsRollingSummaryWithoutSummarizer(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyRollingSummary,
	}, memory.Deps{State: store, Bus: bus}, inmem.Options{})
	if err == nil {
		t.Fatal("err=nil, want non-nil for rolling_summary without summarizer")
	}
}

func TestInMem_New_RejectsUnknownStrategy(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.Strategy("not-a-strategy"),
	}, memory.Deps{State: store, Bus: bus}, inmem.Options{})
	if !errors.Is(err, memory.ErrStrategyNotImplemented) {
		t.Fatalf("err=%v, want errors.Is ErrStrategyNotImplemented", err)
	}
}

func TestInMem_New_RejectsNilState(t *testing.T) {
	bus, _ := buildDeps(t)
	_, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyNone,
	}, memory.Deps{State: nil, Bus: bus}, inmem.Options{})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

func TestInMem_New_RejectsNilBus(t *testing.T) {
	_, store := buildDeps(t)
	_, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: nil}, inmem.Options{})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

func TestInMem_New_DefaultsToStrategyNone(t *testing.T) {
	bus, store := buildDeps(t)
	mem, err := inmem.New(memory.ConfigSnapshot{
		Driver: "inmem",
		// Strategy intentionally empty — must default to none.
	}, memory.Deps{State: store, Bus: bus}, inmem.Options{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	defer mem.Close(context.Background())
}

// TestInMem_RegistryOpen_RejectsRollingSummaryWithoutSummarizer
// asserts the fail-loud contract (AC-6): the registry path rejects
// rolling_summary when no Summarizer is supplied — never a stub
// fallback (AGENTS.md §13).
func TestInMem_RegistryOpen_RejectsRollingSummaryWithoutSummarizer(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyRollingSummary,
	}, memory.Deps{State: store, Bus: bus})
	if err == nil {
		t.Fatal("err=nil, want non-nil (rolling_summary needs summariser)")
	}
}

// TestInMem_RegistryOpen_AcceptsRollingSummaryWithSummarizer asserts
// the Phase 25a (D-174) win: with a Summarizer threaded through
// `memory.Deps.Summarizer`, rolling_summary is now registry-reachable.
func TestInMem_RegistryOpen_AcceptsRollingSummaryWithSummarizer(t *testing.T) {
	bus, store := buildDeps(t)
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyRollingSummary,
	}, memory.Deps{State: store, Bus: bus, Summarizer: strategy.EchoSummarizer{}})
	if err != nil {
		t.Fatalf("memory.Open(rolling_summary, with summarizer): %v", err)
	}
	defer func() { _ = mem.Close(context.Background()) }()
}

func driverEventsConfig() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60_000_000_000,
		DropWindow:               1_000_000_000,
	}
}

func buildDeps(t *testing.T) (events.EventBus, state.StateStore) {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), driverEventsConfig(), red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return bus, store
}
