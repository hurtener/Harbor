package localdb

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/skills"
)

// readSchemaVersions returns the sorted applied-migration versions from
// `schema_migrations` via the driver's own pool.
func readSchemaVersions(t *testing.T, d *driver) []int {
	t.Helper()
	rows, err := d.db.QueryContext(context.Background(),
		`SELECT version FROM schema_migrations ORDER BY version ASC`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := []int{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate schema_migrations: %v", err)
	}
	return out
}

// TestMigrate_InstalledPackages_CleanStartsAtV3 — a fresh file-backed DB
// applies the additive installed-package migration: schema_migrations is
// exactly [1 2 3].
func TestMigrate_InstalledPackages_CleanStartsAtV3(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "installed-clean.sqlite")
	d, err := New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn}, skills.Deps{Bus: covBus(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = d.Close(context.Background()) }()

	want := []int{1, 2, 3}
	if got := readSchemaVersions(t, d.(*driver)); !reflect.DeepEqual(got, want) {
		t.Fatalf("schema_migrations = %v, want %v", got, want)
	}
}

// TestMigrate_InstalledPackages_IdempotentReopen — re-opening the same DB
// applies nothing: schema_migrations stays [1 2 3] and the second store
// works (the migration is restart-safe).
func TestMigrate_InstalledPackages_IdempotentReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "installed-reopen.sqlite")
	s1, err := New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn}, skills.Deps{Bus: covBus(t)})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if got := readSchemaVersions(t, s1.(*driver)); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("first open schema_migrations = %v, want [1 2 3]", got)
	}
	if err := s1.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	s2, err := New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn}, skills.Deps{Bus: covBus(t)})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()
	if got := readSchemaVersions(t, s2.(*driver)); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("second open schema_migrations = %v, want [1 2 3]", got)
	}
}

// TestMigrate_InstalledPackages_ConcurrentNew — N concurrent New() calls
// against the same file-backed DB all succeed and schema_migrations stays
// exactly [1 2 3] (INSERT OR IGNORE + busy_timeout make the concurrent
// boot safe; SQLite needs no advisory lock).
func TestMigrate_InstalledPackages_ConcurrentNew(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "installed-concurrent.sqlite")
	const n = 12
	var (
		wg     sync.WaitGroup
		errsN  int
		errsMu sync.Mutex
	)
	stores := make([]skills.SkillStore, n)
	wg.Add(n)
	start := make(chan struct{})
	for i := range n {
		go func(i int) {
			defer wg.Done()
			<-start
			s, err := New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn}, skills.Deps{Bus: covBus(t)})
			if err != nil {
				errsMu.Lock()
				errsN++
				errsMu.Unlock()
				t.Errorf("goroutine %d: New: %v", i, err)
				return
			}
			stores[i] = s
		}(i)
	}
	close(start)
	wg.Wait()
	for _, s := range stores {
		if s != nil {
			_ = s.Close(context.Background())
		}
	}
	if errsN != 0 {
		t.Fatalf("%d concurrent migration runs errored", errsN)
	}
	probe, err := New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn}, skills.Deps{Bus: covBus(t)})
	if err != nil {
		t.Fatalf("probe New: %v", err)
	}
	defer func() { _ = probe.Close(context.Background()) }()
	if got := readSchemaVersions(t, probe.(*driver)); !reflect.DeepEqual(got, []int{1, 2, 3}) {
		t.Fatalf("schema_migrations after %d concurrent runs = %v, want [1 2 3]", n, got)
	}
}

// TestBootstrapSQLite_RetriesCrossPoolContention proves the open-time retry
// covers migration work as well as the WAL verification query. The lock holder
// and contender use independent pools, matching overlapping New calls and
// processes; the contender's 1 ms busy timeout forces at least one retry
// before the holder releases its transaction.
func TestBootstrapSQLite_RetriesCrossPoolContention(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "bootstrap-contention.sqlite")
	lockerDSN := dsn + `?_pragma=busy_timeout%285000%29&_pragma=journal_mode%28WAL%29&_txlock=immediate`
	locker, err := sql.Open(sqliteDriver, lockerDSN)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	locker.SetMaxOpenConns(1)
	defer func() { _ = locker.Close() }()
	if err := verifyJournalMode(context.Background(), locker, dsn); err != nil {
		t.Fatalf("initialize WAL: %v", err)
	}

	tx, err := locker.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(), `CREATE TABLE lock_probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("acquire write lock: %v", err)
	}

	contenderDSN := dsn + `?_pragma=busy_timeout%281%29&_pragma=journal_mode%28WAL%29&_txlock=immediate`
	contender, err := sql.Open(sqliteDriver, contenderDSN)
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	contender.SetMaxOpenConns(1)
	defer func() { _ = contender.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- bootstrapSQLite(ctx, contender, dsn) }()
	time.Sleep(100 * time.Millisecond)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("release lock holder: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("bootstrap after contention: %v", err)
	}
}

// TestInstalledPackageSchema_TablesColumnsIndex pins the additive schema
// shape: the installed_packages envelope columns, the installed_support
// manifest columns (including the BLOB data column), the by-package
// support index, and the FK binding support rows to their envelope.
func TestInstalledPackageSchema_TablesColumnsIndex(t *testing.T) {
	d := covDriver(t)
	ctx := context.Background()

	pkgCols := []string{
		"tenant", "user", "agent_id", "name", "origin", "package_hash",
		"package_version", "package_json", "skill_json", "created_at", "updated_at",
	}
	for _, col := range pkgCols {
		var n int
		if err := d.db.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info('installed_packages') WHERE name = ?`, col).Scan(&n); err != nil || n != 1 {
			t.Fatalf("installed_packages missing column %q (n=%d err=%v)", col, n, err)
		}
	}
	supportCols := []string{
		"tenant", "user", "agent_id", "name", "path", "mime", "size", "digest", "data",
	}
	for _, col := range supportCols {
		var n int
		if err := d.db.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info('installed_support') WHERE name = ?`, col).Scan(&n); err != nil || n != 1 {
			t.Fatalf("installed_support missing column %q (n=%d err=%v)", col, n, err)
		}
	}

	var idx int
	if err := d.db.QueryRowContext(ctx, `
        SELECT count(*) FROM sqlite_master
        WHERE type = 'index' AND name = 'installed_support_by_package' AND tbl_name = 'installed_support'`,
	).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("installed_support_by_package index missing (n=%d err=%v)", idx, err)
	}

	var fk int
	if err := d.db.QueryRowContext(ctx, `
        SELECT count(*) FROM pragma_foreign_key_list('installed_support') WHERE "table" = 'installed_packages'`,
	).Scan(&fk); err != nil || fk != 4 {
		t.Fatalf("installed_support -> installed_packages FK missing (n=%d err=%v)", fk, err)
	}
}
