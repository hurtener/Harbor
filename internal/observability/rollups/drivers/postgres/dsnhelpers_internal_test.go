package postgres

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
)

// Internal-package DSN helpers for the EXPLAIN/index tests. The external
// package (postgres_test) carries its own copy; Go test files cannot share
// helpers across the internal/external package boundary.

const (
	internalPGDSNEnv = "HARBOR_PG_DSN"

	internalSkipNoDSN = "HARBOR_PG_DSN not set; skipping Postgres rollups EXPLAIN test (CI provides a postgres:16 service container)"

	internalTestTimeout = 30 * time.Second
)

// requireDSN returns the DSN from the environment or skips the test
// cleanly.
func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(internalPGDSNEnv)
	if dsn == "" {
		t.Skip(internalSkipNoDSN)
	}
	return dsn
}

// freshSchema creates a per-test Postgres schema and returns a DSN that
// pins `search_path` to it, registering a cleanup that drops the schema.
func freshSchema(t *testing.T, baseDSN string) string {
	t.Helper()
	schema := "harbor_rollups_test_" + internalRandSuffix(t)

	adminDB, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("admin sql.Open: %v", err)
	}
	defer func() { _ = adminDB.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), internalTestTimeout)
	defer cancel()
	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s", internalQuoteIdent(schema))); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		dropDB, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Logf("cleanup sql.Open: %v", err)
			return
		}
		defer func() { _ = dropDB.Close() }()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), internalTestTimeout)
		defer dropCancel()
		if _, err := dropDB.ExecContext(dropCtx, fmt.Sprintf("DROP SCHEMA %s CASCADE", internalQuoteIdent(schema))); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	return internalAppendSearchPath(baseDSN, schema)
}

func internalRandSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func internalQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func internalAppendSearchPath(dsn, schema string) string {
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
