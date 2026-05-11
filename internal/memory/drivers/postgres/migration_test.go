package postgres_test

// Migration-runner tests for the Postgres MemoryStore driver. The
// migration code is reachable indirectly through `New` (which calls
// `applyMigrations` after Ping), so these tests exercise:
//
//   - A clean schema applies migrations end-to-end.
//   - A second `New` against the same schema is an idempotent no-op
//     (forward-only contract).
//   - The schema_migrations table records the applied version.

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/memory"
	memorydriverpostgres "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
)

// TestPostgres_Migrations_AppliedOnFreshSchema asserts a clean schema
// has the `memory_state` table + a row in `schema_migrations` after
// New.
func TestPostgres_Migrations_AppliedOnFreshSchema(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	bus, store := buildDeps(t)

	m, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = m.Close(context.Background()) }()

	db, err := sql.Open("pgx", dsn)
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

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_state`).Scan(&count); err != nil {
		t.Fatalf("count memory_state: %v", err)
	}
	if count != 0 {
		t.Errorf("memory_state row count=%d, want 0 (fresh schema)", count)
	}
}

// TestPostgres_Migrations_IdempotentOnReopen asserts a second New
// against the same schema does not re-apply any migration.
func TestPostgres_Migrations_IdempotentOnReopen(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	bus, store := buildDeps(t)

	m1, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if err := m1.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	m2, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("second New (idempotent expected): %v", err)
	}
	defer func() { _ = m2.Close(context.Background()) }()

	db, err := sql.Open("pgx", dsn)
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
