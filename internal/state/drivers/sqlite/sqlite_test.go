package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/conformancetest"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// identityTripleForTest builds a fully-populated identity triple
// with a non-empty RunID — sufficient for any state.StateStore
// boundary check (ValidateIdentity, ValidateRecord).
func identityTripleForTest() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "tenant-A",
			UserID:    "user-1",
			SessionID: "sess-1",
		},
		RunID: "run-1",
	}
}

// TestSQLite_Conformance_TempDirFile drives the canonical conformance
// suite (Phase 07's `internal/state/conformancetest.Run`) against a
// fresh tempdir-backed SQLite database. Each top-level subtest gets
// its own database file so subtests are independent.
func TestSQLite_Conformance_TempDirFile(t *testing.T) {
	conformancetest.Run(t, func() (state.StateStore, func()) {
		dir := t.TempDir()
		dsn := filepath.Join(dir, "harbor-state.sqlite")
		s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
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
	conformancetest.Run(t, func() (state.StateStore, func()) {
		s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: ":memory:"})
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
	cfg := config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "registered.sqlite")}
	s, err := state.OpenDriver("sqlite", cfg)
	if err != nil {
		t.Fatalf("OpenDriver(sqlite): %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	registered := state.RegisteredDrivers()
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
	_, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: ""})
	if err == nil {
		t.Fatal("sqlite.New with empty DSN: err=nil, want error")
	}
}

// TestSQLite_Close_Idempotent — repeat Close calls are safe and
// return nil after the first.
func TestSQLite_Close_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "close.sqlite")})
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

// TestSQLite_LoadAfterClose returns ErrStoreClosed for every method —
// not just Save (which the conformance suite covers).
func TestSQLite_LoadAfterClose(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "after-close.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	tripleA := identityTripleForTest()

	if _, err := s.Load(context.Background(), tripleA, "k"); !errors.Is(err, state.ErrStoreClosed) {
		t.Errorf("Load: err=%v, want ErrStoreClosed", err)
	}
	if _, err := s.LoadByEventID(context.Background(), "ev"); !errors.Is(err, state.ErrStoreClosed) {
		t.Errorf("LoadByEventID: err=%v, want ErrStoreClosed", err)
	}
	if err := s.Delete(context.Background(), tripleA, "k"); !errors.Is(err, state.ErrStoreClosed) {
		t.Errorf("Delete: err=%v, want ErrStoreClosed", err)
	}
}

// TestSQLite_New_AcceptsFileURI exercises the `file:` URI DSN form —
// modernc.org/sqlite supports it natively and our DSN augmentation
// must layer `_pragma` + `_txlock` query params onto the existing
// query string instead of overwriting it.
func TestSQLite_New_AcceptsFileURI(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "uri.sqlite") + "?cache=shared"
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("sqlite.New(%q): %v", dsn, err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	q := identityTripleForTest()
	rec := state.StateRecord{
		ID:       "01HABXX-uri-0001",
		Identity: q,
		Kind:     "session.lifecycle",
		Bytes:    []byte("uri-payload"),
	}
	if saveErr := s.Save(context.Background(), rec); saveErr != nil {
		t.Fatalf("Save against file: URI: %v", saveErr)
	}
	got, loadErr := s.Load(context.Background(), q, "session.lifecycle")
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if string(got.Bytes) != "uri-payload" {
		t.Errorf("file: URI round-trip failed: got %q", got.Bytes)
	}
}

// TestSQLite_New_AcceptsBarePathWithQuery covers the niche bare-path
// DSN that already has a `?` (e.g., a caller hand-rolled a DSN with
// `?cache=shared` before passing it through). Our augmentation must
// append with `&` rather than `?` to avoid producing an invalid DSN.
func TestSQLite_New_AcceptsBarePathWithQuery(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "with-query.sqlite") + "?_journal=wal2"
	// `_journal=wal2` is invalid; we expect modernc.org/sqlite to
	// either ignore unrecognized params or fail. The point of THIS
	// test is to assert our augmentation produces a syntactically
	// valid string (`?…&_pragma=…`) — we only assert sql.Open
	// completes WITHOUT a parse error in the augmentation layer.
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		// Any error is fine as long as it is NOT our augmentation
		// layer's URL parse error — that would indicate we mangled
		// the DSN ourselves.
		if err.Error() != "" && contains(err.Error(), "augment DSN") {
			t.Fatalf("augment DSN failed unexpectedly: %v", err)
		}
		return
	}
	_ = s.Close(context.Background())
}

