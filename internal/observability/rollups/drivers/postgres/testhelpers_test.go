package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver
)

const (
	// pgDSNEnv is the CI-provided Postgres DSN. CI runs a postgres:16
	// service container and pipes HARBOR_PG_DSN to the test run; locally
	// without Postgres the DSN-dependent tests skip cleanly.
	pgDSNEnv = "HARBOR_PG_DSN"

	// skipNoDSN is the skip reason when HARBOR_PG_DSN is unset.
	skipNoDSN = "HARBOR_PG_DSN not set; skipping Postgres rollups tests (CI provides a postgres:16 service container)"

	// defaultTestTimeout is the deadline applied to per-test admin
	// connections (CREATE / DROP SCHEMA, TRUNCATE, ANALYZE).
	defaultTestTimeout = 30 * time.Second
)

// requireDSN returns the DSN from the environment or skips the test
// cleanly. CI sets the var; local dev without Postgres trips a Skip.
func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skip(skipNoDSN)
	}
	return dsn
}

// freshSchema creates a per-test Postgres schema, returns a DSN that pins
// `search_path` to it (so all driver queries hit the test schema only),
// and registers a t.Cleanup that drops the schema. This keeps concurrent
// test runs isolated even though they share a single database.
//
// We use search_path rather than schema-qualified SQL because the driver
// is shared across all three V1 deployment shapes and the reference
// implementation uses the same query shapes.
func freshSchema(t *testing.T, baseDSN string) string {
	t.Helper()
	schema := "harbor_rollups_test_" + randSuffix(t)

	adminDB, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("admin sql.Open: %v", err)
	}
	defer func() { _ = adminDB.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s", quoteIdent(schema))); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		dropDB, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Logf("cleanup sql.Open: %v", err)
			return
		}
		defer func() { _ = dropDB.Close() }()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), defaultTestTimeout)
		defer dropCancel()
		if _, err := dropDB.ExecContext(dropCtx, fmt.Sprintf("DROP SCHEMA %s CASCADE", quoteIdent(schema))); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	return appendSearchPath(baseDSN, schema)
}

// randSuffix returns a 16-hex-char random suffix for schema names.
// Crypto-strong entropy keeps concurrent test runs from colliding.
func randSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// quoteIdent quotes a SQL identifier (schema name) by doubling any
// embedded double-quote characters. We construct schema names from
// known-safe inputs (a fixed prefix + hex suffix), but defense in depth
// keeps a stray test name from doubling as an injection.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// appendSearchPath returns dsn with `search_path` set to schema. The
// driver-side connection pool will apply this on every fresh connection.
// Both URL-form and key-value-form DSNs are supported.
func appendSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn + " search_path=" + schema
		}
		q := u.Query()
		opts := q.Get("options")
		add := "-c search_path=" + schema
		if opts == "" {
			q.Set("options", add)
		} else {
			q.Set("options", opts+" "+add)
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " options='-c search_path=" + schema + "'"
}

// resetStore wipes the rollups tables between conformance subtests so each
// subtest sees a clean slate without paying the CREATE/DROP SCHEMA cost.
// The checkpoint row is reset to 0 (the driver reads a missing row as 0
// defensively, and ApplyBatch re-seeds it on the first advance).
func resetStore(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("resetStore sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if _, err := db.ExecContext(ctx, "TRUNCATE rollup_rows, rollup_fence"); err != nil {
		// Tables may not exist yet on the very first call (the driver
		// creates them via migrations); ignore "does not exist" errors.
		if !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("resetStore truncate: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, "UPDATE rollup_checkpoint SET sequence = 0 WHERE id = 1"); err != nil {
		t.Fatalf("resetStore checkpoint: %v", err)
	}
}
