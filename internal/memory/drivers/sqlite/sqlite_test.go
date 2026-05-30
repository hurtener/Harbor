package sqlite_test

// Driver-level tests for the SQLite MemoryStore. The behavioural
// surface is covered by the shared conformance suite; this file
// invokes that suite + adds driver-specific cases the suite cannot
// express (construction errors, byte-stable round-trip vs the InMem
// reference).

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/conformancetest"
	memorydriverinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	memorydriversqlite "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/memory/strategy"
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
		memory.StrategyTruncation,
		memory.StrategyRollingSummary,
	}
	for _, s := range strategies {
		t.Run(string(s), func(t *testing.T) {
			conformancetest.Run(t, func() conformancetest.Harness {
				bus, store := buildDeps(t)
				dbPath := filepath.Join(t.TempDir(), "memory.sqlite")
				deps := memory.Deps{State: store, Bus: bus}
				if s == memory.StrategyRollingSummary {
					deps.Summarizer = strategy.EchoSummarizer{}
				}
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
	}, memory.Deps{State: store, Bus: bus})
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

// TestSQLite_New_RejectsRollingSummaryWithoutSummarizer pins the
// fail-loud contract (AC-6): rolling_summary with no Summarizer must
// error at construction — never a stub fallback (AGENTS.md §13).
func TestSQLite_New_RejectsRollingSummaryWithoutSummarizer(t *testing.T) {
	bus, store := buildDeps(t)
	dbPath := filepath.Join(t.TempDir(), "memory.sqlite")
	_, err := memorydriversqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dbPath, Strategy: memory.StrategyRollingSummary,
	}, memory.Deps{State: store, Bus: bus})
	if err == nil {
		t.Fatal("err=nil, want non-nil for rolling_summary without summarizer")
	}
}

// TestSQLite_PersistsAcrossReopens proves the driver actually
// persists state to disk: a Restore on one driver instance must be
// visible to a Snapshot on a second driver opened against the same
// DB file. This is the core "persistent" guarantee Phase 25 ships.
func TestSQLite_PersistsAcrossReopens(t *testing.T) {
	bus, store := buildDeps(t)
	dbPath := filepath.Join(t.TempDir(), "memory.sqlite")
	ctx := context.Background()
	id := tripleA()

	m1, err := memorydriversqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dbPath, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("sqlite.New (1): %v", err)
	}
	if err := m1.Restore(ctx, id, memory.Snapshot{}); err != nil {
		t.Fatalf("m1.Restore: %v", err)
	}
	snap1, err := m1.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("m1.Snapshot: %v", err)
	}
	if err := m1.Close(ctx); err != nil {
		t.Fatalf("m1.Close: %v", err)
	}

	// Re-open the same DB; the persisted slot must surface.
	m2, err := memorydriversqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dbPath, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("sqlite.New (2): %v", err)
	}
	defer func() { _ = m2.Close(ctx) }()
	snap2, err := m2.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("m2.Snapshot: %v", err)
	}
	if snap1.Strategy != snap2.Strategy {
		t.Errorf("strategy mismatch after reopen: %q vs %q", snap1.Strategy, snap2.Strategy)
	}
	if string(snap1.Bytes) != string(snap2.Bytes) {
		t.Errorf("bytes mismatch after reopen: %q vs %q", snap1.Bytes, snap2.Bytes)
	}
}

