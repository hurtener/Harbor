// Package sqlmigrate is the shared forward-only SQL migration runner for
// Harbor's SQLite and Postgres persistence drivers. It is the single home
// of the migration contract that was previously copy-pasted per driver:
// the `NNNN_<slug>.sql` filename rule, the partial-apply precheck against
// `schema_migrations`, the per-migration transaction, and (Postgres) the
// FNV-64a advisory-key derivation that serialises concurrent boots.
//
// Forward-only contract (unchanged from the per-driver runners): migrations
// are numbered monotonically; editing a merged migration is forbidden;
// future schema changes land as new files. A bad filename in the embed set
// is a build-time bug and is surfaced loudly, never skipped.
//
// SQLite and Postgres keep deliberately distinct runners — they differ in
// the migrations-table DDL (`TIMESTAMP`/`CURRENT_TIMESTAMP` vs
// `TIMESTAMPTZ`/`NOW()`), in who records the applied version (the SQLite
// runner inserts it; the Postgres migration body inserts its own row),
// and in concurrency control (Postgres apply mode takes a session
// `pg_advisory_lock`, while Postgres verify mode and SQLite need none). Each
// driver passes its own embedded `migrations/` FS and error prefix so wrapped
// errors read exactly as before.
package sqlmigrate

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// sqliteFilenameRE captures the leading numeric version of a migration
// filename. `0001_init.sql` → version 1.
var sqliteFilenameRE = regexp.MustCompile(`^(\d+)_[^/]+\.sql$`)

// migrationFile pairs a migration's filename (for error messages) with
// its parsed numeric version (for ordering + bookkeeping).
type migrationFile struct {
	name    string
	version int
}

// RunSQLite applies any forward-only migrations in migrationsFS whose
// version is not already present in `schema_migrations` to db. errPrefix
// is the driver's error-wrap prefix (e.g. "state/sqlite"). Each migration
// runs inside a single transaction; the runner records the applied
// version with `INSERT OR IGNORE` so a partially-applied DB (DDL applied
// but the trailing INSERT lost) is still recoverable.
func RunSQLite(ctx context.Context, db *sql.DB, migrationsFS fs.FS, errPrefix string) error {
	if err := ensureSQLiteTable(ctx, db, errPrefix); err != nil {
		return err
	}
	files, err := listSQLiteMigrations(migrationsFS, errPrefix)
	if err != nil {
		return err
	}
	applied, err := loadAppliedSQLite(ctx, db, errPrefix)
	if err != nil {
		return err
	}
	for _, f := range files {
		if _, ok := applied[f.version]; ok {
			continue
		}
		if err := applySQLiteMigration(ctx, db, migrationsFS, f); err != nil {
			return fmt.Errorf("%s: apply migration %s: %w", errPrefix, f.name, err)
		}
	}
	return nil
}

