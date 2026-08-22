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
		Subsystem:      "sessions.turns",
		RequiredTables: []string{"turn_rows", "turn_sessions"},
		RequiredColumns: map[string][]string{
			"turn_rows":     {"tenant_id", "user_id", "session_id", "turn_id", "effective_agent_id", "sequence", "sealed", "version", "last_applied_event_seq", "row_json"},
			"turn_sessions": {"tenant_id", "user_id", "session_id", "next_seq", "checkpoint", "snapshot", "truncated", "fenced"},
		},
	}, "turns/postgres", "harbor-turns-migrations", dsn, mode)
}
