package fs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	fsdriver "github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	"github.com/hurtener/Harbor/internal/config"
)

// emptyTaskSentinel mirrors the driver's on-disk stand-in for an empty
// TaskID. Declared here rather than exported because it is a storage
// detail; the tests below reproduce the layout by hand.
const emptyTaskSentinel = "_"

// seedNamespace is the single namespace these layout fixtures use. The
// namespace is orthogonal to what they exercise (the task segment), so
// it is a constant rather than a parameter every call site repeats.
const seedNamespace = "ns"

// seedOnDisk writes `<root>/<t>/<u>/<s>/<task>/<seedNamespace>/<id>` plus
// its meta sibling exactly the way the driver does, WITHOUT going through
// the driver — which is the only way to produce two copies of one
// `(triple, namespace, id)`, since Put now dedups on the triple.
func seedOnDisk(
	t *testing.T,
	root string,
	scope artifacts.ArtifactScope,
	payload []byte,
) string {
	t.Helper()
	namespace := seedNamespace
	digest := sha256.Sum256(payload)
	hexDigest := hex.EncodeToString(digest[:])
	id := fmt.Sprintf("%s_%s", namespace, hexDigest[:12])

	task := scope.TaskID
	if task == "" {
		task = emptyTaskSentinel
	}
	dir := filepath.Join(root, scope.TenantID, scope.UserID, scope.SessionID, task, namespace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	ref := artifacts.ArtifactRef{
		ID:        id,
		MimeType:  "application/json",
		SizeBytes: int64(len(payload)),
		Filename:  "legacy.json",
		SHA256:    hexDigest,
		Scope:     scope,
		Namespace: namespace,
	}
	meta, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id), payload, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".meta.json"), meta, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	return id
}

func blobPath(root string, scope artifacts.ArtifactScope, id string) string {
	task := scope.TaskID
	if task == "" {
		task = emptyTaskSentinel
	}
	return filepath.Join(root, scope.TenantID, scope.UserID, scope.SessionID, task, seedNamespace, id)
}

// TestFS_LegacyDuplicateTaskDirs_CollapseOnRebuild pins the resolution-
// order answer for artifacts already on disk. A build that keyed writes
// on the task could leave the SAME triple + id under two task
// directories. The index key is now the triple, so the rebuild must pick
// one — and it must pick the same one every time, whatever order the
// directory walk happens to visit.
//
// The bytes are identical by content-addressing, so only the provenance
// stamp is at stake; what would NOT be acceptable is an answer that
// varies between restarts.
func TestFS_LegacyDuplicateTaskDirs_CollapseOnRebuild(t *testing.T) {
	root := t.TempDir()
	payload := []byte(`{"same":"bytes"}`)
	base := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}

	zulu := base
	zulu.TaskID = "run-zulu"
	alpha := base
	alpha.TaskID = "run-alpha"
	id := seedOnDisk(t, root, zulu, payload)
	if other := seedOnDisk(t, root, alpha, payload); other != id {
		t.Fatalf("content-addressed ids diverged: %q vs %q", other, id)
	}

	store, err := fsdriver.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatalf("fs.New over a duplicated layout: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	ctx := context.Background()

	rows, err := store.List(ctx, base)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("duplicates did not collapse: %d rows, want 1", len(rows))
	}
	if rows[0].Scope.TaskID != "run-alpha" {
		t.Errorf("survivor stamp=%q, want the smallest task %q",
			rows[0].Scope.TaskID, "run-alpha")
	}

	// The survivor's bytes are readable on the triple with any stamp,
	// and the path is derived from the STORED ref rather than the
	// caller's scope — a read with the wrong task must not miss.
	for _, reader := range []artifacts.ArtifactScope{base, zulu, alpha} {
		got, found, gerr := store.Get(ctx, reader, id)
		if gerr != nil || !found {
			t.Fatalf("Get(task=%q): found=%v err=%v", reader.TaskID, found, gerr)
		}
		if string(got) != string(payload) {
			t.Errorf("Get(task=%q) bytes=%q, want %q", reader.TaskID, got, payload)
		}
	}

	// Determinism across restarts: a second New over the same root must
	// reach the same survivor.
	reopened, err := fsdriver.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close(context.Background()) }()
	again, err := reopened.List(ctx, base)
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(again) != 1 || again[0].Scope.TaskID != rows[0].Scope.TaskID {
		t.Errorf("collapse is not deterministic across restarts: %+v then %+v",
			rows[0].Scope, again)
	}
}

// TestFS_Delete_SweepsEveryTaskDirectory pins the half of Delete that a
// leftover copy would otherwise corrupt: after a Delete on the triple, a
// restart must not resurrect the artifact from a sibling task directory.
// A Delete that reported success while leaving a copy a later Get
// resolves is the silent degradation CLAUDE.md §13 names.
func TestFS_Delete_SweepsEveryTaskDirectory(t *testing.T) {
	root := t.TempDir()
	payload := []byte(`{"same":"bytes"}`)
	base := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	zulu := base
	zulu.TaskID = "run-zulu"
	alpha := base
	alpha.TaskID = "run-alpha"
	sessionScoped := base // empty TaskID → the `_` sentinel directory

	id := seedOnDisk(t, root, zulu, payload)
	seedOnDisk(t, root, alpha, payload)
	seedOnDisk(t, root, sessionScoped, payload)

	store, err := fsdriver.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	ctx := context.Background()

	existed, err := store.Delete(ctx, base, id)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !existed {
		t.Errorf("Delete returned existed=false for a present artifact")
	}
	for _, scope := range []artifacts.ArtifactScope{zulu, alpha, sessionScoped} {
		p := blobPath(root, scope, id)
		if _, statErr := os.Stat(p); statErr == nil {
			t.Errorf("Delete left a copy behind at %q", p)
		}
		metaPath := p + ".meta.json"
		if _, statErr := os.Stat(metaPath); statErr == nil {
			t.Errorf("Delete left a meta sibling behind at %q", metaPath)
		}
	}
	if err := store.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The decisive assertion: a restart re-reads the tree, so a survivor
	// would come back.
	reopened, err := fsdriver.New(config.ArtifactsConfig{Driver: "fs", FSRoot: root})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close(context.Background()) }()
	rows, err := reopened.List(ctx, base)
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("deleted artifact resurrected on restart: %d rows", len(rows))
	}
}
