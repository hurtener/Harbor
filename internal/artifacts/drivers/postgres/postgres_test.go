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

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/conformancetest"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/postgres"
	"github.com/hurtener/Harbor/internal/config"
)

const (
	pgDSNEnv  = "HARBOR_PG_DSN"
	skipNoDSN = "HARBOR_PG_DSN not set; skipping postgres conformance — see docs/plans/phase-18-artifacts-blob.md"
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
// database. Mirrors the Phase 16 StateStore test helper.
func freshSchema(t *testing.T, baseDSN string) string {
	t.Helper()
	suffix := randSuffix(t)
	schema := "harbor_artifacts_test_" + suffix

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
// embedded double-quote characters.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// appendSearchPath returns dsn with `search_path` set to schema. Both
// URL-form and key-value-form DSNs are supported.
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

// TestPostgres_Conformance runs the canonical artifacts.ArtifactStore
// conformance suite against a Postgres connection. The test gates on
// HARBOR_PG_DSN: locally without Postgres available the test skips
// cleanly; CI provides a postgres:16 service container.
func TestPostgres_Conformance(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	conformancetest.Run(t, func() (artifacts.ArtifactStore, func()) {
		s, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatalf("postgres.New: %v", err)
		}
		truncateAll(t, dsn)
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// truncateAll wipes the artifacts_blobs table between conformance
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
	if _, err := db.ExecContext(ctx, "TRUNCATE TABLE artifacts_blobs"); err != nil {
		// Table may not exist yet on the very first call; ignore "does
		// not exist" errors and let the next call retry.
		if !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("truncate artifacts_blobs: %v", err)
		}
	}
}

// TestPostgres_DriverRegistered verifies the init() side-effect: the
// driver self-registers under "postgres" so OpenDriver can resolve.
// Registry-only — does not open a real connection.
func TestPostgres_DriverRegistered(t *testing.T) {
	cfg := config.ArtifactsConfig{Driver: "postgres", DSN: ""}
	_, err := artifacts.OpenDriver("postgres", cfg)
	if err == nil {
		t.Fatalf("OpenDriver: expected DSN error, got nil")
	}
	if errors.Is(err, artifacts.ErrUnknownDriver) {
		t.Fatalf("driver not registered: %v", err)
	}
}

// TestPostgres_New_RequiresDSN pins the explicit-DSN-required
// contract. Empty DSN must surface a clear error rather than panic.
func TestPostgres_New_RequiresDSN(t *testing.T) {
	_, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: ""})
	if err == nil {
		t.Fatalf("expected error on empty DSN")
	}
	if !strings.Contains(err.Error(), "DSN") {
		t.Errorf("error should mention DSN; got: %v", err)
	}
}

// TestPostgres_Close_Idempotent — repeat Close calls are safe and
// return nil after the first.
func TestPostgres_Close_Idempotent(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("second Close (should be no-op): %v", err)
	}
}

// TestPostgres_AllMethodsAfterClose — every method returns
// ErrStoreClosed after Close.
func TestPostgres_AllMethodsAfterClose(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	scope := artifacts.ArtifactScope{
		TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1", TaskID: "task-1",
	}
	ctx := context.Background()

	if _, _, err := s.GetRef(ctx, scope, "ns_deadbeef0000"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("GetRef: err=%v, want ErrStoreClosed", err)
	}
	if _, err := s.Exists(ctx, scope, "ns_deadbeef0000"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Exists: err=%v, want ErrStoreClosed", err)
	}
	if _, err := s.Delete(ctx, scope, "ns_deadbeef0000"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Delete: err=%v, want ErrStoreClosed", err)
	}
}

// TestPostgres_PutSource_RoundTrips — Source is JSON-encoded to
// source_json on Put and decoded on GetRef.
func TestPostgres_PutSource_RoundTrips(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(config.ArtifactsConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	ctx := context.Background()
	scope := artifacts.ArtifactScope{
		TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K",
	}
	src := map[string]any{
		"tool":     "echo",
		"version":  "1.2.3",
		"args_n":   float64(3),
		"absolute": true,
	}
	ref, err := s.PutBytes(ctx, scope, []byte("source-payload"),
		artifacts.PutOpts{Namespace: "ns.src", Source: src})
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := s.GetRef(ctx, scope, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("GetRef found=false")
	}
	if got.Source["tool"] != "echo" {
		t.Errorf("Source[tool]=%v, want %q", got.Source["tool"], "echo")
	}
}
