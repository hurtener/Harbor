package sqlmigrate_test

// Tests for the shared migration runner. The SQLite path uses synthetic
// fstest.MapFS migration sets against an in-memory modernc.org/sqlite DB,
// so they exercise the runner contract (ordering, idempotency,
// loud-on-malformed, partial-apply recovery) without depending on any
// driver's real migrations/. The Postgres path is env-gated
// (HARBOR_TEST_PG_DSN) and otherwise skipped — the per-driver Postgres
// migration tests are the in-CI gate for RunPostgres.

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

func mig(body string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(body)} }

func openMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1) // pin one conn so :memory: state persists across the run
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func appliedVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func TestRunSQLite_AppliesAndIsIdempotent(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_init.sql": mig(`CREATE TABLE IF NOT EXISTS a(id INTEGER PRIMARY KEY);`),
		"migrations/0002_more.sql": mig(`CREATE TABLE IF NOT EXISTS b(id INTEGER PRIMARY KEY);`),
	}
	db := openMem(t)
	ctx := context.Background()

	if err := sqlmigrate.RunSQLite(ctx, db, fsys, "test"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if got := appliedVersions(t, db); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("applied versions = %v, want [1 2]", got)
	}
	// Both tables exist.
	for _, tbl := range []string{"a", "b"} {
		if _, err := db.Exec(`INSERT INTO ` + tbl + `(id) VALUES (1)`); err != nil {
			t.Errorf("table %q not created: %v", tbl, err)
		}
	}
	// Second run is a clean no-op.
	if err := sqlmigrate.RunSQLite(ctx, db, fsys, "test"); err != nil {
		t.Fatalf("second (idempotent) run: %v", err)
	}
	if got := appliedVersions(t, db); len(got) != 2 {
		t.Errorf("after idempotent re-run, versions = %v, want 2 entries", got)
	}
}

func TestRunSQLite_AppliesInVersionOrder(t *testing.T) {
	// 0002 depends on the table 0001 creates; map iteration is random, so a
	// clean apply proves the runner sorts ascending before applying.
	fsys := fstest.MapFS{
		"migrations/0002_use.sql":  mig(`INSERT INTO base(id) VALUES (1);`),
		"migrations/0001_base.sql": mig(`CREATE TABLE base(id INTEGER PRIMARY KEY);`),
	}
	db := openMem(t)
	if err := sqlmigrate.RunSQLite(context.Background(), db, fsys, "test"); err != nil {
		t.Fatalf("ordered apply failed (runner did not sort by version): %v", err)
	}
}

func TestRunSQLite_MalformedFilename_FailsLoud(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_ok.sql": mig(`CREATE TABLE x(id INTEGER PRIMARY KEY);`),
		"migrations/oops.sql":    mig(`CREATE TABLE y(id INTEGER PRIMARY KEY);`),
	}
	db := openMem(t)
	err := sqlmigrate.RunSQLite(context.Background(), db, fsys, "test")
	if err == nil {
		t.Fatal("expected a loud error on a malformed migration filename, got nil")
	}
	if !strings.Contains(err.Error(), "does not match NNNN_<slug>.sql") {
		t.Errorf("error %q does not name the filename contract", err)
	}
}

func TestRunSQLite_PartialApplyRecovery(t *testing.T) {
	// Simulate a partially-applied DB: the DDL landed but the version row
	// was lost (e.g. a crash between the body and the bookkeeping INSERT in
	// an older runner). The precheck + idempotent body must recover.
	fsys := fstest.MapFS{
		"migrations/0001_init.sql": mig(`CREATE TABLE IF NOT EXISTS a(id INTEGER PRIMARY KEY);`),
	}
	db := openMem(t)
	ctx := context.Background()
	if err := sqlmigrate.RunSQLite(ctx, db, fsys, "test"); err != nil {
		t.Fatalf("initial run: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = 1`); err != nil {
		t.Fatalf("simulate lost version row: %v", err)
	}
	// Re-run must recover without error (CREATE TABLE IF NOT EXISTS +
	// INSERT OR IGNORE are idempotent) and re-record the version.
	if err := sqlmigrate.RunSQLite(ctx, db, fsys, "test"); err != nil {
		t.Fatalf("recovery run: %v", err)
	}
	if got := appliedVersions(t, db); len(got) != 1 || got[0] != 1 {
		t.Errorf("after recovery, versions = %v, want [1]", got)
	}
}

// TestRunPostgres_ConcurrentBootsSerialize exercises the advisory-lock
// path: N concurrent RunPostgres calls against the same DB must all
// succeed without racing on CREATE TABLE / version inserts. Env-gated —
// skipped unless HARBOR_TEST_PG_DSN names a reachable Postgres.
func TestRunPostgres_ConcurrentBootsSerialize(t *testing.T) {
	dsn := os.Getenv("HARBOR_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("HARBOR_TEST_PG_DSN not set; per-driver postgres migration tests are the in-CI gate")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fsys := fstest.MapFS{
		"migrations/0001_init.sql": mig(`CREATE TABLE IF NOT EXISTS sqlmigrate_test_a(id INTEGER PRIMARY KEY);
			INSERT INTO schema_migrations(version) VALUES (1) ON CONFLICT DO NOTHING;`),
	}
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- sqlmigrate.RunPostgres(context.Background(), db, fsys, "test", "harbor-sqlmigrate-test")
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Errorf("concurrent RunPostgres failed (advisory lock did not serialise): %v", e)
		}
	}
}
