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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"net/url"
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

// PostgresMigrationSpec describes the identity and required schema of one
// Harbor-owned Postgres projection. The subsystem is part of the migration
// identity; version numbers are never interpreted globally.
type PostgresMigrationSpec struct {
	Subsystem       string
	RequiredTables  []string
	RequiredColumns map[string][]string
}

var knownPostgresSubsystems = map[string]struct{}{
	"state":                 {},
	"memory":                {},
	"artifacts":             {},
	"skills":                {},
	"sessions.turns":        {},
	"observability.rollups": {},
}

// RunPostgresNamed applies or verifies a namespaced, checksummed Postgres
// migration ledger. It is the v1.29.1 runner used by all six Harbor-owned
// Postgres stores. Historical migration files remain immutable: the runner
// computes their SHA-256 and records it in harbor_schema_migrations.
//
// Apply mode requires direct PostgreSQL connectivity (normally port 5432)
// because it takes a session advisory lock. Verify mode is read-only and is
// compatible with transaction-pooled PgBouncer traffic on port 6432.
func RunPostgresNamed(ctx context.Context, db *sql.DB, migrationsFS fs.FS, spec PostgresMigrationSpec, errPrefix, advisoryLockName, dsn string, mode Mode) error {
	if spec.Subsystem == "" {
		return fmt.Errorf("%s: migration subsystem is required", errPrefix)
	}
	if _, ok := knownPostgresSubsystems[spec.Subsystem]; !ok {
		return fmt.Errorf("%s: migration subsystem %q is not in Harbor's closed PostgreSQL subsystem set", errPrefix, spec.Subsystem)
	}
	if len(spec.RequiredTables) == 0 {
		return fmt.Errorf("%s: migration required schema is empty for subsystem %q", errPrefix, spec.Subsystem)
	}
	for table := range spec.RequiredColumns {
		if !containsString(spec.RequiredTables, table) {
			return fmt.Errorf("%s: migration required columns name unknown table %q for subsystem %q", errPrefix, table, spec.Subsystem)
		}
	}
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
		return verifyNamedPostgres(ctx, db, migs, spec, errPrefix)
	}
	if err := ValidateDirectMigrationDSN(dsn); err != nil {
		return fmt.Errorf("%s: %w", errPrefix, err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%s: acquire migration conn: %w", errPrefix, err)
	}
	defer func() { _ = conn.Close() }()
	databaseIdentity, err := migrationDatabaseIdentity(ctx, conn, errPrefix)
	if err != nil {
		return err
	}
	lockKey := fnv64aSigned(migrationLockName(databaseIdentity, spec.Subsystem, advisoryLockName))
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return fmt.Errorf("%s: pg_advisory_lock: %w", errPrefix, err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey) //nolint:errcheck // unlock-on-defer; session close also releases it
	}()

	if err := ensureNamedLedger(ctx, conn, errPrefix); err != nil {
		return err
	}
	if err := ensureLegacyMirror(ctx, conn, errPrefix); err != nil {
		return err
	}
	applied, err := loadNamedLedger(ctx, conn, spec.Subsystem, errPrefix)
	if err != nil {
		return err
	}
	if err := validateNamedLedger(applied, migs, spec, errPrefix); err != nil {
		return err
	}
	legacy, err := loadLegacyVersions(ctx, conn, errPrefix)
	if err != nil {
		return err
	}
	observed, err := observedPostgresTables(ctx, conn, errPrefix)
	if err != nil {
		return err
	}
	columns, err := observedPostgresColumns(ctx, conn, errPrefix)
	if err != nil {
		return err
	}
	namespacedSubsystems, err := loadNamedSubsystems(ctx, conn, errPrefix)
	if err != nil {
		return err
	}
	if err := validateLegacyShape(spec, applied, legacy, observed, columns, namespacedSubsystems, errPrefix); err != nil {
		return err
	}
	if err := ensureStoreIdentity(ctx, conn, spec, highestAppliedVersion(applied), errPrefix); err != nil {
		return err
	}
	if len(applied) > len(migs) {
		return fmt.Errorf("%s: migration ledger for subsystem %q contains %d rows but this binary has only %d migrations; restore the matching Harbor release or perform an audited forward migration", errPrefix, spec.Subsystem, len(applied), len(migs))
	}

	for _, m := range migs {
		checksum := migrationChecksum(m.body)
		if row, ok := applied[m.version]; ok {
			if row.filename != m.name || row.checksumSHA256 != checksum {
				return fmt.Errorf("%s: migration identity mismatch for subsystem %q version %04d: ledger has filename=%q checksum_sha256=%s, binary has filename=%q checksum_sha256=%s; restore the matching Harbor release or perform an audited forward migration", errPrefix, spec.Subsystem, m.version, row.filename, row.checksumSHA256, m.name, checksum)
			}
			continue
		}
		if _, adopted := legacy[m.version]; adopted && hasRequiredSchema(spec, observed, columns) {
			if err := recordNamedMigration(ctx, conn, spec.Subsystem, m, checksum, errPrefix); err != nil {
				return err
			}
			continue
		}
		body := strings.ReplaceAll(m.body, "schema_migrations", legacyMirrorName(spec.Subsystem))
		if err := applyNamedPostgres(ctx, conn, spec.Subsystem, m, checksum, body, errPrefix); err != nil {
			return err
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type namedLedgerRow struct {
	filename       string
	checksumSHA256 string
}

func migrationChecksum(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func legacyMirrorName(subsystem string) string {
	return "harbor_legacy_schema_migrations"
}

func ensureNamedLedger(ctx context.Context, conn *sql.Conn, prefix string) error {
	_, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS harbor_schema_migrations (
			subsystem      TEXT NOT NULL,
			version        BIGINT NOT NULL,
			filename       TEXT NOT NULL,
			checksum_sha256 TEXT NOT NULL,
			applied_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (subsystem, version),
			UNIQUE (subsystem, filename),
			CHECK (subsystem IN ('state', 'memory', 'artifacts', 'skills', 'sessions.turns', 'observability.rollups')),
			CHECK (length(filename) > 0),
			CHECK (checksum_sha256 ~ '^[0-9a-f]{64}$')
		)`)
	if err != nil {
		return fmt.Errorf("%s: bootstrap harbor_schema_migrations: %w", prefix, err)
	}
	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS harbor_store_identity (
			subsystem                TEXT PRIMARY KEY CHECK (subsystem IN ('state', 'memory', 'artifacts', 'skills', 'sessions.turns', 'observability.rollups')),
			schema_version            BIGINT NOT NULL,
			contract_checksum_sha256 TEXT NOT NULL CHECK (contract_checksum_sha256 ~ '^[0-9a-f]{64}$'),
			created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("%s: bootstrap harbor_store_identity: %w", prefix, err)
	}
	return nil
}

func contractChecksum(spec PostgresMigrationSpec) string {
	tables := append([]string(nil), spec.RequiredTables...)
	sort.Strings(tables)
	parts := []string{spec.Subsystem, strings.Join(tables, "\x00")}
	columnTables := make([]string, 0, len(spec.RequiredColumns))
	for table := range spec.RequiredColumns {
		columnTables = append(columnTables, table)
	}
	sort.Strings(columnTables)
	for _, table := range columnTables {
		columns := append([]string(nil), spec.RequiredColumns[table]...)
		sort.Strings(columns)
		parts = append(parts, table, strings.Join(columns, "\x00"))
	}
	return migrationChecksum(strings.Join(parts, "\x00"))
}

func highestAppliedVersion(applied map[int]namedLedgerRow) int64 {
	var highest int
	for version := range applied {
		if version > highest {
			highest = version
		}
	}
	return int64(highest)
}

func validateNamedLedger(applied map[int]namedLedgerRow, migs []migration, spec PostgresMigrationSpec, prefix string) error {
	known := make(map[int]string, len(migs))
	for _, m := range migs {
		known[m.version] = m.name
	}
	for version, row := range applied {
		filename, ok := known[version]
		if !ok {
			return fmt.Errorf("%s: migration ledger for subsystem %q contains unknown version %d filename=%q; restore the matching Harbor release or perform an audited forward migration", prefix, spec.Subsystem, version, row.filename)
		}
		if row.filename != filename {
			return fmt.Errorf("%s: migration ledger for subsystem %q binds version %d to filename=%q, expected %q", prefix, spec.Subsystem, version, row.filename, filename)
		}
	}
	gap := false
	for _, migration := range migs {
		_, present := applied[migration.version]
		if !present {
			gap = true
			continue
		}
		if gap {
			return fmt.Errorf("%s: migration ledger for subsystem %q is not a contiguous prefix; version %04d is recorded after an unapplied migration", prefix, spec.Subsystem, migration.version)
		}
	}
	return nil
}

func ensureStoreIdentity(ctx context.Context, conn *sql.Conn, spec PostgresMigrationSpec, expectedAppliedVersion int64, prefix string) error {
	contract := contractChecksum(spec)
	var schemaVersion int64
	var observed string
	err := conn.QueryRowContext(ctx, `SELECT schema_version, contract_checksum_sha256 FROM harbor_store_identity WHERE subsystem = $1`, spec.Subsystem).Scan(&schemaVersion, &observed)
	if err == nil {
		if observed != contract {
			return fmt.Errorf("%s: store identity mismatch for subsystem %q: ledger contract_checksum_sha256=%s, binary contract_checksum_sha256=%s", prefix, spec.Subsystem, observed, contract)
		}
		if schemaVersion != expectedAppliedVersion {
			return fmt.Errorf("%s: store identity mismatch for subsystem %q: schema_version=%d but namespaced ledger highest version=%d", prefix, spec.Subsystem, schemaVersion, expectedAppliedVersion)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: read harbor_store_identity for %q: %w", prefix, spec.Subsystem, err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO harbor_store_identity (subsystem, schema_version, contract_checksum_sha256) VALUES ($1, $2, $3)`, spec.Subsystem, expectedAppliedVersion, contract); err != nil {
		return fmt.Errorf("%s: record harbor_store_identity for %q: %w", prefix, spec.Subsystem, err)
	}
	return nil
}

func verifyStoreIdentity(ctx context.Context, db *sql.DB, spec PostgresMigrationSpec, expectedVersion int, prefix string) error {
	var schemaVersion int64
	var observed string
	err := db.QueryRowContext(ctx, `SELECT schema_version, contract_checksum_sha256 FROM harbor_store_identity WHERE subsystem = $1`, spec.Subsystem).Scan(&schemaVersion, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: verify requires harbor_store_identity row for subsystem %q (remediation: run direct PostgreSQL 5432 apply before transaction-pooled verify)", prefix, spec.Subsystem)
	}
	if err != nil {
		return fmt.Errorf("%s: read harbor_store_identity for %q: %w", prefix, spec.Subsystem, err)
	}
	expected := contractChecksum(spec)
	if schemaVersion != int64(expectedVersion) || observed != expected {
		return fmt.Errorf("%s: verify store identity mismatch for subsystem %q: ledger schema_version=%d contract_checksum_sha256=%s, binary schema_version=%d contract_checksum_sha256=%s", prefix, spec.Subsystem, schemaVersion, observed, expectedVersion, expected)
	}
	return nil
}

func ensureLegacyMirror(ctx context.Context, conn *sql.Conn, prefix string) error {
	_, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS harbor_legacy_schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`)
	if err != nil {
		return fmt.Errorf("%s: inspect legacy schema_migrations: %w", prefix, err)
	}
	return nil
}

func loadNamedLedger(ctx context.Context, conn *sql.Conn, subsystem, prefix string) (map[int]namedLedgerRow, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version, filename, checksum_sha256 FROM harbor_schema_migrations WHERE subsystem = $1`, subsystem)
	if err != nil {
		return nil, fmt.Errorf("%s: read harbor_schema_migrations for %q: %w", prefix, subsystem, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int]namedLedgerRow)
	for rows.Next() {
		var version int
		var row namedLedgerRow
		if err := rows.Scan(&version, &row.filename, &row.checksumSHA256); err != nil {
			return nil, fmt.Errorf("%s: scan harbor_schema_migrations for %q: %w", prefix, subsystem, err)
		}
		out[version] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate harbor_schema_migrations for %q: %w", prefix, subsystem, err)
	}
	return out, nil
}

func loadLegacyVersions(ctx context.Context, conn *sql.Conn, prefix string) (map[int]struct{}, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("%s: read legacy schema_migrations: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int]struct{})
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("%s: scan legacy schema_migrations: %w", prefix, err)
		}
		out[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate legacy schema_migrations: %w", prefix, err)
	}
	return out, nil
}

func loadNamedSubsystems(ctx context.Context, conn *sql.Conn, prefix string) (map[string]struct{}, error) {
	rows, err := conn.QueryContext(ctx, `SELECT DISTINCT subsystem FROM harbor_schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("%s: read namespaced migration subsystems: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]struct{})
	for rows.Next() {
		var subsystem string
		if err := rows.Scan(&subsystem); err != nil {
			return nil, fmt.Errorf("%s: scan namespaced migration subsystem: %w", prefix, err)
		}
		out[subsystem] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate namespaced migration subsystems: %w", prefix, err)
	}
	return out, nil
}

func observedPostgresTables(ctx context.Context, conn *sql.Conn, prefix string) (map[string]struct{}, error) {
	rows, err := conn.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		return nil, fmt.Errorf("%s: inspect PostgreSQL schema objects: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("%s: scan PostgreSQL schema object: %w", prefix, err)
		}
		out[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate PostgreSQL schema objects: %w", prefix, err)
	}
	return out, nil
}

func observedPostgresColumns(ctx context.Context, conn *sql.Conn, prefix string) (map[string]map[string]struct{}, error) {
	rows, err := conn.QueryContext(ctx, `SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = current_schema()`)
	if err != nil {
		return nil, fmt.Errorf("%s: inspect PostgreSQL schema columns: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]map[string]struct{})
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, fmt.Errorf("%s: scan PostgreSQL schema column: %w", prefix, err)
		}
		if out[table] == nil {
			out[table] = make(map[string]struct{})
		}
		out[table][column] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: iterate PostgreSQL schema columns: %w", prefix, err)
	}
	return out, nil
}

func hasRequiredTables(required []string, observed map[string]struct{}) bool {
	for _, table := range required {
		if _, ok := observed[table]; !ok {
			return false
		}
	}
	return true
}

func hasRequiredSchema(spec PostgresMigrationSpec, observed map[string]struct{}, columns map[string]map[string]struct{}) bool {
	if !hasRequiredTables(spec.RequiredTables, observed) {
		return false
	}
	for table, required := range spec.RequiredColumns {
		if _, ok := observed[table]; !ok {
			return false
		}
		if columns == nil {
			continue
		}
		observedColumns := columns[table]
		for _, column := range required {
			if _, ok := observedColumns[column]; !ok {
				return false
			}
		}
	}
	return true
}

var knownSubsystemTables = map[string]string{
	"state_records":              "state",
	"memory_state":               "memory",
	"artifacts_blobs":            "artifacts",
	"skills":                     "skills",
	"installed_packages":         "skills",
	"installed_package_supports": "skills",
	"turn_rows":                  "sessions.turns",
	"turn_sessions":              "sessions.turns",
	"rollup_rows":                "observability.rollups",
	"rollup_checkpoint":          "observability.rollups",
	"rollup_fence":               "observability.rollups",
}

func validateLegacyShape(spec PostgresMigrationSpec, applied map[int]namedLedgerRow, legacy map[int]struct{}, observed map[string]struct{}, columns map[string]map[string]struct{}, namespacedSubsystems map[string]struct{}, prefix string) error {
	if hasRequiredSchema(spec, observed, columns) {
		return nil
	}
	foreign := make([]string, 0)
	for table := range observed {
		owner, known := knownSubsystemTables[table]
		if !known || owner == spec.Subsystem || table == "schema_migrations" || table == "harbor_schema_migrations" || table == "harbor_store_identity" || table == "harbor_legacy_schema_migrations" {
			continue
		}
		// A table from a subsystem that is already represented in the new
		// ledger is a compatibility mirror in a shared database. A legacy
		// bare ledger without that identity is the unsafe posture we refuse.
		if _, known := namespacedSubsystems[owner]; known {
			continue
		}
		foreign = append(foreign, table)
	}
	if len(foreign) > 0 && len(applied) == 0 {
		sort.Strings(foreign)
		return fmt.Errorf("%s: refusing %q migration verify/adoption: expected subsystem schema tables %v, observed foreign tables %v with legacy schema_migrations versions %v; the database is provisioned for another subsystem (remediation: point %s.dsn at the correct database or run the audited cutover)", prefix, spec.Subsystem, spec.RequiredTables, foreign, sortedVersions(legacy), spec.Subsystem)
	}
	return nil
}

func sortedVersions(values map[int]struct{}) []int {
	out := make([]int, 0, len(values))
	for v := range values {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func recordNamedMigration(ctx context.Context, conn *sql.Conn, subsystem string, m migration, checksum, prefix string) error {
	// Legacy adoption is bookkeeping-only, but it still changes three pieces
	// of migration authority. Keep the transaction on the conn that already
	// owns the subsystem advisory lock: a restart must observe either all
	// adoption records or none of them, never a canonical row with a stale
	// identity/mirror.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin legacy migration adoption %s: %w", prefix, m.name, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; original error is returned
		}
	}()
	if _, err := tx.ExecContext(ctx, `INSERT INTO harbor_schema_migrations (subsystem, version, filename, checksum_sha256) VALUES ($1, $2, $3, $4) ON CONFLICT (subsystem, version) DO NOTHING`, subsystem, m.version, m.name, checksum); err != nil {
		return fmt.Errorf("%s: record namespaced migration %s: %w", prefix, m.name, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE harbor_store_identity SET schema_version = $2, updated_at = NOW() WHERE subsystem = $1`, subsystem, m.version); err != nil {
		return fmt.Errorf("%s: update harbor_store_identity for %q: %w", prefix, subsystem, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, m.version); err != nil {
		return fmt.Errorf("%s: retain legacy migration mirror %s: %w", prefix, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit legacy migration adoption %s: %w", prefix, m.name, err)
	}
	committed = true
	return nil
}

func applyNamedPostgres(ctx context.Context, conn *sql.Conn, subsystem string, m migration, checksum, body, prefix string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin migration %s: %w", prefix, m.name, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; original error is returned
		}
	}()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("%s: execute migration %s: %w", prefix, m.name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO harbor_schema_migrations (subsystem, version, filename, checksum_sha256) VALUES ($1, $2, $3, $4)`, subsystem, m.version, m.name, checksum); err != nil {
		return fmt.Errorf("%s: record namespaced migration %s: %w", prefix, m.name, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE harbor_store_identity SET schema_version = $2, updated_at = NOW() WHERE subsystem = $1`, subsystem, m.version); err != nil {
		return fmt.Errorf("%s: update harbor_store_identity for %q: %w", prefix, subsystem, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, m.version); err != nil {
		return fmt.Errorf("%s: retain legacy migration mirror %s: %w", prefix, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit migration %s: %w", prefix, m.name, err)
	}
	committed = true
	return nil
}

func verifyNamedPostgres(ctx context.Context, db *sql.DB, migs []migration, spec PostgresMigrationSpec, prefix string) error {
	applied, err := loadNamedLedgerDB(ctx, db, spec.Subsystem, prefix)
	if err != nil {
		// A legacy database is diagnosed using its actual objects; integer
		// versions alone are never accepted as readiness.
		legacy, legacyErr := loadLegacyVersionsDB(ctx, db, prefix)
		if legacyErr == nil {
			observed, observedErr := observedPostgresTablesDB(ctx, db, prefix)
			columns, columnsErr := observedPostgresColumnsDB(ctx, db, prefix)
			if observedErr == nil && columnsErr == nil && !hasRequiredSchema(spec, observed, columns) {
				foreign := make([]string, 0)
				for table := range observed {
					if owner, ok := knownSubsystemTables[table]; ok && owner != spec.Subsystem {
						foreign = append(foreign, table)
					}
				}
				sort.Strings(foreign)
				return fmt.Errorf("%s: verify refused for expected subsystem %q: observed legacy schema_migrations versions %v and tables %v (foreign known tables %v), but required tables/columns %v/%v are absent; this is not a %s schema (remediation: run direct PostgreSQL 5432 apply against the correct DSN or audited cutover; do not mark verify ready)", prefix, spec.Subsystem, sortedVersions(legacy), sortedTableNames(observed), foreign, spec.RequiredTables, spec.RequiredColumns, spec.Subsystem)
			}
		}
		return fmt.Errorf("%s: verify requires namespaced harbor_schema_migrations for subsystem %q (remediation: run migration apply over direct PostgreSQL 5432, then use transaction-pooled verify): %w", prefix, spec.Subsystem, err)
	}
	observed, err := observedPostgresTablesDB(ctx, db, prefix)
	if err != nil {
		return err
	}
	columns, err := observedPostgresColumnsDB(ctx, db, prefix)
	if err != nil {
		return err
	}
	if !hasRequiredSchema(spec, observed, columns) {
		return fmt.Errorf("%s: verify refused for subsystem %q: namespaced ledger is present but required schema tables/columns are incomplete (required tables=%v columns=%v; observed tables=%v; remediation: run direct PostgreSQL 5432 apply against the correct DSN or audited cutover)", prefix, spec.Subsystem, spec.RequiredTables, spec.RequiredColumns, sortedTableNames(observed))
	}
	if err := verifyStoreIdentity(ctx, db, spec, migs[len(migs)-1].version, prefix); err != nil {
		return err
	}
	for _, m := range migs {
		row, ok := applied[m.version]
		if !ok {
			return fmt.Errorf("%s: verify migrations for %q: missing namespaced migration %s", prefix, spec.Subsystem, m.name)
		}
		checksum := migrationChecksum(m.body)
		if row.filename != m.name || row.checksumSHA256 != checksum {
			return fmt.Errorf("%s: verify migration identity mismatch for %q version %04d: ledger filename=%q checksum_sha256=%s, binary filename=%q checksum_sha256=%s", prefix, spec.Subsystem, m.version, row.filename, row.checksumSHA256, m.name, checksum)
		}
	}
	if len(applied) != len(migs) {
		return fmt.Errorf("%s: verify migrations for %q: namespaced ledger has %d rows, expected exactly %d ordered migrations (remediation: apply the matching direct PostgreSQL 5432 migration set)", prefix, spec.Subsystem, len(applied), len(migs))
	}
	return nil
}

func sortedTableNames(observed map[string]struct{}) []string {
	result := make([]string, 0, len(observed))
	for table := range observed {
		result = append(result, table)
	}
	sort.Strings(result)
	return result
}

func loadNamedLedgerDB(ctx context.Context, db *sql.DB, subsystem, prefix string) (map[int]namedLedgerRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, filename, checksum_sha256 FROM harbor_schema_migrations WHERE subsystem = $1`, subsystem)
	if err != nil {
		return nil, fmt.Errorf("%s: read harbor_schema_migrations for %q: %w", prefix, subsystem, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int]namedLedgerRow)
	for rows.Next() {
		var version int
		var row namedLedgerRow
		if err := rows.Scan(&version, &row.filename, &row.checksumSHA256); err != nil {
			return nil, err
		}
		out[version] = row
	}
	return out, rows.Err()
}

func loadLegacyVersionsDB(ctx context.Context, db *sql.DB, prefix string) (map[int]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("%s: read legacy schema_migrations: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[int]struct{})
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		out[version] = struct{}{}
	}
	return out, rows.Err()
}

func observedPostgresTablesDB(ctx context.Context, db *sql.DB, prefix string) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		return nil, fmt.Errorf("%s: inspect PostgreSQL schema objects: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = struct{}{}
	}
	return out, rows.Err()
}

func observedPostgresColumnsDB(ctx context.Context, db *sql.DB, prefix string) (map[string]map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = current_schema()`)
	if err != nil {
		return nil, fmt.Errorf("%s: inspect PostgreSQL schema columns: %w", prefix, err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]map[string]struct{})
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, err
		}
		if out[table] == nil {
			out[table] = make(map[string]struct{})
		}
		out[table][column] = struct{}{}
	}
	return out, rows.Err()
}

