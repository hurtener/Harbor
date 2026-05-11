package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// migrationsFS holds the forward-only SQL migrations bundled into the
// binary. Each file is named `NNNN_<slug>.sql` (zero-padded numeric
// version) and is applied in lexicographic order. The bundling makes
// the driver self-contained — no external migrate tool, no
// runtime-discovered SQL files.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationFilenameRE captures the leading numeric version of a
// migration filename. `0001_init.sql` → version 1.
var migrationFilenameRE = regexp.MustCompile(`^(\d+)_[^/]+\.sql$`)

// migrate applies any forward-only migrations whose version is not
// already present in `schema_migrations` to the supplied database.
//
// Forward-only contract (AGENTS.md §13): migrations are numbered
// monotonically. Editing a merged migration is forbidden; future
// schema changes land as new files. Each file ends with
// `INSERT OR IGNORE INTO schema_migrations(version) VALUES (N);` so
// the runner is idempotent even if a migration is run twice.
//
// Each migration runs inside a single transaction so either all of its
// statements apply or none do.
func migrate(ctx context.Context, db *sql.DB) error {
	if err := ensureMigrationsTable(ctx, db); err != nil {
		return err
	}

	files, err := listMigrations()
	if err != nil {
		return err
	}

	applied, err := loadAppliedVersions(ctx, db)
	if err != nil {
		return err
	}

	for _, f := range files {
		if _, ok := applied[f.version]; ok {
			continue
		}
		if err := applyMigration(ctx, db, f); err != nil {
			return fmt.Errorf("memory/sqlite: apply migration %s: %w", f.name, err)
		}
	}
	return nil
}

// migrationFile pairs a migration's filename (used for error messages)
// with its parsed numeric version (used for ordering and bookkeeping).
type migrationFile struct {
	name    string
	version int
}

// listMigrations returns the embedded migration files sorted by
// version ascending. A filename that does not match `NNNN_<slug>.sql`
// is a build-time bug — we surface it loudly rather than silently
// skipping.
func listMigrations() ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("memory/sqlite: read embedded migrations: %w", err)
	}
	out := make([]migrationFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilenameRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("memory/sqlite: migration %q does not match NNNN_<slug>.sql", e.Name())
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("memory/sqlite: migration %q has unparseable version: %w", e.Name(), err)
		}
		out = append(out, migrationFile{name: e.Name(), version: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// ensureMigrationsTable creates `schema_migrations` if the database
// has never been touched. The table is also created by `0001_init.sql`
// — the duplicate `CREATE TABLE IF NOT EXISTS` is intentional and
// harmless. Without this preflight, `loadAppliedVersions` would fail
// on a clean DB before the first migration ran.
func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS schema_migrations (
            version    INTEGER PRIMARY KEY,
            applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        )`)
	if err != nil {
		return fmt.Errorf("memory/sqlite: bootstrap schema_migrations: %w", err)
	}
	return nil
}

// loadAppliedVersions returns the set of versions already recorded in
// `schema_migrations`.
func loadAppliedVersions(ctx context.Context, db *sql.DB) (map[int]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("memory/sqlite: read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[int]struct{}{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("memory/sqlite: scan schema_migrations.version: %w", err)
		}
		out[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory/sqlite: iterate schema_migrations: %w", err)
	}
	return out, nil
}

// applyMigration reads the embedded SQL for f and executes it inside
// a single transaction. The trailing
// `INSERT OR IGNORE INTO schema_migrations` keeps the runner
// idempotent across repeat runs (see migrate's contract).
func applyMigration(ctx context.Context, db *sql.DB, f migrationFile) error {
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
