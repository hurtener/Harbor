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
		Subsystem:      "observability.rollups",
		RequiredTables: []string{"rollup_rows", "rollup_checkpoint", "rollup_fence"},
		RequiredColumns: map[string][]string{
			"rollup_rows":       {"bucket_start", "tenant_id", "user_id", "session_id", "model", "llm_completions", "llm_tokens_prompt", "llm_tokens_completion", "llm_tokens_reasoning", "llm_tokens_cache_read", "llm_tokens_cache_write", "llm_tokens_total", "llm_cost_micros", "llm_latency_count", "llm_latency_sum_ms", "llm_latency_min_ms", "llm_latency_max_ms", "tasks_completed", "tasks_failed", "tasks_cancelled"},
			"rollup_checkpoint": {"id", "sequence"},
			"rollup_fence":      {"tenant_id", "user_id", "session_id"},
		},
	}, "rollups/postgres", "harbor-rollup-migrations", dsn, mode)
}
