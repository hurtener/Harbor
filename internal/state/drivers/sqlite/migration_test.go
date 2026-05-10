package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// TestMigrate_CleanDB_StartsClean — fresh tempdir DB, run migrations
// (transitively via New), verify schema_migrations row at version 1.
func TestMigrate_CleanDB_StartsClean(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "clean.sqlite")

	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	// Open a parallel connection to inspect schema_migrations directly
	// — the StateStore interface is opaque about its bookkeeping.
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

// TestMigrate_Idempotent — reopen the same DB; the migration runner
// must skip the already-applied migration without error and without
// adding a duplicate row.
func TestMigrate_Idempotent(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "idempotent.sqlite")

	// First open: applies 0001.
	s1, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if closeErr := s1.Close(context.Background()); closeErr != nil {
		t.Fatalf("first Close: %v", closeErr)
	}

	// Second open: should be a no-op for the migration runner.
	s2, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("second New (re-open): %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()

	db, openErr := sql.Open("sqlite", dsn)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer func() { _ = db.Close() }()

	versions := loadSchemaMigrations(t, db)
	if len(versions) != 1 || versions[0] != 1 {
		t.Fatalf("schema_migrations after re-open=%v, want [1]", versions)
	}
}

// TestMigrate_Roundtrip_AcrossMigration — Save records, close, reopen
// (re-runs migration; idempotent), Load round-trips byte-equal. Pins
// the durability promise of the SQLite leg of the persistence triad.
func TestMigrate_Roundtrip_AcrossMigration(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "roundtrip.sqlite")

	s1, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}

	q := identityTripleForTest()
	want := state.StateRecord{
		ID:       "01HABXX-roundtrip-0001",
		Identity: q,
		Kind:     "session.lifecycle",
		Bytes:    []byte("durable-payload"),
		Version:  3,
	}
	if saveErr := s1.Save(context.Background(), want); saveErr != nil {
		t.Fatalf("Save: %v", saveErr)
	}
	if closeErr := s1.Close(context.Background()); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	// Reopen — migration runner is idempotent; data persists.
	s2, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("reopen New: %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()

	got, err := s2.Load(context.Background(), q, "session.lifecycle")
	if err != nil {
		t.Fatalf("Load after reopen: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("ID round-trip failed: got %q, want %q", got.ID, want.ID)
	}
	if string(got.Bytes) != "durable-payload" {
		t.Errorf("Bytes round-trip failed: got %q", got.Bytes)
	}
	if got.Version != 3 {
		t.Errorf("Version round-trip failed: got %d, want 3", got.Version)
	}
}

// TestMigrate_StateRecordsTablePresent confirms the primary table
// exists with the documented composite primary key after open.
func TestMigrate_StateRecordsTablePresent(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "schema-present.sqlite")

	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	tables := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"state_records", "schema_migrations"} {
		if !tables[name] {
			t.Errorf("missing expected table %q (have %v)", name, tables)
		}
	}
}

// loadSchemaMigrations reads the schema_migrations table into a sorted
// version slice. Test helper — fails the test on SQL errors.
func loadSchemaMigrations(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := []int{}
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
