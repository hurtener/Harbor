package localdb

import (
	"context"
	"database/sql"
	"embed"

	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

// migrationsFS holds the forward-only SQL migrations bundled into the
// binary (`NNNN_<slug>.sql`, applied in order). The bundling keeps the
// driver self-contained — no external migrate tool.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate applies any forward-only migrations not yet recorded in
// `schema_migrations` via the shared runner (internal/persistence/sqlmigrate).
func migrate(ctx context.Context, db *sql.DB) error {
	return sqlmigrate.RunSQLite(ctx, db, migrationsFS, "skills/localdb")
}
