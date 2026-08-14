package postgres

import (
	"context"
	"database/sql"
	"embed"

	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

// migrationsFS embeds the forward-only SQL migration files. Filenames are
// `NNNN_slug.sql`; the leading 4-digit version is the row key in
// `schema_migrations`.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// applyMigrations runs every migration in migrationsFS not yet recorded in
// `schema_migrations`, under a session `pg_advisory_lock` so concurrent
// New() calls across replicas don't race. The shared runner lives in
// internal/persistence/sqlmigrate; the advisory-lock name is stable per
// subsystem so the FNV-derived key never collides with another subsystem's
// migration lock.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	return sqlmigrate.RunPostgres(ctx, db, migrationsFS, "rollups/postgres", "harbor-rollup-migrations")
}
