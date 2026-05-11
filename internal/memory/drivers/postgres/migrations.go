package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// migrationsFS embeds the forward-only SQL migration files. Filenames
// are `NNNN_slug.sql`; the leading 4-digit version is parsed and used
// as the row key in the `schema_migrations` table.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// advisoryLockKey is the int64 passed to pg_advisory_lock to
// serialise migration application across multiple replicas booting
// simultaneously. The key is derived from the FNV-64a hash of a
// stable string so all replicas compute the same value without
// coordination. A distinct key per Harbor subsystem (state, memory,
// artifacts) keeps the migration runners from competing for the same
// lock when a single binary boots multiple persistent subsystems.
var advisoryLockKey = fnv64aSigned("harbor-memory-migrations")

// fnv64aSigned returns the FNV-64a hash of s reinterpreted as int64.
// hash/fnv writes to its hasher never fail (the underlying buffer
// grows in memory) so the Write error is impossible by construction.
func fnv64aSigned(s string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	//nolint:gosec // intentional bit reinterpretation; pg_advisory_lock takes int8
	return int64(h.Sum64())
}

// migration is one forward-only SQL file plus its parsed version.
// Field order chosen for size-class packing per `govet:fieldalignment`.
type migration struct {
	name    string
	body    string
	version int
}

// loadMigrations reads every `NNNN_*.sql` entry from migrationsFS and
// returns them sorted ascending by version. Filenames must start with
// a 4-digit version followed by an underscore; anything else is an
// error.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("memory/postgres: read embedded migrations: %w", err)
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		vstr, _, ok := strings.Cut(e.Name(), "_")
		if !ok || len(vstr) != 4 {
			return nil, fmt.Errorf("memory/postgres: malformed migration filename %q (want NNNN_*.sql)", e.Name())
		}
		v, err := strconv.Atoi(vstr)
		if err != nil {
			return nil, fmt.Errorf("memory/postgres: malformed migration version in %q: %w", e.Name(), err)
		}
		body, err := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("memory/postgres: read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// applyMigrations runs every migration in migrationsFS that has not
// yet been recorded in `schema_migrations`, wrapping the entire run
// in a session-level pg_advisory_lock. The advisory lock serialises
// concurrent New() calls across replicas so no two writers race on
// CREATE TABLE / INSERT INTO schema_migrations.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(migs) == 0 {
		return errors.New("memory/postgres: no migrations found in embedded migrations/ — package mis-built")
	}

	// Acquire a dedicated connection so the advisory lock stays bound
	// to the same session for the lifetime of the run.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("memory/postgres: acquire migration conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, lockErr := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); lockErr != nil {
		return fmt.Errorf("memory/postgres: pg_advisory_lock: %w", lockErr)
	}
	defer func() {
		// Release the lock; we ignore the error because the connection
		// will be returned to the pool either way and a stuck advisory
		// lock would only cost a single backend session.
		_, _ = conn.ExecContext(context.Background(), //nolint:errcheck // unlock-on-defer; lock auto-releases at session end
			"SELECT pg_advisory_unlock($1)", advisoryLockKey)
	}()

	// Ensure schema_migrations exists before any version probe. If the
	// table is missing (clean DB), the first migration creates it; the
	// CREATE TABLE IF NOT EXISTS in 0001_init.sql is idempotent.
	if _, bootErr := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER     PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`); bootErr != nil {
		return fmt.Errorf("memory/postgres: create schema_migrations bootstrap: %w", bootErr)
	}

	applied, err := loadAppliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	for _, m := range migs {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := applyOne(ctx, conn, m); err != nil {
			return fmt.Errorf("memory/postgres: apply migration %s: %w", m.name, err)
		}
	}
	return nil
}

// loadAppliedVersions returns the set of versions present in
// schema_migrations.
func loadAppliedVersions(ctx context.Context, conn *sql.Conn) (map[int]struct{}, error) {
	rows, err := conn.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("memory/postgres: select schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int]struct{}{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("memory/postgres: scan schema_migrations: %w", err)
		}
		out[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory/postgres: iterate schema_migrations: %w", err)
	}
	return out, nil
}

// applyOne runs a single migration's body inside a transaction. The
// migration file is responsible for inserting its own
// schema_migrations row (matching the SQLite convention). The
// transaction guarantees that a partial failure leaves no half-applied
// state.
func applyOne(ctx context.Context, conn *sql.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() //nolint:errcheck // rollback is best-effort; surfaced error is the original failure
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
