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
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestSQLite_ConformanceSuite invokes the canonical conformance
// suite against the SQLite memory driver. Each subtest gets its own
// fresh DB file under t.TempDir so cross-subtest state cannot bleed.
func TestSQLite_ConformanceSuite(t *testing.T) {
	conformancetest.Run(t, func() conformancetest.Harness {
		bus, store := buildDeps(t)
		dbPath := filepath.Join(t.TempDir(), "memory.sqlite")
		mem, err := memorydriversqlite.New(memory.ConfigSnapshot{
			Driver:   "sqlite",
			DSN:      dbPath,
			Strategy: memory.StrategyNone,
		}, memory.Deps{State: store, Bus: bus})
		if err != nil {
			t.Fatalf("sqlite.New: %v", err)
		}
		return conformancetest.Harness{
			Store: mem,
			Bus:   bus,
			Cleanup: func() {
				_ = mem.Close(context.Background())
			},
		}
	})
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

// TestSQLite_New_RejectsTruncationStrategy pins the
// `ErrStrategyNotImplemented` guard until Phase 24 widens.
func TestSQLite_New_RejectsTruncationStrategy(t *testing.T) {
	bus, store := buildDeps(t)
	dbPath := filepath.Join(t.TempDir(), "memory.sqlite")
	_, err := memorydriversqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dbPath, Strategy: memory.StrategyTruncation,
	}, memory.Deps{State: store, Bus: bus})
	if !errors.Is(err, memory.ErrStrategyNotImplemented) {
		t.Fatalf("err=%v, want errors.Is ErrStrategyNotImplemented", err)
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
	}, memory.Deps{State: store, Bus: bus})
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
