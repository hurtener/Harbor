package sqlite

import (
	"context"
	"database/sql"
	"embed"

	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

// migrationsFS holds the forward-only SQL migrations bundled into the
// binary. Each file is named `NNNN_<slug>.sql` (zero-padded numeric
// version) and is applied in lexicographic order via the shared runner
// (internal/persistence/sqlmigrate — one home for the filename
// contract, the partial-apply precheck, and the per-migration
// transaction). The bundling keeps the driver self-contained — no
// external migrate tool (RFC §10).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate applies any forward-only migrations whose version is not
// already present in `schema_migrations`. The runner lives in the
// shared internal/persistence/sqlmigrate package; this driver supplies
// its embedded migrations + error prefix.
func migrate(ctx context.Context, db *sql.DB) error {
	return sqlmigrate.RunSQLite(ctx, db, migrationsFS, "sessions/turns/sqlite")
}
