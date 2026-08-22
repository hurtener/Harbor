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

// runMigrations applies or read-only verifies the embedded migration ledger.
// Apply mode takes the subsystem-stable session advisory lock; verify mode
// performs no DDL, transaction, or lock and is safe through transaction pools.
func runMigrations(ctx context.Context, db *sql.DB, dsn string, mode sqlmigrate.Mode) error {
	return sqlmigrate.RunPostgresNamed(ctx, db, migrationsFS, sqlmigrate.PostgresMigrationSpec{
		Subsystem:      "memory",
		RequiredTables: []string{"memory_state"},
		RequiredColumns: map[string][]string{
			"memory_state": {"tenant_id", "user_id", "session_id", "run_id", "kind", "strategy", "bytes", "updated_at"},
		},
	}, "memory/postgres", "harbor-memory-migrations", dsn, mode)
}
