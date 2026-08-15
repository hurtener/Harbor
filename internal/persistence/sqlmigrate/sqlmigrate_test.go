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
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx "pgx" driver for the env-gated Postgres test
	_ "modernc.org/sqlite"

	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

func mig(body string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(body)} }

type verifyProbe struct {
	mu       sync.Mutex
	queryErr error
	versions []int
	queries  []string
	execs    []string
	prepares []string
	begins   int
}

type verifyConnector struct{ probe *verifyProbe }

func (c *verifyConnector) Connect(context.Context) (driver.Conn, error) {
	return &verifyConn{probe: c.probe}, nil
}

func (*verifyConnector) Driver() driver.Driver { return verifyDriver{} }

type verifyDriver struct{}

func (verifyDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("verifyDriver.Open is unused; tests use sql.OpenDB")
}

type verifyConn struct{ probe *verifyProbe }

func (c *verifyConn) Prepare(query string) (driver.Stmt, error) {
	c.probe.mu.Lock()
	c.probe.prepares = append(c.probe.prepares, query)
	c.probe.mu.Unlock()
	return nil, errors.New("verify probe rejects prepared statements")
}

func (*verifyConn) Close() error { return nil }

func (c *verifyConn) Begin() (driver.Tx, error) {
	c.probe.mu.Lock()
	c.probe.begins++
	c.probe.mu.Unlock()
	return nil, errors.New("verify probe rejects transactions")
}

func (c *verifyConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}

func (c *verifyConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.probe.mu.Lock()
	c.probe.execs = append(c.probe.execs, query)
	c.probe.mu.Unlock()
	return nil, errors.New("verify probe rejects writes")
}

func (c *verifyConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.probe.mu.Lock()
	c.probe.queries = append(c.probe.queries, query)
	queryErr := c.probe.queryErr
	versions := append([]int(nil), c.probe.versions...)
	c.probe.mu.Unlock()
	if queryErr != nil {
		return nil, queryErr
	}
	return &verifyRows{versions: versions}, nil
}

type verifyRows struct {
	versions []int
	index    int
}

func (*verifyRows) Columns() []string { return []string{"version"} }
func (*verifyRows) Close() error      { return nil }

func (r *verifyRows) Next(dest []driver.Value) error {
	if r.index >= len(r.versions) {
		return io.EOF
	}
	dest[0] = int64(r.versions[r.index])
	r.index++
	return nil
}

func openVerifyProbe(t *testing.T, versions ...int) (*sql.DB, *verifyProbe) {
	t.Helper()
	probe := &verifyProbe{versions: append([]int(nil), versions...)}
	db := sql.OpenDB(&verifyConnector{probe: probe})
	t.Cleanup(func() { _ = db.Close() })
	return db, probe
}

