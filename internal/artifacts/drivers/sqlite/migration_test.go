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
// (transitively via New), verify both forward-only migrations recorded.
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
	if !equalVersions(versions, []int{1, 2}) {
		t.Fatalf("schema_migrations=%v, want [1 2]", versions)
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
	if !equalVersions(versions, []int{1, 2}) {
		t.Fatalf("schema_migrations after re-open=%v, want [1 2]", versions)
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

// TestMigrate_LegacyTaskKeyedRows_CollapseOntoTheTriple builds a
// database in the PRE-RECONCILIATION shape by hand — the four-field
// primary key `(tenant, user, session, task, namespace, id)` with two
// rows that differ only in `task` — and then opens it through the
// driver, which runs migration 0002.
//
// This is the only way to exercise the collapse: after the migration the
// primary key forbids the duplicate, so no sequence of interface calls
// can construct the input. A migration whose data path is never executed
// is a migration nobody has tested.
func TestMigrate_LegacyTaskKeyedRows_CollapseOntoTheTriple(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "legacy.sqlite")

	seed, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// The v1 schema verbatim, plus its schema_migrations row, so the
	// runner sees a database that has had 0001 and not 0002.
	const v1Schema = `
        CREATE TABLE artifacts_blobs (
            tenant      TEXT      NOT NULL,
            user        TEXT      NOT NULL,
            session     TEXT      NOT NULL,
            task        TEXT      NOT NULL,
            namespace   TEXT      NOT NULL,
            id          TEXT      NOT NULL,
            mime_type   TEXT      NOT NULL,
            size_bytes  INTEGER   NOT NULL,
            filename    TEXT      NOT NULL,
            sha256      TEXT      NOT NULL,
            source_json BLOB      NOT NULL,
            bytes       BLOB      NOT NULL,
            PRIMARY KEY (tenant, user, session, task, namespace, id)
        );
        CREATE TABLE schema_migrations (
            version    INTEGER   PRIMARY KEY,
            applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );
        INSERT INTO schema_migrations(version) VALUES (1);`
	if _, err := seed.Exec(v1Schema); err != nil {
		t.Fatalf("seed v1 schema: %v", err)
	}

	const payload = "identical bytes, two legacy rows"
	const id = "ns_0123456789ab"
	const insert = `
        INSERT INTO artifacts_blobs
            (tenant, user, session, task, namespace, id,
             mime_type, size_bytes, filename, sha256, source_json, bytes)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	// Deliberately inserted LATEST-task-first, so a collapse rule that
	// silently depended on physical row order would pick the wrong row.
	for _, task := range []string{"run-zulu", "run-alpha"} {
		if _, err := seed.Exec(insert,
			"T", "U", "S", task, "ns", id,
			"application/json", int64(len(payload)), "legacy.json",
			"deadbeef", []byte("null"), []byte(payload),
		); err != nil {
			t.Fatalf("seed row task=%s: %v", task, err)
		}
	}
	// A row in a DIFFERENT session with the same id must survive
	// untouched — the collapse is scoped to one triple.
	if _, err := seed.Exec(insert,
		"T", "U", "other-session", "run-zulu", "ns", id,
		"application/json", int64(len(payload)), "legacy.json",
		"deadbeef", []byte("null"), []byte(payload),
	); err != nil {
		t.Fatalf("seed sibling-session row: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed handle: %v", err)
	}

	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("New over a legacy DB (migration 0002 failed): %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	triple := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	rows, err := s.List(context.Background(), triple)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("legacy duplicates did not collapse: %d rows, want 1", len(rows))
	}
	if rows[0].Scope.TaskID != "run-alpha" {
		t.Errorf("survivor stamp=%q, want the smallest task %q",
			rows[0].Scope.TaskID, "run-alpha")
	}

	// The survivor is readable on the triple with any task stamp.
	got, found, err := s.Get(context.Background(), triple, id)
	if err != nil || !found {
		t.Fatalf("Get on the migrated row: found=%v err=%v", found, err)
	}
	if string(got) != payload {
		t.Errorf("migrated bytes=%q, want %q", got, payload)
	}

	// The sibling session kept its own copy.
	otherRows, err := s.List(context.Background(),
		artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "other-session"})
	if err != nil {
		t.Fatalf("List sibling session: %v", err)
	}
	if len(otherRows) != 1 {
		t.Errorf("sibling session lost its row: %d, want 1", len(otherRows))
	}
}

// equalVersions compares two ascending version slices.
func equalVersions(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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
