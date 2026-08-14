package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
)

// The migration tests pin the driver's forward-only embedded-migration
// contract: a clean DB starts clean, a re-open is an idempotent no-op,
// data round-trips across the migration runner, and the documented
// tables exist after open.

func TestMigrate_CleanDB_StartsClean(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "clean.sqlite")
	s, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	versions := loadSchemaMigrations(t, db)
	if len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("schema_migrations=%v, want [1]", versions)
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "idempotent.sqlite")
	s1, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if err := s1.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	s2, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("second New (re-open): %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if versions := loadSchemaMigrations(t, db); len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("schema_migrations after re-open=%v, want [1]", versions)
	}
}

func TestMigrate_Roundtrip_AcrossMigration(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "roundtrip.sqlite")
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}

	s1, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	want := richRow("run-1", id, 1)
	if _, err := s1.AppendTurnIf(context.Background(), id, want); err != nil {
		t.Fatalf("AppendTurnIf: %v", err)
	}
	if err := s1.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("reopen New: %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()
	got, err := s2.GetTurn(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("GetTurn after reopen: %v", err)
	}
	if got.Sequence != 1 || got.Version != 1 || got.Query.Text != want.Query.Text {
		t.Errorf("row did not round-trip across migration: %+v", got)
	}
}

func TestMigrate_TablesPresent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "schema-present.sqlite")
	s, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()
	tables := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		tables[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	for _, name := range []string{
		"turn_rows", "turn_activity_rows", "turn_apps",
		"turn_session_state", "turn_fences", "turn_snapshot_gens",
		"schema_migrations",
	} {
		if !tables[name] {
			t.Errorf("missing expected table %q (have %v)", name, tables)
		}
	}
}

// TestMigrate_FenceAndSnapshotSurviveReopen pins the two DURABLE
// tables the erasure contract rides on: the fence (tombstone) and the
// projection snapshot generation both survive a close + reopen.
func TestMigrate_FenceAndSnapshotSurviveReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "fence.sqlite")
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}
	ctx := context.Background()

	s1, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, err := s1.AppendTurnIf(ctx, id, richRow("run-1", id, 1)); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s1.FenceSession(ctx, id); err != nil {
		t.Fatalf("fence: %v", err)
	}
	if n, err := s1.DeleteScope(ctx, id); err != nil || n < 1 {
		t.Fatalf("DeleteScope = (%d, %v)", n, err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()
	if _, err := s2.AppendTurnIf(ctx, id, richRow("run-2", id, 2)); !errors.Is(err, turns.ErrErasureFenced) {
		t.Errorf("post-reopen append error=%v, want ErrErasureFenced", err)
	}
}

func loadSchemaMigrations(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}
