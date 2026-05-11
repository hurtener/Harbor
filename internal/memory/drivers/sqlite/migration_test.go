package sqlite_test

// Migration-runner tests for the SQLite MemoryStore driver. The
// migration code is reachable indirectly through `New` (which calls
// `migrate` after `sql.Open`), so these tests exercise:
//
//   - A clean DB applies migrations end-to-end.
//   - A second `New` against the same DB is an idempotent no-op
//     (forward-only contract — re-running the migration runner on an
//     already-migrated DB MUST NOT error).
//   - The schema_migrations table records the applied version.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
)

// TestSQLite_Migrations_AppliedOnFreshDB asserts a clean DB has the
// `memory_state` table + a row in `schema_migrations` after New.
func TestSQLite_Migrations_AppliedOnFreshDB(t *testing.T) {
	bus, store := buildDeps(t)
	dsn := filepath.Join(t.TempDir(), "fresh.sqlite")

	m, err := sqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer func() { _ = m.Close(context.Background()) }()

	// Verify the schema_migrations row landed by opening a side-band
	// connection (we don't expose the *sql.DB through the driver
	// surface, so we round-trip through database/sql directly).
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("side-band sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_migrations WHERE version = 1`).Scan(&version); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if version != 1 {
		t.Errorf("schema_migrations.version=%d, want 1", version)
	}

	// Confirm memory_state table exists by issuing a count.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_state`).Scan(&count); err != nil {
		t.Fatalf("count memory_state: %v", err)
	}
	if count != 0 {
		t.Errorf("memory_state row count=%d, want 0 (fresh DB)", count)
	}
}

// TestSQLite_Migrations_IdempotentOnReopen asserts a second New
// against the same DB file does not re-apply any migration (the
// forward-only contract).
func TestSQLite_Migrations_IdempotentOnReopen(t *testing.T) {
	bus, store := buildDeps(t)
	dsn := filepath.Join(t.TempDir(), "reopen.sqlite")

	m1, err := sqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if err := m1.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	m2, err := sqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("second New (idempotent expected): %v", err)
	}
	defer func() { _ = m2.Close(context.Background()) }()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("side-band sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rows != 1 {
		t.Errorf("schema_migrations row count after second New=%d, want 1", rows)
	}
}