func listSQLiteMigrations(migrationsFS fs.FS, errPrefix string) ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("%s: read embedded migrations: %w", errPrefix, err)
	}
	out := make([]migrationFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := sqliteFilenameRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("%s: migration %q does not match NNNN_<slug>.sql", errPrefix, e.Name())
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("%s: migration %q has unparseable version: %w", errPrefix, e.Name(), err)
		}
		out = append(out, migrationFile{name: e.Name(), version: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// ensureSQLiteTable creates `schema_migrations` if the DB has never been
// touched. The duplicate `CREATE TABLE IF NOT EXISTS` (also in
// `0001_init.sql`) is intentional and harmless — without it,
// loadAppliedSQLite would fail on a clean DB before the first migration.
func ensureSQLiteTable(ctx context.Context, db *sql.DB, errPrefix string) error {
	_, err := db.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS schema_migrations (
            version    INTEGER PRIMARY KEY,
            applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        )`)
	if err != nil {
		return fmt.Errorf("%s: bootstrap schema_migrations: %w", errPrefix, err)
	}
	return nil
}

func loadAppliedSQLite(ctx context.Context, db *sql.DB, errPrefix string) (map[int]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("%s: read schema_migrations: %w", errPrefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int]struct{}{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("%s: scan schema_migrations.version: %w", errPrefix, err)
		}
		out[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate schema_migrations: %w", errPrefix, err)
	}
	return out, nil
}

// applySQLiteMigration reads f's SQL and executes it in a single
// transaction, then records the version with INSERT OR IGNORE.
func applySQLiteMigration(ctx context.Context, db *sql.DB, migrationsFS fs.FS, f migrationFile) error {
	body, err := fs.ReadFile(migrationsFS, "migrations/"+f.name)
	if err != nil {
		return fmt.Errorf("read embedded SQL: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	rolled := false
	defer func() {
		if !rolled {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original failure
		}
	}()
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("exec migration body: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations(version) VALUES (?)`, f.version); err != nil {
		return fmt.Errorf("record migration version %d: %w", f.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	rolled = true
	return nil
}

// migration is one forward-only Postgres migration plus its parsed
// version. Field order chosen for size-class packing per
// govet:fieldalignment.
type migration struct {
	name    string
	body    string
	version int
}

// RunPostgres applies or verifies the forward-only migrations in migrationsFS.
// Empty mode resolves to ModeApply, which wraps application in a session-level
// `pg_advisory_lock` derived from advisoryLockName so concurrent New() calls
// across replicas don't race. ModeVerify performs only a read-only query of
// the existing `schema_migrations` ledger: it creates no table, begins no
// transaction, takes no advisory lock, and fails when an embedded migration is
// not recorded. errPrefix is the driver's error-wrap prefix (e.g. "postgres",
// "memory/postgres"). Migration bodies record their own schema_migrations row.
func RunPostgres(ctx context.Context, db *sql.DB, migrationsFS fs.FS, errPrefix, advisoryLockName string, mode Mode) error {
	resolvedMode, err := mode.Resolve()
	if err != nil {
		return fmt.Errorf("%s: %w", errPrefix, err)
	}
	migs, err := loadPostgresMigrations(migrationsFS, errPrefix)
	if err != nil {
		return err
	}
	if len(migs) == 0 {
		return fmt.Errorf("%s: no migrations found in embedded migrations/ — package mis-built", errPrefix)
	}
	if resolvedMode == ModeVerify {
		return verifyPostgres(ctx, db, migs, errPrefix)
	}

	// Dedicated connection so the advisory lock stays bound to one session.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%s: acquire migration conn: %w", errPrefix, err)
	}
	defer func() { _ = conn.Close() }()

	lockKey := fnv64aSigned(advisoryLockName)
	if _, lockErr := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", lockKey); lockErr != nil {
		return fmt.Errorf("%s: pg_advisory_lock: %w", errPrefix, lockErr)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), //nolint:errcheck // unlock-on-defer; lock auto-releases at session end either way
			"SELECT pg_advisory_unlock($1)", lockKey)
	}()

	if _, bootErr := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER     PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`); bootErr != nil {
		return fmt.Errorf("%s: create schema_migrations bootstrap: %w", errPrefix, bootErr)
	}

	applied, err := loadAppliedPostgres(ctx, conn, errPrefix)
	if err != nil {
		return err
	}
	for _, m := range migs {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := applyOnePostgres(ctx, conn, m); err != nil {
			return fmt.Errorf("%s: apply migration %s: %w", errPrefix, m.name, err)
		}
	}
	return nil
}

// verifyPostgres proves every embedded migration is recorded in the existing
// ledger. Querying through *sql.DB is deliberate: unlike apply mode, verify
// mode needs no session affinity and is compatible with transaction-pooled
// Postgres connections. A missing/malformed ledger fails through the SELECT or
// Scan path; this function has no write-capable call site.
func verifyPostgres(ctx context.Context, db *sql.DB, migs []migration, errPrefix string) error {
	applied, err := loadAppliedPostgres(ctx, db, errPrefix)
	if err != nil {
		return fmt.Errorf("%s: verify migrations: %w", errPrefix, err)
	}
	missing := make([]string, 0)
	for _, m := range migs {
		if _, ok := applied[m.version]; !ok {
			missing = append(missing, m.name)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("%s: verify migrations: unapplied embedded migrations: %s", errPrefix, strings.Join(missing, ", "))
	}
	return nil
}

func loadPostgresMigrations(migrationsFS fs.FS, errPrefix string) ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("%s: read embedded migrations: %w", errPrefix, err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		vstr, _, ok := strings.Cut(e.Name(), "_")
		if !ok || len(vstr) != 4 {
			return nil, fmt.Errorf("%s: malformed migration filename %q (want NNNN_*.sql)", errPrefix, e.Name())
		}
		v, err := strconv.Atoi(vstr)
		if err != nil {
			return nil, fmt.Errorf("%s: malformed migration version in %q: %w", errPrefix, e.Name(), err)
		}
		if v <= 0 {
			return nil, fmt.Errorf("%s: malformed migration version in %q: must be greater than zero", errPrefix, e.Name())
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("%s: read migration %q: %w", errPrefix, e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i-1].version == out[i].version {
			return nil, fmt.Errorf("%s: duplicate migration version %04d in %q and %q", errPrefix, out[i].version, out[i-1].name, out[i].name)
		}
	}
	return out, nil
}

type postgresQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadAppliedPostgres(ctx context.Context, queryer postgresQueryer, errPrefix string) (map[int]struct{}, error) {
	rows, err := queryer.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("%s: select schema_migrations: %w", errPrefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int]struct{}{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("%s: scan schema_migrations: %w", errPrefix, err)
		}
		out[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate schema_migrations: %w", errPrefix, err)
	}
	return out, nil
}

// applyOnePostgres runs a single migration's body inside a transaction.
// The migration file is responsible for inserting its own
// schema_migrations row (matching the SQLite convention).
func applyOnePostgres(ctx context.Context, conn *sql.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; the surfaced error is the original commit/exec failure
		}
	}()
	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("exec body: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// fnv64aSigned returns the FNV-64a hash of s reinterpreted as int64.
// hash/fnv writes never fail (the buffer grows in memory), so the Write
// error is impossible by construction. pg_advisory_lock takes a signed
// int64; the bit-pattern reinterpretation is intentional and safe —
// Postgres hashes the bits, not their numeric value.
func fnv64aSigned(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	//nolint:gosec // intentional bit reinterpretation; pg_advisory_lock takes int8
	return int64(h.Sum64())
}
