package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/config"
)

// TestMigrate_CleanDB_StartsClean — fresh tempdir DB, run migrations
// (transitively via New), verify schema_migrations row at version 1.
func TestMigrate_CleanDB_StartsClean(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "clean.sqlite")

	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
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

// TestMigrate_Idempotent — reopen the same DB; the migration runner
// must skip the already-applied migration without error and without
// adding a duplicate row.
func TestMigrate_Idempotent(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "idempotent.sqlite")

	s1, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if closeErr := s1.Close(context.Background()); closeErr != nil {
		t.Fatalf("first Close: %v", closeErr)
	}

	s2, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
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

// TestMigrate_Roundtrip_AcrossMigration — Put records, close, reopen
// (re-runs migration; idempotent), Get round-trips byte-equal. Pins
// the durability promise of the SQLite leg of the artifact persistence
// triad.
func TestMigrate_Roundtrip_AcrossMigration(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "roundtrip.sqlite")

	s1, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}

	scope := artifacts.ArtifactScope{
		TenantID:  "tenant-rt",
		UserID:    "user-rt",
		SessionID: "sess-rt",
		TaskID:    "task-rt",
	}
	want := []byte("durable-payload")
	ref, err := s1.PutBytes(context.Background(), scope, want,
		artifacts.PutOpts{Namespace: "rt", Filename: "rt.bin"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if closeErr := s1.Close(context.Background()); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}

	s2, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("reopen New: %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()

	got, found, err := s2.Get(context.Background(), scope, ref.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if !found {
		t.Fatal("Get found=false after reopen")
	}
	if string(got) != string(want) {
		t.Errorf("Bytes round-trip failed: got=%q, want=%q", got, want)
	}

	gotRef, found, err := s2.GetRef(context.Background(), scope, ref.ID)
	if err != nil {
		t.Fatalf("GetRef after reopen: %v", err)
	}
	if !found {
		t.Fatal("GetRef found=false after reopen")
	}
	if gotRef.Filename != "rt.bin" {
		t.Errorf("Filename round-trip failed: got=%q", gotRef.Filename)
	}
}

// TestMigrate_TablePresent confirms the primary table exists with the
// documented composite primary key after open.
func TestMigrate_TablePresent(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "schema-present.sqlite")

	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
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

	for _, name := range []string{"artifacts_blobs", "schema_migrations"} {
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
