package fs_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/conformancetest"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	"github.com/hurtener/Harbor/internal/config"
)

// TestFS_Conformance drives the canonical conformance suite against
// the FS driver, using `t.TempDir()` for cfg.FSRoot. This is the
// gate Phase 18 (SQLite-blob / Postgres-blob) and Phase 19 (S3-style)
// inherit verbatim.
func TestFS_Conformance(t *testing.T) {
	conformancetest.Run(t, func() (artifacts.ArtifactStore, func()) {
		root := t.TempDir()
		s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
		if err != nil {
			t.Fatalf("fs.New: %v", err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}

func TestFS_New_RejectsEmptyRoot(t *testing.T) {
	_, err := fs.New(config.ArtifactsConfig{Driver: "fs"})
	if err == nil {
		t.Fatal("fs.New with empty FSRoot accepted; expected error")
	}
}

func TestFS_New_CreatesRootDirectory(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "missing-subdir")
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatalf("fs.New on missing directory: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
		t.Errorf("FSRoot not created: stat=%v", statErr)
	}
}

func TestFS_DriverRegistered(t *testing.T) {
	cfg := config.ArtifactsConfig{Driver: "fs", FSRoot: t.TempDir()}
	s, err := artifacts.OpenDriver("fs", cfg)
	if err != nil {
		t.Fatalf("OpenDriver fs: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
}

func TestFS_PersistsAcrossReopen(t *testing.T) {
	root := t.TempDir()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"}

	s1, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s1.PutBytes(context.Background(), scope, []byte("durable"), artifacts.PutOpts{
		Namespace: "ns",
		MimeType:  "text/plain",
		Source:    map[string]any{"tool": "echo", "n": 42},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Re-open. The index is rebuilt from disk; the ref must still be
	// resolvable, the bytes still recoverable.
	s2, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close(context.Background()) }()

	got, found, err := s2.Get(context.Background(), scope, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("Get found=false after reopen")
	}
	if string(got) != "durable" {
		t.Errorf("Get bytes=%q after reopen", got)
	}
	gotRef, found, err := s2.GetRef(context.Background(), scope, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("GetRef found=false after reopen")
	}
	if gotRef.MimeType != "text/plain" {
		t.Errorf("MimeType lost across reopen: %q", gotRef.MimeType)
	}
	if gotRef.Source["tool"] != "echo" {
		t.Errorf("Source lost across reopen: %+v", gotRef.Source)
	}
}

func TestFS_AtomicWrite_LeavesNoTmpOnSuccess(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"}
	_, err = s.PutBytes(context.Background(), scope, []byte("atomic"), artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}

	// Walk root; assert no tmp.* files remain.
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if name := info.Name(); len(name) >= len("tmp.") && name[:4] == "tmp." {
			t.Errorf("found tmp file after Put: %s", path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
}

func TestFS_StartupCleansStraysTmpFiles(t *testing.T) {
	root := t.TempDir()
	// Plant a stray tmp file in the root to simulate a crash.
	stray := filepath.Join(root, "tmp.crash-leftover")
	if err := os.WriteFile(stray, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if _, statErr := os.Stat(stray); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("startup did not clean tmp file: stat=%v", statErr)
	}
}

func TestFS_Source_NonEncodableFails(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	_, err = s.PutBytes(context.Background(), scope, []byte("x"), artifacts.PutOpts{
		Namespace: "ns",
		Source:    map[string]any{"chan": make(chan int)},
	})
	if err == nil {
		t.Fatal("expected error on non-encodable Source")
	}
}

// TestFS_EmptyID_ReturnsFoundFalse — explicit edge: empty id is
// treated as "no such artifact" rather than yielding an error.
func TestFS_EmptyID_ReturnsFoundFalse(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	got, found, err := s.Get(context.Background(), scope, "")
	if err != nil || found || got != nil {
		t.Errorf("Get empty id: got=%v found=%v err=%v", got, found, err)
	}
	gotRef, found, err := s.GetRef(context.Background(), scope, "")
	if err != nil || found || gotRef != nil {
		t.Errorf("GetRef empty id: ref=%v found=%v err=%v", gotRef, found, err)
	}
	exists, err := s.Exists(context.Background(), scope, "")
	if err != nil || exists {
		t.Errorf("Exists empty id: %v err=%v", exists, err)
	}
	existed, err := s.Delete(context.Background(), scope, "")
	if err != nil || existed {
		t.Errorf("Delete empty id: %v err=%v", existed, err)
	}
}

// TestFS_ClosedRejectsAllOps verifies every method returns
// ErrStoreClosed after Close.
func TestFS_ClosedRejectsAllOps(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx := context.Background()
	if _, err := s.PutBytes(ctx, scope, []byte("x"), artifacts.PutOpts{}); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("PutBytes: err=%v", err)
	}
	if _, err := s.PutText(ctx, scope, "x", artifacts.PutOpts{}); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("PutText: err=%v", err)
	}
	if _, _, err := s.Get(ctx, scope, "id"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Get: err=%v", err)
	}
	if _, _, err := s.GetRef(ctx, scope, "id"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("GetRef: err=%v", err)
	}
	if _, err := s.Exists(ctx, scope, "id"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Exists: err=%v", err)
	}
	if _, err := s.Delete(ctx, scope, "id"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Delete: err=%v", err)
	}
	if _, err := s.List(ctx, scope); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("List: err=%v", err)
	}
}

// TestFS_New_IgnoresNonMetaFiles verifies the rebuildIndex walk
// skips files that don't have the .meta.json suffix (e.g. blob files
// themselves, stray text files). Defends against a regression where
// the walk would try to JSON-parse a binary blob.
func TestFS_New_IgnoresNonMetaFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "T", "U", "S", "K", "ns")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a non-meta file that contains non-JSON bytes — startup must
	// ignore it, not fail.
	stray := filepath.Join(dir, "ns_blob_only")
	if err := os.WriteFile(stray, []byte("\x00\x01binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatalf("New rejected stray non-meta file: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
}

// TestFS_New_IgnoresStrayTmpMetaInIndex covers the rebuildIndex
// branch that explicitly skips tmp-prefixed meta files (defense in
// depth — cleanupTmp removes them earlier, but rebuildIndex must not
// trip if one slips through).
func TestFS_New_IgnoresStrayTmpMetaInIndex(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "T", "U", "S", "K", "ns")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a meta file that has the tmp prefix AND is invalid JSON.
	// It will be cleaned by cleanupTmp before rebuildIndex runs, so
	// New succeeds; this exercises the cleanupTmp removal branch.
	stray := filepath.Join(dir, "tmp.ns_corrupt.meta.json")
	if err := os.WriteFile(stray, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	// The stray must be gone.
	if _, statErr := os.Stat(stray); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("cleanupTmp did not remove stray tmp meta: stat=%v", statErr)
	}
}

// TestFS_New_RejectsCorruptMeta surfaces a malformed .meta.json at
// rebuildIndex time as an error from New, rather than silently
// degrading the in-memory index.
func TestFS_New_RejectsCorruptMeta(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "T", "U", "S", "K", "ns")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a malformed meta.
	corrupt := filepath.Join(dir, "ns_garbage.meta.json")
	if err := os.WriteFile(corrupt, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err == nil {
		t.Fatal("New accepted root with corrupt meta; expected error")
	}
}

// TestFS_FilterDimensions exercises every filter dimension's mismatch
// path (Tenant, User, Session, Task) — the matchesFilter function
// has one branch per dimension.
func TestFS_FilterDimensions(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"}
	if _, err := s.PutBytes(context.Background(), scope, []byte("x"), artifacts.PutOpts{Namespace: "ns"}); err != nil {
		t.Fatal(err)
	}
	cases := []artifacts.ArtifactScope{
		{TenantID: "X"},                                                 // tenant mismatch
		{TenantID: "T", UserID: "X"},                                    // user mismatch
		{TenantID: "T", UserID: "U", SessionID: "X"},                    // session mismatch
		{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "X"},       // task mismatch
	}
	for i, filter := range cases {
		got, err := s.List(context.Background(), filter)
		if err != nil {
			t.Errorf("case %d List: %v", i, err)
		}
		if len(got) != 0 {
			t.Errorf("case %d (%+v): got %d entries, want 0", i, filter, len(got))
		}
	}
	// Full-match filter returns the one entry.
	full, err := s.List(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 1 {
		t.Errorf("full-match filter: got %d, want 1", len(full))
	}
}

// TestFS_List_FilterMatchesNothing exercises the filter loop's "no
// match" path.
func TestFS_List_FilterMatchesNothing(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"}
	if _, err := s.PutBytes(context.Background(), scope, []byte("x"), artifacts.PutOpts{Namespace: "ns"}); err != nil {
		t.Fatal(err)
	}
	// Filter that doesn't match.
	got, err := s.List(context.Background(), artifacts.ArtifactScope{TenantID: "OTHER"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("List with non-matching filter returned %d entries", len(got))
	}
}

// TestFS_Get_DriftBetweenIndexAndDisk surfaces ErrNotFound when the
// index claims a ref exists but the on-disk blob has been deleted
// out-of-band. Surfaces loudly rather than returning (nil, false, nil)
// — the index/disk mismatch is a real error context.
func TestFS_Get_DriftBetweenIndexAndDisk(t *testing.T) {
	root := t.TempDir()
	s, err := fs.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"}
	ref, err := s.PutBytes(context.Background(), scope, []byte("drift-test"), artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	// Sneak the blob out from under the index.
	blobPath := filepath.Join(root, scope.TenantID, scope.UserID, scope.SessionID, scope.TaskID, "ns", ref.ID)
	if err := os.Remove(blobPath); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Get(context.Background(), scope, ref.ID)
	if !errors.Is(err, artifacts.ErrNotFound) {
		t.Errorf("Get on disk-drift: err=%v, want errors.Is ErrNotFound", err)
	}
}
