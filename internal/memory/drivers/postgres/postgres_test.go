package postgres_test

// Driver-level tests for the Postgres MemoryStore. The behavioural
// surface is covered by the shared conformance suite; this file
// invokes that suite + adds driver-specific cases the suite cannot
// express (construction errors, byte-stable round-trip vs the InMem
// reference).
//
// Skip-clean without HARBOR_PG_DSN: every Postgres-touching test
// uses `requireDSN(t)` which `t.Skip`s when the env var is unset.
// CI's memory-postgres job sets HARBOR_PG_DSN against the postgres:16
// service container so the suite actually exercises the driver
// there.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/conformancetest"
	memorydriverinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	memorydriverpostgres "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

const (
	pgDSNEnv  = "HARBOR_PG_DSN"
	skipNoDSN = "HARBOR_PG_DSN not set; skipping postgres conformance — see docs/plans/phase-25-memory-drivers.md"
)

// requireDSN returns the DSN from the environment or skips the test
// cleanly. CI's memory-postgres job sets the var; local dev without
// Postgres trips a Skip.
func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skip(skipNoDSN)
	}
	return dsn
}

// freshSchema creates a per-test Postgres schema, returns a DSN that
// pins `search_path` to it, and registers a t.Cleanup that drops the
// schema. Mirrors the state-postgres test helper so test isolation
// is consistent across persistence-triad subsystems.
func freshSchema(t *testing.T, baseDSN string) string {
	t.Helper()
	suffix := randSuffix(t)
	schema := "harbor_memtest_" + suffix

	adminDB, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("admin sql.Open: %v", err)
	}
	defer func() { _ = adminDB.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s", quoteIdent(schema)),
	); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		dropDB, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Logf("cleanup sql.Open: %v", err)
			return
		}
		defer func() { _ = dropDB.Close() }()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
		defer dropCancel()
		if _, err := dropDB.ExecContext(dropCtx,
			fmt.Sprintf("DROP SCHEMA %s CASCADE", quoteIdent(schema)),
		); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	return appendSearchPath(baseDSN, schema)
}

// randSuffix returns a 16-hex-char random suffix for schema names.
func randSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// quoteIdent quotes a SQL identifier (schema name).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// appendSearchPath returns dsn with `search_path` set to schema.
func appendSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn + " search_path=" + schema
		}
		q := u.Query()
		opts := q.Get("options")
		add := "-c search_path=" + schema
		if opts == "" {
			q.Set("options", add)
		} else {
			q.Set("options", opts+" "+add)
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " options='-c search_path=" + schema + "'"
}

// TestPostgres_ConformanceSuite invokes the canonical conformance
// suite against a Postgres connection. Each subtest gets its own
// fresh driver + TRUNCATE between cases so state cannot bleed.
func TestPostgres_ConformanceSuite(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	conformancetest.Run(t, func() conformancetest.Harness {
		bus, store := buildDeps(t)
		m, err := memorydriverpostgres.New(memory.ConfigSnapshot{
			Driver: "postgres", DSN: dsn, Strategy: memory.StrategyNone,
		}, memory.Deps{State: store, Bus: bus})
		if err != nil {
			t.Fatalf("postgres.New: %v", err)
		}
		truncateAll(t, dsn)
		return conformancetest.Harness{
			Store: m,
			Bus:   bus,
			Cleanup: func() {
				_ = m.Close(context.Background())
			},
		}
	})
}

// truncateAll wipes the memory_state table between conformance
// subtests so each subtest sees a clean slate.
func truncateAll(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("truncateAll sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if _, err := db.ExecContext(ctx, "TRUNCATE TABLE memory_state"); err != nil {
		// The table is created by migrations on the first New within
		// the schema; ignore "does not exist" the very first time.
		if !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("truncate memory_state: %v", err)
		}
	}
}

// TestPostgres_DriverRegistered verifies the init() side-effect.
func TestPostgres_DriverRegistered(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := memory.OpenDriver("postgres", memory.ConfigSnapshot{
		Driver: "postgres", Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err == nil {
		t.Fatal("OpenDriver(postgres, empty DSN): err=nil, want non-nil")
	}
	if errors.Is(err, memory.ErrUnknownDriver) {
		t.Fatalf("driver not registered: %v", err)
	}
}

// TestPostgres_New_RequiresDSN pins the explicit-DSN-required
// contract.
func TestPostgres_New_RequiresDSN(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("error should mention DSN; got: %v", err)
	}
}

// TestPostgres_New_RequiresBus checks the fail-loud bus dep guard.
func TestPostgres_New_RequiresBus(t *testing.T) {
	_, store := buildDeps(t)
	_, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", DSN: "postgres://x", Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: nil})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

// TestPostgres_New_RejectsTruncationStrategy pins the
// `ErrStrategyNotImplemented` guard.
func TestPostgres_New_RejectsTruncationStrategy(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", DSN: "postgres://x", Strategy: memory.StrategyTruncation,
	}, memory.Deps{State: store, Bus: bus})
	if !errors.Is(err, memory.ErrStrategyNotImplemented) {
		t.Fatalf("err=%v, want errors.Is ErrStrategyNotImplemented", err)
	}
}

// TestPostgres_PersistsAcrossReopens proves the driver actually
// persists state across driver instances.
func TestPostgres_PersistsAcrossReopens(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	bus, store := buildDeps(t)
	ctx := context.Background()
	id := tripleA()

	m1, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New (1): %v", err)
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

	m2, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New (2): %v", err)
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

// TestPostgres_CrossDriver_ByteStableRoundTrip asserts the Phase 25
// acceptance criterion: an InMem snapshot must restore byte-stably
// into the Postgres driver and re-read as the same canonical record.
func TestPostgres_CrossDriver_ByteStableRoundTrip(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	bus, store := buildDeps(t)
	ctx := context.Background()
	id := tripleA()

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

	var rec memory.Record
	if err := json.Unmarshal(inmemSnap.Bytes, &rec); err != nil {
		t.Fatalf("unmarshal inmem snapshot bytes: %v", err)
	}
	if rec.Strategy != memory.StrategyNone {
		t.Errorf("inmem record strategy=%q, want %q", rec.Strategy, memory.StrategyNone)
	}

	pgStore, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = pgStore.Close(ctx) }()
	if err := pgStore.Restore(ctx, id, inmemSnap); err != nil {
		t.Fatalf("postgres.Restore(inmemSnap): %v", err)
	}
	pgSnap, err := pgStore.Snapshot(ctx, id)
	if err != nil {
		t.Fatalf("postgres.Snapshot: %v", err)
	}
	var rec2 memory.Record
	if err := json.Unmarshal(pgSnap.Bytes, &rec2); err != nil {
		t.Fatalf("unmarshal postgres snapshot bytes: %v", err)
	}
	if rec2.Strategy != memory.StrategyNone {
		t.Errorf("postgres record strategy=%q, want %q", rec2.Strategy, memory.StrategyNone)
	}
	if len(rec2.Turns) != 0 {
		t.Errorf("postgres record turns=%d, want 0", len(rec2.Turns))
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
