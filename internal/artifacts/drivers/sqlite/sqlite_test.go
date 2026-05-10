package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/conformancetest"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/config"
)

// scopeForTest returns a fully-populated ArtifactScope suitable for
// boundary checks (Validate / Put*).
func scopeForTest() artifacts.ArtifactScope {
	return artifacts.ArtifactScope{
		TenantID:  "tenant-A",
		UserID:    "user-1",
		SessionID: "sess-1",
		TaskID:    "task-1",
	}
}

// TestSQLite_Conformance_TempDirFile drives the canonical conformance
// suite against a fresh tempdir-backed SQLite database. Each top-level
// subtest gets its own database file so subtests are independent.
func TestSQLite_Conformance_TempDirFile(t *testing.T) {
	conformancetest.Run(t, func() (artifacts.ArtifactStore, func()) {
		dir := t.TempDir()
		dsn := filepath.Join(dir, "harbor-artifacts.sqlite")
		s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatalf("sqlite.New(%q): %v", dsn, err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestSQLite_Conformance_InMemory exercises the same suite against
// the `:memory:` DSN — the degenerate dev case. Useful in CI because
// it removes the disk seek path from the test budget while still
// exercising the full SQL stack.
func TestSQLite_Conformance_InMemory(t *testing.T) {
	conformancetest.Run(t, func() (artifacts.ArtifactStore, func()) {
		s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: ":memory:"})
		if err != nil {
			t.Fatalf("sqlite.New(:memory:): %v", err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestSQLite_DriverRegistered verifies the init() side-effect — the
// driver self-registers under "sqlite" so OpenDriver can resolve.
func TestSQLite_DriverRegistered(t *testing.T) {
	dir := t.TempDir()
	cfg := config.ArtifactsConfig{Driver: "sqlite", DSN: filepath.Join(dir, "registered.sqlite")}
	s, err := artifacts.OpenDriver("sqlite", cfg)
	if err != nil {
		t.Fatalf("OpenDriver(sqlite): %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	registered := artifacts.RegisteredDrivers()
	found := false
	for _, n := range registered {
		if n == "sqlite" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("RegisteredDrivers()=%v, expected to include \"sqlite\"", registered)
	}
}

// TestSQLite_New_RejectsEmptyDSN locks in the no-silent-degradation
// rule: an empty DSN is a misconfiguration, not a default-fallback
// invitation (AGENTS.md §13).
func TestSQLite_New_RejectsEmptyDSN(t *testing.T) {
	_, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: ""})
	if err == nil {
		t.Fatal("sqlite.New with empty DSN: err=nil, want error")
	}
}

// TestSQLite_Close_Idempotent — repeat Close calls are safe and
// return nil after the first.
func TestSQLite_Close_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: filepath.Join(dir, "close.sqlite")})
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

// TestSQLite_AllMethodsAfterClose — every method returns
// ErrStoreClosed after Close. The conformance suite covers the
// PutBytes / Get / List paths; this pins GetRef / Exists / Delete.
func TestSQLite_AllMethodsAfterClose(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: filepath.Join(dir, "after-close.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := scopeForTest()

	if _, _, err := s.GetRef(ctx, scope, "ns_deadbeef0000"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("GetRef: err=%v, want ErrStoreClosed", err)
	}
	if _, err := s.Exists(ctx, scope, "ns_deadbeef0000"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Exists: err=%v, want ErrStoreClosed", err)
	}
	if _, err := s.Delete(ctx, scope, "ns_deadbeef0000"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Delete: err=%v, want ErrStoreClosed", err)
	}
	if _, err := s.PutText(ctx, scope, "x", artifacts.PutOpts{Namespace: "ns"}); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("PutText: err=%v, want ErrStoreClosed", err)
	}
}

// TestSQLite_New_AcceptsFileURI exercises the `file:` URI DSN form —
// modernc.org/sqlite supports it natively and our DSN augmentation
// must layer `_pragma` + `_txlock` query params onto the existing
// query string instead of overwriting it.
func TestSQLite_New_AcceptsFileURI(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "uri.sqlite") + "?cache=shared"
	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("sqlite.New(%q): %v", dsn, err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	scope := scopeForTest()
	ref, err := s.PutBytes(context.Background(), scope, []byte("uri-payload"),
		artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatalf("Put against file: URI: %v", err)
	}
	got, found, err := s.Get(context.Background(), scope, ref.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || string(got) != "uri-payload" {
		t.Errorf("file: URI round-trip failed: got=%q, found=%v", got, found)
	}
}

// TestSQLite_PutBytes_BadIdentityRejected — PutBytes rejects an
// incomplete identity scope before reaching SQL.
func TestSQLite_PutBytes_BadIdentityRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: filepath.Join(dir, "bad-id.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	if _, err := s.PutBytes(context.Background(),
		artifacts.ArtifactScope{}, []byte("x"),
		artifacts.PutOpts{Namespace: "ns"}); !errors.Is(err, artifacts.ErrIdentityRequired) {
		t.Errorf("PutBytes with empty scope: err=%v, want ErrIdentityRequired", err)
	}
}

// TestSQLite_New_FailsOnNonExistentDirectory — when the DSN's parent
// directory does not exist, modernc.org/sqlite cannot create the
// database file. Surface the failure rather than silently returning a
// half-initialized driver.
func TestSQLite_New_FailsOnNonExistentDirectory(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "definitely", "not", "a", "real", "dir", "x.sqlite")
	_, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: dsn})
	if err == nil {
		t.Fatal("expected error opening DB in non-existent directory")
	}
}

// TestSQLite_PutSource_RoundTrips — Source is JSON-encoded to
// source_json on Put and decoded on GetRef. Pin the round-trip.
func TestSQLite_PutSource_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: filepath.Join(dir, "source.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	ctx := context.Background()
	scope := scopeForTest()
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
	if got.Source["absolute"] != true {
		t.Errorf("Source[absolute]=%v, want true", got.Source["absolute"])
	}
}

// TestSQLite_PutText_DefaultsMime — PutText defaults MimeType to
// text/plain when caller leaves it empty.
func TestSQLite_PutText_DefaultsMime(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: filepath.Join(dir, "text.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	ctx := context.Background()
	ref, err := s.PutText(ctx, scopeForTest(), "hello", artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.MimeType != "text/plain; charset=utf-8" {
		t.Errorf("MimeType=%q, want %q", ref.MimeType, "text/plain; charset=utf-8")
	}
}

// TestSQLite_Put_NonEncodableSourceFailsLoud — non-encodable Source
// values must surface a marshal error rather than silently dropping
// the field. Per Phase 17's documented behavior + AGENTS.md §5
// (fail loudly).
func TestSQLite_Put_NonEncodableSourceFailsLoud(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.ArtifactsConfig{Driver: "sqlite", DSN: filepath.Join(dir, "bad-src.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	// Channels are not JSON-encodable.
	src := map[string]any{"chan": make(chan int)}
	_, err = s.PutBytes(context.Background(), scopeForTest(), []byte("p"),
		artifacts.PutOpts{Namespace: "ns", Source: src})
	if err == nil {
		t.Fatal("expected marshal error for non-encodable Source value")
	}
}
