package postgres_test

// Shared helpers for the Postgres turns.Store driver tests.
//
// Skip-clean without HARBOR_PG_DSN: every Postgres-touching test uses
// `requireDSN(t)` which `t.Skip`s when the env var is unset. CI's
// postgres:16 job sets HARBOR_PG_DSN so the shared conformance suite
// actually exercises the driver there. The env-gated pattern mirrors
// the state / memory / skills / artifacts Postgres driver tests.

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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/identity"
)

const (
	pgDSNEnv           = "HARBOR_PG_DSN"
	skipNoDSN          = "HARBOR_PG_DSN not set; skipping postgres turns conformance — the postgres:16 CI job sets it (see .github/workflows/ci.yml)"
	defaultTestTimeout = 30 * time.Second
)

// requireDSN returns the DSN from the environment or skips the test
// cleanly.
func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skip(skipNoDSN)
	}
	return dsn
}

// freshSchema creates a per-test Postgres schema, returns a DSN that
// pins `search_path` to it, and registers a t.Cleanup that drops the
// schema. Mirrors the state/memory postgres helpers so test isolation
// is consistent across persistence-triad subsystems.
func freshSchema(t *testing.T, baseDSN string) string {
	t.Helper()
	schema := "harbor_turntest_" + randSuffix(t)

	adminDB, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("admin sql.Open: %v", err)
	}
	defer func() { _ = adminDB.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if _, err := adminDB.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA %s", quoteIdent(schema)),
	); err != nil {
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
		if _, err := dropDB.ExecContext(dropCtx,
			fmt.Sprintf("DROP SCHEMA %s CASCADE", quoteIdent(schema)),
		); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	return appendSearchPath(baseDSN, schema)
}

// randSuffix returns a 16-hex-char random suffix for schema names.
func randSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// quoteIdent quotes a SQL identifier (schema name).
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// appendSearchPath returns dsn with `search_path` set to schema.
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

// truncateTurns wipes the driver's tables between conformance
// subtests so each subtest sees a clean slate without paying the
// CREATE/DROP SCHEMA cost.
func truncateTurns(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("truncateTurns sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	for _, tbl := range []string{"turn_rows", "turn_sessions"} {
		if _, err := db.ExecContext(ctx, "TRUNCATE TABLE "+tbl); err != nil {
			// Tables may not exist yet on the very first call (the
			// first driver New() creates them via migrations); ignore
			// "does not exist" errors and let the next call retry.
			if !strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("truncate %s: %v", tbl, err)
			}
		}
	}
}

// fixtureID is a stable identity triple for driver-scoped tests.
var fixtureID = identity.Identity{
	TenantID:  "t-turns-pg",
	UserID:    "u-turns-pg",
	SessionID: "s-turns-pg",
}
