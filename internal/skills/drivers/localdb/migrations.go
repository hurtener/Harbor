package localdb

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

// migrationsFS holds the forward-only SQL migrations bundled into
// the binary. Each file is named `NNNN_<slug>.sql` (zero-padded
// numeric version) and is applied in lexicographic order.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

var migrationFilenameRE = regexp.MustCompile(`^(\d+)_[^/]+\.sql$`)

// migrate applies any forward-only migrations whose version is not
// already present in `schema_migrations`. Mirrors the memory/sqlite
// runner verbatim — the only divergence is the package path in
// error messages.
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
			return fmt.Errorf("skills/localdb: apply migration %s: %w", f.name, err)
		}
	}
	return nil
}

type migrationFile struct {
	name    string
	version int
}

func listMigrations() ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: read embedded migrations: %w", err)
	}
	out := make([]migrationFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilenameRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("skills/localdb: migration %q does not match NNNN_<slug>.sql", e.Name())
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("skills/localdb: migration %q has unparseable version: %w", e.Name(), err)
		}
		out = append(out, migrationFile{name: e.Name(), version: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
        CREATE TABLE IF NOT EXISTS schema_migrations (
            version    INTEGER PRIMARY KEY,
            applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        )`)
	if err != nil {
		return fmt.Errorf("skills/localdb: bootstrap schema_migrations: %w", err)
	}
	return nil
}

func loadAppliedVersions(ctx context.Context, db *sql.DB) (map[int]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("skills/localdb: read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int]struct{}{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("skills/localdb: scan schema_migrations.version: %w", err)
		}
		out[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("skills/localdb: iterate schema_migrations: %w", err)
	}
	return out, nil
}

func applyMigration(ctx context.Context, db *sql.DB, f migrationFile) error {
	body, err := fs.ReadFile(migrationsFS, "migrations/"+f.name)
	if err != nil {
		return fmt.Errorf("read embedded SQL: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; surfaced error is the original failure
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
	committed = true
	return nil
}
