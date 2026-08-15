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

// runMigrations applies or read-only verifies the embedded migration ledger.
// Apply mode takes the subsystem-stable session advisory lock; verify mode
// performs no DDL, transaction, or lock and is safe through transaction pools.
func runMigrations(ctx context.Context, db *sql.DB, mode sqlmigrate.Mode) error {
	return sqlmigrate.RunPostgres(ctx, db, migrationsFS, "rollups/postgres", "harbor-rollup-migrations", mode)
}
