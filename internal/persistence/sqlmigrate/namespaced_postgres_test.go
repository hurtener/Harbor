package sqlmigrate_test

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

//go:embed testdata/namespaced/*.sql
var namespacedMigrations embed.FS

func TestRunPostgresNamed_VerifyRejectsStateLedgerForMemory(t *testing.T) {
	base := os.Getenv("HARBOR_PG_DSN")
	if base == "" {
		t.Skip("HARBOR_PG_DSN not set; skipping namespaced Postgres migration test")
	}
	dsn, schema := namespacedTestSchema(t, base)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()); INSERT INTO schema_migrations(version) VALUES (1); CREATE TABLE state_records (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("seed wrong legacy schema: %v", err)
	}
	err = sqlmigrate.RunPostgresNamed(ctx, db, namespacedMigrations, sqlmigrate.PostgresMigrationSpec{
		Subsystem:      "memory",
		RequiredTables: []string{"memory_state"},
	}, "memory/postgres", "harbor-memory-migrations", dsn, sqlmigrate.ModeVerify)
	if err == nil {
		t.Fatal("verify accepted state ledger/state_records as memory schema")
	}
	message := err.Error()
	for _, want := range []string{"memory", "state_records", "memory_state", "remediation"} {
		if !strings.Contains(message, want) {
			t.Fatalf("verify error %q does not name %q", message, want)
		}
	}
	_ = schema
}

func TestValidateDirectMigrationDSN_RejectsTransactionPool(t *testing.T) {
	for _, dsn := range []string{
		"postgres://harbor:secret@example.test:6432/harbor",
		"host=example.test port=6432 dbname=harbor",
		"host=example.test port = 6432 dbname=harbor",
		"postgres://harbor:secret@example.test:5432/harbor?pool_mode=transaction",
	} {
		if err := sqlmigrate.ValidateDirectMigrationDSN(dsn); err == nil {
			t.Errorf("ValidateDirectMigrationDSN(%q) accepted unsafe transaction pool", dsn)
		}
	}
}

func namespacedTestSchema(t *testing.T, base string) (string, string) {
	t.Helper()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin postgres: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if err := admin.PingContext(context.Background()); err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	schema := fmt.Sprintf("ha_namespaced_%d", os.Getpid())
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})
	if strings.HasPrefix(base, "postgres://") || strings.HasPrefix(base, "postgresql://") {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatalf("parse DSN: %v", err)
		}
		q := u.Query()
		q.Set("options", "-c search_path="+schema)
		u.RawQuery = q.Encode()
		return u.String(), schema
	}
	return base + " options='-c search_path=" + schema + "'", schema
}
