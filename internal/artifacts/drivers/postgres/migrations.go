package postgres

import (
	"context"
	"database/sql"
	"embed"

	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

// migrationsFS embeds the forward-only SQL migration files
// (`NNNN_slug.sql`; the 4-digit version keys `schema_migrations`).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// applyMigrations runs every migration not yet recorded in
// `schema_migrations` under a session `pg_advisory_lock`, via the shared
// runner (internal/persistence/sqlmigrate). The advisory-lock name is
// stable + subsystem-specific so the FNV-derived key never collides.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	return sqlmigrate.RunPostgres(ctx, db, migrationsFS, "artifacts/postgres", "harbor-artifacts-migrations")
}