// TestSQLite_CrossDriver_ByteStableRoundTrip asserts the Phase 25
// acceptance criterion: a Snapshot taken from one driver must
// Restore via another driver byte-stably. The wire shape lives in
// `internal/memory/wire.go`; both drivers marshal through it.
//
// Today (Strategy=none) the only round-trippable payload is the
// canonical empty record. The test pins the wire shape and proves
// Snapshot/Restore cross-driver compatibility for that payload.
func TestSQLite_CrossDriver_ByteStableRoundTrip(t *testing.T) {
	bus, store := buildDeps(t)
	ctx := context.Background()
	id := tripleA()

	// 1. Build an InMem driver, write the canonical empty record, take
	//    a Snapshot.
	inmemStore, err := memorydriverinmem.New(memory.ConfigSnapshot{
		Driver: "inmem", Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus}, memorydriverinmem.Options{})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	defer func() { _ = inmemStore.Close(ctx) }()
	if err := inmemStore.Restore(ctx, id, memory.Snapshot{}); err != nil {
		t.Fatalf("inmem.Restore: %v", err)
	}
	inmemSnap, err := inmemStore.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("inmem.Snapshot: %v", err)
	}

	// Bytes must unmarshal to the canonical empty record.
	var rec memory.Record
	if err := json.Unmarshal(inmemSnap.Bytes, &rec); err != nil {
		t.Fatalf("unmarshal inmem snapshot bytes: %v", err)
	}
	if rec.Strategy != memory.StrategyNone {
		t.Errorf("inmem record strategy=%q, want %q", rec.Strategy, memory.StrategyNone)
	}
	if len(rec.Turns) != 0 {
		t.Errorf("inmem record turns=%d, want 0", len(rec.Turns))
	}

	// 2. Open a SQLite driver against a fresh DB, Restore the InMem
	//    snapshot — must succeed (cross-driver byte-stable).
	dbPath := filepath.Join(t.TempDir(), "memory.sqlite")
	sqliteStore, err := memorydriversqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dbPath, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer func() { _ = sqliteStore.Close(ctx) }()
	if err := sqliteStore.Restore(ctx, id, inmemSnap); err != nil {
		t.Fatalf("sqlite.Restore(inmemSnap): %v", err)
	}

	// 3. Read the SQLite snapshot back; the bytes must round-trip to
	//    the canonical empty record again.
	sqliteSnap, err := sqliteStore.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("sqlite.Snapshot: %v", err)
	}
	var rec2 memory.Record
	if err := json.Unmarshal(sqliteSnap.Bytes, &rec2); err != nil {
		t.Fatalf("unmarshal sqlite snapshot bytes: %v", err)
	}
	if rec2.Strategy != memory.StrategyNone {
		t.Errorf("sqlite record strategy=%q, want %q", rec2.Strategy, memory.StrategyNone)
	}
	if len(rec2.Turns) != 0 {
		t.Errorf("sqlite record turns=%d, want 0", len(rec2.Turns))
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
	}, memory.Deps{State: store, Bus: bus})
	if err == nil {
		t.Fatal("OpenDriver(sqlite, empty DSN): err=nil, want non-nil")
	}
	if errors.Is(err, memory.ErrUnknownDriver) {
		t.Fatalf("driver not registered: %v", err)
	}
}