func postgresMigrations() fstest.MapFS {
	return fstest.MapFS{
		"migrations/0001_init.sql": mig(`SELECT 1;`),
		"migrations/0002_more.sql": mig(`SELECT 2;`),
	}
}

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
	// Non-idempotent bodies (no IF NOT EXISTS): the second run MUST be a
	// clean no-op via the version precheck — if the precheck didn't skip,
	// re-executing `CREATE TABLE a` would error "table a already exists".
	fsys := fstest.MapFS{
		"migrations/0001_init.sql": mig(`CREATE TABLE a(id INTEGER PRIMARY KEY);`),
		"migrations/0002_more.sql": mig(`CREATE TABLE b(id INTEGER PRIMARY KEY);`),
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
	// Version 10 (the table USER) depends on the table version 2 (the
	// CREATOR) makes. Filenames are intentionally NOT zero-padded, so
	// fs.ReadDir's name order ("10_..." before "2_...") is the REVERSE of
	// numeric order — a clean apply therefore proves the runner sorts by
	// the PARSED numeric version, not by filename. Drop the sort and "10"
	// runs first → INSERT into a non-existent table → failure.
	fsys := fstest.MapFS{
		"migrations/10_use.sql": mig(`INSERT INTO base(id) VALUES (1);`),
		"migrations/2_base.sql": mig(`CREATE TABLE base(id INTEGER PRIMARY KEY);`),
	}
	db := openMem(t)
	if err := sqlmigrate.RunSQLite(context.Background(), db, fsys, "test"); err != nil {
		t.Fatalf("ordered apply failed (runner did not sort by numeric version): %v", err)
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

func TestMode_Resolve_DefaultsToApply(t *testing.T) {
	got, err := (sqlmigrate.Mode("")).Resolve()
	if err != nil {
		t.Fatalf("Resolve empty mode: %v", err)
	}
	if got != sqlmigrate.ModeApply {
		t.Fatalf("Resolve empty mode = %q, want %q", got, sqlmigrate.ModeApply)
	}
	if _, err := sqlmigrate.Mode("future").Resolve(); err == nil {
		t.Fatal("Resolve accepted unknown mode")
	}
}

func TestRunPostgres_Verify_UsesOnlyReadOnlyLedgerQuery(t *testing.T) {
	db, probe := openVerifyProbe(t, 1, 2)
	if err := sqlmigrate.RunPostgres(context.Background(), db, postgresMigrations(), "test", "must-not-lock", sqlmigrate.ModeVerify); err != nil {
		t.Fatalf("verify: %v", err)
	}

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.queries) != 1 || strings.TrimSpace(probe.queries[0]) != "SELECT version FROM schema_migrations" {
		t.Fatalf("queries = %q, want one schema_migrations SELECT", probe.queries)
	}
	if len(probe.execs) != 0 || len(probe.prepares) != 0 || probe.begins != 0 {
		t.Fatalf("verify used write/session machinery: execs=%q prepares=%q begins=%d", probe.execs, probe.prepares, probe.begins)
	}
	if strings.Contains(strings.Join(probe.queries, " "), "pg_advisory") {
		t.Fatalf("verify queried an advisory-lock function: %q", probe.queries)
	}
}

func TestRunPostgres_Verify_FailsWhenEmbeddedVersionIsUnapplied(t *testing.T) {
	db, _ := openVerifyProbe(t, 1)
	err := sqlmigrate.RunPostgres(context.Background(), db, postgresMigrations(), "test", "must-not-lock", sqlmigrate.ModeVerify)
	if err == nil || !strings.Contains(err.Error(), "unapplied embedded migrations: 0002_more.sql") {
		t.Fatalf("verify missing version error = %v", err)
	}
}

func TestRunPostgres_Verify_FailsWhenLedgerIsMissing(t *testing.T) {
	db, probe := openVerifyProbe(t)
	probe.queryErr = errors.New(`relation "schema_migrations" does not exist`)
	err := sqlmigrate.RunPostgres(context.Background(), db, postgresMigrations(), "test", "must-not-lock", sqlmigrate.ModeVerify)
	if err == nil || !strings.Contains(err.Error(), "verify migrations") || !strings.Contains(err.Error(), "schema_migrations") {
		t.Fatalf("verify missing ledger error = %v", err)
	}
}

func TestRunPostgres_Verify_FailsBeforeQueryOnMalformedEmbeddedSet(t *testing.T) {
	cases := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{
			name: "malformed filename",
			fsys: fstest.MapFS{"migrations/not-versioned.sql": mig(`SELECT 1;`)},
			want: "malformed migration filename",
		},
		{
			name: "duplicate version",
			fsys: fstest.MapFS{
				"migrations/0001_one.sql": mig(`SELECT 1;`),
				"migrations/0001_two.sql": mig(`SELECT 2;`),
			},
			want: "duplicate migration version 0001",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, probe := openVerifyProbe(t, 1)
			err := sqlmigrate.RunPostgres(context.Background(), db, tc.fsys, "test", "must-not-lock", sqlmigrate.ModeVerify)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if len(probe.queries) != 0 {
				t.Fatalf("malformed embedded set reached database: queries=%q", probe.queries)
			}
		})
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
			errs <- sqlmigrate.RunPostgres(context.Background(), db, fsys, "test", "harbor-sqlmigrate-test", sqlmigrate.ModeApply)
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Errorf("concurrent RunPostgres failed (advisory lock did not serialise): %v", e)
		}
	}
	if err := sqlmigrate.RunPostgres(context.Background(), db, fsys, "test", "harbor-sqlmigrate-test", sqlmigrate.ModeVerify); err != nil {
		t.Fatalf("read-only verification after live migration apply: %v", err)
	}
}
