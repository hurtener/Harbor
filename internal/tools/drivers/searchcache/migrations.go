package searchcache

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

//go:embed migrations/*.sql
var migrationsFS embed.FS

var migrationFilenameRE = regexp.MustCompile(`^(\d+)_[^/]+\.sql$`)

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
		if _, err := db.ExecContext(ctx, string(f.body)); err != nil {
			return fmt.Errorf("searchcache: migration %s: %w", f.name, err)
		}
	}
	return nil
}

func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS tool_cache_migrations (version INTEGER PRIMARY KEY)`)
	return err
}

type migrationFile struct {
	version int
	name    string
	body    []byte
}

func listMigrations() ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("searchcache: read migrations dir: %w", err)
	}
	var out []migrationFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilenameRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("searchcache: read %s: %w", e.Name(), err)
		}
		out = append(out, migrationFile{version: v, name: e.Name(), body: body})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func loadAppliedVersions(ctx context.Context, db *sql.DB) (map[int]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM tool_cache_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("searchcache: query migrations: %w", err)
	}
	defer rows.Close()
	out := make(map[int]struct{})
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = struct{}{}
	}
	return out, rows.Err()
}