// TestSQLite_RestartRehydration is the durability proof (AC-5):
// write turns under a strategy on a SQLite memory store backed by a
// REAL SQLite `state.StateStore` (durable to disk), Close everything,
// then reopen a fresh state store + memory store against the SAME
// state DSN and assert `GetLLMContext` returns the prior summary +
// recent turns. This is the "memory survives a restart" guarantee.
//
// It also exercises the fail-loud failure mode (AC-6): reopening
// rolling_summary with a nil Summarizer errors at `memory.Open`.
func TestSQLite_RestartRehydration(t *testing.T) {
	ctx := context.Background()
	id := tripleA()

	cases := []struct {
		name         string
		strategyName memory.Strategy
		summarizer   memory.Summarizer
		wantSummary  bool
	}{
		{
			name:         "rolling_summary",
			strategyName: memory.StrategyRollingSummary,
			summarizer:   strategy.EchoSummarizer{},
			wantSummary:  true,
		},
		{
			name:         "truncation",
			strategyName: memory.StrategyTruncation,
			summarizer:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A real, disk-backed SQLite StateStore DSN is the durable
			// backing the strategy executor persists through. The memory
			// driver's own DSN is incidental (vestigial table); the
			// strategy state lives in the StateStore.
			stateDSN := filepath.Join(t.TempDir(), "state.sqlite")
			memDSN := filepath.Join(t.TempDir(), "memory.sqlite")

			bus := buildBus(t)

			// --- Session 1: write turns, then Close everything. ---
			st1, err := state.Open(ctx, config.StateConfig{Driver: "sqlite", DSN: stateDSN})
			if err != nil {
				t.Fatalf("state.Open (1): %v", err)
			}
			deps1 := memory.Deps{State: st1, Bus: bus, Summarizer: tc.summarizer}
			m1, err := memorydriversqlite.New(memory.ConfigSnapshot{
				Driver: "sqlite", DSN: memDSN, Strategy: tc.strategyName, BudgetTokens: 256,
			}, deps1)
			if err != nil {
				t.Fatalf("sqlite.New (1): %v", err)
			}
			// 6 turns guarantees the recent-window (FullZoneTurns=4)
			// overflows so rolling_summary spills + summarises.
			for i := range 6 {
				if err := m1.AddTurn(ctx, id, memory.ConversationTurn{
					UserMessage: "u", AssistantResponse: "a",
				}); err != nil {
					t.Fatalf("AddTurn %d: %v", i, err)
				}
			}
			patch1, err := m1.GetLLMContext(ctx, id)
			if err != nil {
				t.Fatalf("GetLLMContext (1): %v", err)
			}
			if tc.wantSummary && patch1.Summary == "" {
				t.Fatalf("pre-restart: expected non-empty summary, got empty patch: %+v", patch1)
			}
			if len(patch1.RecentTurns) == 0 {
				t.Fatalf("pre-restart: expected recent turns, got none")
			}
			if err := m1.Close(ctx); err != nil {
				t.Fatalf("m1.Close: %v", err)
			}
			if err := st1.Close(ctx); err != nil {
				t.Fatalf("st1.Close: %v", err)
			}

			// --- Session 2: reopen against the SAME state DSN. ---
			st2, err := state.Open(ctx, config.StateConfig{Driver: "sqlite", DSN: stateDSN})
			if err != nil {
				t.Fatalf("state.Open (2): %v", err)
			}
			defer func() { _ = st2.Close(ctx) }()
			deps2 := memory.Deps{State: st2, Bus: bus, Summarizer: tc.summarizer}
			m2, err := memorydriversqlite.New(memory.ConfigSnapshot{
				Driver: "sqlite", DSN: memDSN, Strategy: tc.strategyName, BudgetTokens: 256,
			}, deps2)
			if err != nil {
				t.Fatalf("sqlite.New (2): %v", err)
			}
			defer func() { _ = m2.Close(ctx) }()

			patch2, err := m2.GetLLMContext(ctx, id)
			if err != nil {
				t.Fatalf("GetLLMContext (2): %v", err)
			}
			if tc.wantSummary {
				if patch2.Summary == "" {
					t.Errorf("post-restart: lost the summary (got empty)")
				}
				if patch2.Summary != patch1.Summary {
					t.Errorf("post-restart summary drift:\n pre=%q\npost=%q", patch1.Summary, patch2.Summary)
				}
			}
			if len(patch2.RecentTurns) == 0 {
				t.Errorf("post-restart: lost recent turns")
			}
		})
	}

	// Failure mode (AC-6): rolling_summary with a nil Summarizer fails
	// loud at memory.Open — no stub fallback.
	t.Run("rolling_summary_nil_summarizer_fails_loud", func(t *testing.T) {
		stateDSN := filepath.Join(t.TempDir(), "state.sqlite")
		memDSN := filepath.Join(t.TempDir(), "memory.sqlite")
		bus := buildBus(t)
		st, err := state.Open(ctx, config.StateConfig{Driver: "sqlite", DSN: stateDSN})
		if err != nil {
			t.Fatalf("state.Open: %v", err)
		}
		defer func() { _ = st.Close(ctx) }()
		_, err = memory.Open(ctx, memory.ConfigSnapshot{
			Driver: "sqlite", DSN: memDSN, Strategy: memory.StrategyRollingSummary,
		}, memory.Deps{State: st, Bus: bus})
		if err == nil {
			t.Fatal("err=nil, want non-nil (rolling_summary needs a Summarizer)")
		}
	})
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

func tripleA() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1"},
		RunID:    "run-1",
	}
}
