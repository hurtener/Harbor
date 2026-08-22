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
func runMigrations(ctx context.Context, db *sql.DB, dsn string, mode sqlmigrate.Mode) error {
	return sqlmigrate.RunPostgresNamed(ctx, db, migrationsFS, sqlmigrate.PostgresMigrationSpec{
		Subsystem:      "state",
		RequiredTables: []string{"state_records"},
		RequiredColumns: map[string][]string{
			"state_records": {"tenant_id", "user_id", "session_id", "run_id", "kind", "event_id", "version", "bytes", "updated_at"},
		},
	}, "postgres", "harbor-state-migrations", dsn, mode)
}
