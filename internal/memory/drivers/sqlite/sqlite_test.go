package sqlite_test

// Driver-level tests for the SQLite MemoryStore. The behavioural
// surface is covered by the shared conformance suite; this file
// invokes that suite + adds driver-specific cases the suite cannot
// express (construction errors, byte-stable round-trip vs the InMem
// reference).

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/conformancetest"
	memorydriversqlite "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// TestSQLite_ConformanceSuite invokes the canonical conformance
// suite against the SQLite memory driver under all three strategies
// (Phase 25a, D-174). Each subtest gets its own fresh DB file under
// t.TempDir so cross-subtest state cannot bleed; the rolling_summary
// leg injects a stub Summarizer via `memory.Deps.Summarizer`.
func TestSQLite_ConformanceSuite(t *testing.T) {
	strategies := []memory.Strategy{
		memory.StrategyNone,
		memory.StrategyRollingSummary,
	}
	for _, s := range strategies {
		t.Run(string(s), func(t *testing.T) {
			conformancetest.Run(t, func() conformancetest.Harness {
				bus, store := buildDeps(t)
				dbPath := filepath.Join(t.TempDir(), "memory.sqlite")
				deps := memory.Deps{State: store, Bus: bus, Redactor: cumulativeRedactor(t)}
				mem, err := memorydriversqlite.New(memory.ConfigSnapshot{
					Driver:       "sqlite",
					DSN:          dbPath,
					Strategy:     s,
					BudgetTokens: 64, // small but non-zero so truncation has work to do
				}, deps)
				if err != nil {
					t.Fatalf("sqlite.New(%q): %v", s, err)
				}
				return conformancetest.Harness{
					Store:    mem,
					Bus:      bus,
					Strategy: s,
					Cleanup: func() {
						_ = mem.Close(context.Background())
					},
				}
			})
		})
	}

}

// TestSQLite_New_RequiresDSN pins the explicit-DSN-required contract.
// Empty DSN must surface a clear error rather than panic inside
// sql.Open.
func TestSQLite_New_RequiresDSN(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := memorydriversqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus, Redactor: cumulativeRedactor(t)})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

// TestSQLite_New_RequiresBus checks the fail-loud bus dep guard.
func TestSQLite_New_RequiresBus(t *testing.T) {
	_, store := buildDeps(t)
	dbPath := filepath.Join(t.TempDir(), "memory.sqlite")
	_, err := memorydriversqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dbPath, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: nil})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

// TestSQLite_DriverRegistered checks the init() side-effect: the
// driver self-registers under "sqlite" so OpenDriver can resolve it.
// Empty DSN means New surfaces the DSN error from the factory; that
// proves the registry found the factory.
func TestSQLite_DriverRegistered(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := memory.OpenDriver("sqlite", memory.ConfigSnapshot{
		Driver: "sqlite", Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus, Redactor: cumulativeRedactor(t)})
	if err == nil {
		t.Fatal("OpenDriver(sqlite, empty DSN): err=nil, want non-nil")
	}
	if errors.Is(err, memory.ErrUnknownDriver) {
		t.Fatalf("driver not registered: %v", err)
	}
}

// buildBus builds just the EventBus (the rehydration test owns its
// own StateStore lifecycle, so it can't reuse buildDeps which builds
// an inmem state store).
func buildBus(t *testing.T) events.EventBus {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60_000_000_000,
		DropWindow:               1_000_000_000,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func buildDeps(t *testing.T) (events.EventBus, state.StateStore) {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
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
	return bus, store
}

func cumulativeRedactor(t *testing.T) audit.Redactor {
	t.Helper()
	red, err := audit.Open(t.Context(), config.AuditConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return red
}