// contains is a tiny helper to keep the assertion above readable.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && stringIndex(s, sub) >= 0
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestSQLite_Save_IdempotentNoOp_CommitPath exercises the
// "EventID seen before AND identical bytes/version" branch — the
// idempotent no-op needs to commit the (empty) tx so the connection
// is returned to the pool. Without exercising it explicitly, the
// branch shows up in coverage reports but only via the conformance
// suite's `Save_Idempotent_SameIDSameContent`. This pins the contract
// at the file level.
func TestSQLite_Save_IdempotentNoOp_CommitPath(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "noop-commit.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	q := identityTripleForTest()
	rec := state.StateRecord{
		ID:       "01HABXX-noop",
		Identity: q,
		Kind:     "task.checkpoint",
		Bytes:    []byte("payload"),
		Version:  1,
	}
	if saveErr := s.Save(context.Background(), rec); saveErr != nil {
		t.Fatalf("first Save: %v", saveErr)
	}
	// Run the identical Save four times in a row; each must succeed
	// (idempotent) and must NOT leak the connection — with
	// MaxOpenConns(1), a leaked tx would deadlock the next call.
	for i := range 4 {
		if saveErr := s.Save(context.Background(), rec); saveErr != nil {
			t.Fatalf("idempotent Save %d: %v", i+2, saveErr)
		}
	}
	// And subsequent unrelated work must still go through.
	got, loadErr := s.Load(context.Background(), q, "task.checkpoint")
	if loadErr != nil {
		t.Fatalf("Load after idempotent burst: %v", loadErr)
	}
	if string(got.Bytes) != "payload" {
		t.Errorf("Load returned %q; idempotent burst corrupted slot", got.Bytes)
	}
}

// TestSQLite_Save_RespectsExplicitUpdatedAt — when the caller sets
// UpdatedAt explicitly (controllable-clock pattern), the driver must
// honor it rather than overwriting with `time.Now()`.
func TestSQLite_Save_RespectsExplicitUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "updatedat.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	q := identityTripleForTest()
	pinned := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	rec := state.StateRecord{
		ID:        "01HABXX-explicit-ts",
		Identity:  q,
		Kind:      "task.checkpoint",
		Bytes:     []byte("p"),
		UpdatedAt: pinned,
	}
	if saveErr := s.Save(context.Background(), rec); saveErr != nil {
		t.Fatal(saveErr)
	}
	got, loadErr := s.Load(context.Background(), q, "task.checkpoint")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !got.UpdatedAt.Equal(pinned) {
		t.Errorf("UpdatedAt round-trip: got %v, want %v", got.UpdatedAt, pinned)
	}
}

// TestSQLite_New_FailsOnNonExistentDirectory — when the DSN's parent
// directory does not exist, modernc.org/sqlite cannot create the
// database file. Surface the failure with a clear "migrate" error
// rather than silently returning a half-initialized driver.
func TestSQLite_New_FailsOnNonExistentDirectory(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "definitely", "not", "a", "real", "dir", "x.sqlite")
	_, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err == nil {
		t.Fatal("expected error opening DB in non-existent directory")
	}
}

// TestSQLite_Save_EmptyKindRejected — Save validates Kind upstream of
// the SQL boundary; ValidateRecord returns ErrInvalidRecord for an
// empty Kind.
func TestSQLite_Save_EmptyKindRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "empty-kind.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	rec := state.StateRecord{
		ID:       "01HABXX-empty-kind",
		Identity: identityTripleForTest(),
		Kind:     "", // intentionally empty
		Bytes:    []byte("p"),
	}
	if err := s.Save(context.Background(), rec); !errors.Is(err, state.ErrInvalidRecord) {
		t.Errorf("Save with empty Kind: err=%v, want ErrInvalidRecord", err)
	}
}

// TestSQLite_Load_EmptyKindRejected — Load also rejects empty Kind
// at the boundary, before issuing the SELECT.
func TestSQLite_Load_EmptyKindRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "load-empty-kind.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	if _, err := s.Load(context.Background(), identityTripleForTest(), ""); !errors.Is(err, state.ErrInvalidRecord) {
		t.Errorf("Load with empty Kind: err=%v, want ErrInvalidRecord", err)
	}
}

// TestSQLite_LoadByEventID_EmptyRejected — LoadByEventID rejects an
// empty EventID at the boundary, before the SELECT.
func TestSQLite_LoadByEventID_EmptyRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "byid-empty.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	if _, err := s.LoadByEventID(context.Background(), ""); !errors.Is(err, state.ErrInvalidRecord) {
		t.Errorf("LoadByEventID with empty EventID: err=%v, want ErrInvalidRecord", err)
	}
}

// TestSQLite_Delete_EmptyKindRejected — Delete rejects empty Kind.
func TestSQLite_Delete_EmptyKindRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "del-empty-kind.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	if err := s.Delete(context.Background(), identityTripleForTest(), ""); !errors.Is(err, state.ErrInvalidRecord) {
		t.Errorf("Delete with empty Kind: err=%v, want ErrInvalidRecord", err)
	}
}

// TestSQLite_Save_BadIdentityRejected — Save rejects an incomplete
// identity triple before reaching SQL.
func TestSQLite_Save_BadIdentityRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "bad-id.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	rec := state.StateRecord{
		ID:       "01HABXX-bad-id",
		Identity: identity.Quadruple{}, // empty
		Kind:     "k",
		Bytes:    []byte("p"),
	}
	if err := s.Save(context.Background(), rec); !errors.Is(err, state.ErrIdentityRequired) {
		t.Errorf("Save with empty identity: err=%v, want ErrIdentityRequired", err)
	}
}

// TestSQLite_Delete_BadIdentityRejected mirrors the Save case.
func TestSQLite_Delete_BadIdentityRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: filepath.Join(dir, "del-bad-id.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	if err := s.Delete(context.Background(), identity.Quadruple{}, "k"); !errors.Is(err, state.ErrIdentityRequired) {
		t.Errorf("Delete with empty identity: err=%v, want ErrIdentityRequired", err)
	}
}