// ValidateDirectMigrationDSN refuses the well-known transaction-pooled
// endpoint. PgBouncer transaction mode cannot safely hold a session advisory
// lock across migration statements; ordinary verify traffic remains safe.
func ValidateDirectMigrationDSN(dsn string) error {
	lower := strings.ToLower(strings.TrimSpace(dsn))
	if lower == "" {
		return errors.New("migration apply requires a direct PostgreSQL DSN on port 5432; DSN is empty")
	}
	compact := strings.ReplaceAll(lower, " ", "")
	if strings.Contains(compact, "port=6432") || strings.Contains(compact, "pool_mode=transaction") || strings.Contains(compact, "pgbouncer=true") {
		return errors.New("migration apply requires direct PostgreSQL 5432 connectivity; transaction-pooled PgBouncer 6432 DSN is unsafe for advisory locks")
	}
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		if parsed, err := url.Parse(dsn); err == nil && parsed.Port() == "6432" {
			return errors.New("migration apply requires direct PostgreSQL 5432 connectivity; transaction-pooled PgBouncer 6432 DSN is unsafe for advisory locks")
		}
	}
	return nil
}

func migrationDatabaseIdentity(ctx context.Context, conn *sql.Conn, prefix string) (string, error) {
	var database, serverAddress string
	var serverPort int
	if err := conn.QueryRowContext(ctx, `SELECT current_database(), COALESCE(inet_server_addr()::text, ''), COALESCE(inet_server_port(), 0)`).Scan(&database, &serverAddress, &serverPort); err != nil {
		return "", fmt.Errorf("%s: identify PostgreSQL database for migration lock: %w", prefix, err)
	}
	return migrationDatabaseIdentityKey(database, serverAddress, serverPort), nil
}

func migrationDatabaseIdentityKey(database, serverAddress string, serverPort int) string {
	return database + "\x00" + serverAddress + "\x00" + strconv.Itoa(serverPort)
}

func migrationLockName(databaseIdentity, subsystem, requested string) string {
	// The identity is read from the already-acquired PostgreSQL session. It
	// intentionally excludes credentials and DSN spelling, so equivalent DSNs
	// for one database share the same advisory-lock authority.
	identityHash := migrationChecksum(databaseIdentity)
	return "harbor-migrations:" + identityHash + ":" + subsystem + ":" + requested
}
