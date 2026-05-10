package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/conformancetest"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

const (
	pgDSNEnv  = "HARBOR_PG_DSN"
	skipNoDSN = "HARBOR_PG_DSN not set; skipping postgres conformance — see docs/plans/phase-16-state-postgres.md"
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

// freshSchema creates a per-test Postgres schema, returns a DSN that
// pins `search_path` to it (so all driver queries hit the test schema
// only), and registers a t.Cleanup that drops the schema. This keeps
// concurrent test runs isolated even though they share a single
// database.
//
// We use search_path rather than rewriting every query because the
// driver is shared across all three V1 deployment shapes (in-mem,
// SQLite, Postgres) and the inmem reference uses the same query
// shapes — adding schema-qualified names to the SQL would diverge
// per-driver and complicate the conformance gate.
func freshSchema(t *testing.T, baseDSN string) string {
	t.Helper()
	suffix := randSuffix(t)
	schema := "harbor_test_" + suffix

	// Use a one-shot admin connection to create the schema.
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
// known-safe inputs (a fixed prefix + hex suffix), but defense in
// depth keeps a stray test name from doubling as an injection.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// appendSearchPath returns dsn with `search_path` set to schema. The
// driver-side connection pool will apply this on every fresh
// connection.
//
// Both URL-form and key-value-form DSNs are supported. URL-form is
// canonical (per the phase plan); key-value-form is handled because
// pgx accepts both and the env var may originate either way.
func appendSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			// Fall back to key-value style append.
			return dsn + " search_path=" + schema
		}
		q := u.Query()
		// pgx accepts `search_path` in URL params via the `options`
		// key as `-c search_path=...`. Stack on top of any existing.
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
	// Key-value form: append a search_path directive.
	return dsn + " options='-c search_path=" + schema + "'"
}

// TestPostgres_Conformance runs the canonical state.StateStore
// conformance suite against a Postgres connection. The test gates on
// HARBOR_PG_DSN: locally without Postgres available the test skips
// cleanly; CI provides a postgres:16 service container.
func TestPostgres_Conformance(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	conformancetest.Run(t, func() (state.StateStore, func()) {
		// Each conformance subtest gets its own driver instance so
		// state from one subtest can't bleed into another. We share
		// a single schema across subtests and TRUNCATE between them
		// so the per-test setup cost stays bounded.
		s, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatalf("postgres.New: %v", err)
		}
		truncateAll(t, dsn)
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// truncateAll wipes the state_records table between conformance
// subtests so each subtest sees a clean slate without paying the
// CREATE/DROP SCHEMA cost.
func truncateAll(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("truncateAll sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	if _, err := db.ExecContext(ctx, "TRUNCATE TABLE state_records"); err != nil {
		// Table may not exist yet on the very first call (the test
		// driver creates it via migrations); ignore "does not exist"
		// errors and let the next call retry.
		if !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("truncate state_records: %v", err)
		}
	}
}

// TestPostgres_DriverRegistered verifies the init() side-effect: the
// driver self-registers under "postgres" so OpenDriver can resolve.
// This is a registry-only check — it does not open a real connection
// (which would require Postgres availability).
func TestPostgres_DriverRegistered(t *testing.T) {
	cfg := config.StateConfig{Driver: "postgres", DSN: ""}
	_, err := state.OpenDriver("postgres", cfg)
	// Empty DSN must error from New — that's how we know the factory
	// resolved through the registry.
	if err == nil {
		t.Fatalf("OpenDriver: expected DSN error, got nil")
	}
	if errors.Is(err, state.ErrUnknownDriver) {
		t.Fatalf("driver not registered: %v", err)
	}
}

// TestPostgres_New_RequiresDSN pins the explicit-DSN-required
// contract. Empty DSN must surface a clear error rather than panic
// inside sql.Open.
func TestPostgres_New_RequiresDSN(t *testing.T) {
	_, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: ""})
	if err == nil {
		t.Fatalf("expected error on empty DSN")
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("error should mention DSN; got: %v", err)
	}
}
