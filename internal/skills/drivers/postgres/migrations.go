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
		Subsystem:      "skills",
		RequiredTables: []string{"skills", "installed_packages", "installed_package_supports"},
		RequiredColumns: map[string][]string{
			"skills":                     {"tenant_id", "user_id", "session_id", "scope", "agent_id", "name", "title", "description", "trigger_text", "task_type", "tags_json", "tags_text", "steps_json", "preconditions_json", "failure_modes_json", "required_tools_json", "required_ns_json", "required_tags_json", "origin", "origin_ref", "scope_tenant", "scope_project", "content_hash", "created_at", "updated_at", "last_used", "use_count", "extra_json", "search_tsv"},
			"installed_packages":         {"tenant_id", "user_id", "agent_id", "name", "package_hash", "package_version", "origin", "skill_json", "canonical", "created_at", "updated_at"},
			"installed_package_supports": {"tenant_id", "user_id", "agent_id", "name", "path", "mime", "size", "digest", "data"},
		},
	}, "skills/postgres", "harbor-skills-migrations", dsn, mode)
}
