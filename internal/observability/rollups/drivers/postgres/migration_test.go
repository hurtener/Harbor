package postgres_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hurtener/Harbor/internal/observability/rollups/drivers/postgres"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

// TestPostgres_Migrations_Idempotent pins the forward-only migration
// contract on the driver: a first boot creates the rollup tables and
// records schema_migrations version 1, a second boot (a restart, or a
// concurrent New() on another replica) is a no-op that applies nothing
// again, and the tables exist under the expected names.
func TestPostgres_Migrations_Idempotent(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	ctx := context.Background()

	s1, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Second boot: migrations are already applied — must succeed as a
	// no-op (multi-replica boots race on the advisory lock, not the DDL).
	s2, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("admin sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Exactly one applied version.
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("schema_migrations query: %v", err)
	}
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate versions: %v", err)
	}
	if len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("applied migrations = %v; want exactly [1]", versions)
	}

	// The rollup tables exist.
	for _, tbl := range []string{"rollup_rows", "rollup_checkpoint", "rollup_fence"} {
		var reg sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT to_regclass($1)`, tbl).Scan(&reg); err != nil {
			t.Fatalf("to_regclass(%s): %v", tbl, err)
		}
		if !reg.Valid {
			t.Fatalf("table %s does not exist after migrations", tbl)
		}
	}
}

func TestPostgres_Migrations_VerifyMode_AfterApply(t *testing.T) {
	dsn := freshSchema(t, requireDSN(t))
	ctx := context.Background()
	applyStore, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	_ = applyStore.Close(ctx)
	verifyStore, err := postgres.New(postgres.Config{DSN: dsn, MigrationMode: sqlmigrate.ModeVerify})
	if err != nil {
		t.Fatalf("verify applied ledger: %v", err)
	}
	_ = verifyStore.Close(ctx)
}

// TestPostgres_Migrations_SeededCheckpoint pins that the migration seeds
// the single-row checkpoint at sequence 0, so a fresh store reports a
// zero checkpoint and the first advancing batch has a serialization point
// to lock.
func TestPostgres_Migrations_SeededCheckpoint(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	ctx := context.Background()

	s, err := postgres.New(postgres.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s.Close(ctx) }()

	if ck, err := s.Checkpoint(ctx); err != nil || ck != 0 {
		t.Fatalf("fresh checkpoint = %d, %v; want 0", ck, err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("admin sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM rollup_checkpoint WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("count checkpoint rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("checkpoint rows = %d; want exactly 1 (the single-row id = 1 serialization point)", n)
	}
}
