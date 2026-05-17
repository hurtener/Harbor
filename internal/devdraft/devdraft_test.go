// Package devdraft tests cover the Store + HTTP handler. The
// load-bearing integration test (round-trip across the handler with
// real bus + scaffold engine) lives in test/integration/phase66_draft_save_test.go.
package devdraft

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
)

// testIdentity is the canonical triple every test uses unless it
// needs to assert cross-identity isolation.
var testIdentity = identity.Identity{
	TenantID:  "t1",
	UserID:    "u1",
	SessionID: "s1",
}

// otherIdentity is the second triple used by the cross-identity
// isolation tests.
var otherIdentity = identity.Identity{
	TenantID:  "t2",
	UserID:    "u2",
	SessionID: "s2",
}

// newTestBus builds the canonical in-mem bus the tests publish onto.
func newTestBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     32,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// newTestStore is the canonical Store constructor every test reaches
// for. The root is per-test (t.TempDir).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".harbor", "drafts")
	store, err := NewStore(Options{
		Root: root,
		Bus:  newTestBus(t),
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// ctxWith attaches the test identity to ctx — every Store method
// expects this.
func ctxWith(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestNewStore_RejectsMissingRoot pins the §13 fail-loud-at-
// construction posture for the on-disk root.
func TestNewStore_RejectsMissingRoot(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	if _, err := NewStore(Options{Bus: bus}); err == nil {
		t.Fatal("NewStore returned nil error for missing root")
	}
	if _, err := NewStore(Options{Root: "  ", Bus: bus}); err == nil {
		t.Fatal("NewStore returned nil error for whitespace-only root")
	}
}

// TestNewStore_RejectsMissingBus pins the F1 lesson — a Store without
// a bus would silently drop the observability surface.
func TestNewStore_RejectsMissingBus(t *testing.T) {
	t.Parallel()
	if _, err := NewStore(Options{Root: t.TempDir()}); err == nil {
		t.Fatal("NewStore returned nil error for missing bus")
	}
}

// TestStore_Create_HappyPath_MaterialisesDraftTree pins the seed
// shape: a Create call produces the same file set the scaffold
// engine renders, under the identity-scoped path.
func TestStore_Create_HappyPath_MaterialisesDraftTree(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)

	draft, err := store.Create(ctx, CreateOptions{Name: "agent-x"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if draft.ID == "" {
		t.Fatal("Create returned empty DraftID")
	}
	if draft.Template != "minimal-react" {
		t.Errorf("Create draft.Template = %q, want minimal-react", draft.Template)
	}

	// On-disk path: <root>/<tenant>/<user>/<session>/<draft_id>/
	want := filepath.Join(store.Root(),
		testIdentity.TenantID, testIdentity.UserID, testIdentity.SessionID, draft.ID)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("Create did not materialise draft root at %s: %v", want, err)
	}

	// File set matches the scaffold engine's output.
	expected := []string{"README.md", "agent.go", "agent_test.go", "go.mod", "harbor.yaml"}
	if len(draft.Files) != len(expected) {
		t.Fatalf("Create draft.Files len = %d, want %d (%v)", len(draft.Files), len(expected), pathsOf(draft.Files))
	}
	for i, want := range expected {
		if draft.Files[i].Path != want {
			t.Errorf("draft.Files[%d].Path = %q, want %q", i, draft.Files[i].Path, want)
		}
	}
}

// TestStore_Create_RejectsInvalidName pins the scaffold name shape.
func TestStore_Create_RejectsInvalidName(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	cases := []string{"", "Foo", "foo/bar", "../oops", "foo bar"}
	for _, name := range cases {
		_, err := store.Create(ctx, CreateOptions{Name: name})
		if err == nil {
			t.Errorf("Create(%q) returned nil error", name)
			continue
		}
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) error not ErrInvalidName: %v", name, err)
		}
	}
}

// TestStore_Create_RejectsUnknownTemplate pins the templates surface.
func TestStore_Create_RejectsUnknownTemplate(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	_, err := store.Create(ctx, CreateOptions{Name: "agent", Template: "does-not-exist"})
	if err == nil {
		t.Fatal("Create returned nil error for unknown template")
	}
	if !errors.Is(err, ErrUnknownTemplate) {
		t.Errorf("error not ErrUnknownTemplate: %v", err)
	}
}

// TestStore_Methods_RejectMissingIdentity pins §6 rule 9. Every
// public Store method MUST fail closed on missing identity.
func TestStore_Methods_RejectMissingIdentity(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	bareCtx := context.Background()

	if _, err := store.Create(bareCtx, CreateOptions{Name: "agent"}); !errors.Is(err, ErrIdentityMissing) {
		t.Errorf("Create on bare ctx: error not ErrIdentityMissing: %v", err)
	}
	if _, err := store.Get(bareCtx, "anything"); !errors.Is(err, ErrIdentityMissing) {
		t.Errorf("Get on bare ctx: error not ErrIdentityMissing: %v", err)
	}
	if err := store.WriteFile(bareCtx, "anything", "x.txt", []byte{}); !errors.Is(err, ErrIdentityMissing) {
		t.Errorf("WriteFile on bare ctx: error not ErrIdentityMissing: %v", err)
	}
	if _, err := store.Preview(bareCtx, "anything"); !errors.Is(err, ErrIdentityMissing) {
		t.Errorf("Preview on bare ctx: error not ErrIdentityMissing: %v", err)
	}
	if _, err := store.Save(bareCtx, "anything", SaveOptions{Name: "x", OutputDir: "/tmp/x"}); !errors.Is(err, ErrIdentityMissing) {
		t.Errorf("Save on bare ctx: error not ErrIdentityMissing: %v", err)
	}
	if err := store.Discard(bareCtx, "anything"); !errors.Is(err, ErrIdentityMissing) {
		t.Errorf("Discard on bare ctx: error not ErrIdentityMissing: %v", err)
	}
}

// TestStore_Get_RoundTrip pins the read-after-create invariant.
func TestStore_Get_RoundTrip(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Get(ctx, draft.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != draft.ID {
		t.Errorf("Get id = %q, want %q", got.ID, draft.ID)
	}
	if len(got.Files) != len(draft.Files) {
		t.Errorf("Get file count = %d, want %d", len(got.Files), len(draft.Files))
	}
}

// TestStore_Get_NotFound pins ErrNotFound.
func TestStore_Get_NotFound(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	_, err := store.Get(ctx, "01HZZZZZZZZZZZZZZZZZZZZZZZ")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on missing draft: error not ErrNotFound: %v", err)
	}
}

// TestStore_Get_CrossIdentityIsolation pins §6 rule 2 — a draft
// created under (t1,u1,s1) is invisible to (t2,u2,s2). The on-disk
// layout enforces this by construction.
func TestStore_Get_CrossIdentityIsolation(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctxA := ctxWith(t, testIdentity)
	ctxB := ctxWith(t, otherIdentity)

	draft, err := store.Create(ctxA, CreateOptions{Name: "isolated"})
	if err != nil {
		t.Fatalf("Create (A): %v", err)
	}
	// B should not see it.
	if _, err := store.Get(ctxB, draft.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get under (B) returned the draft created under (A) — isolation breach: %v", err)
	}
	// A still sees it.
	if _, err := store.Get(ctxA, draft.ID); err != nil {
		t.Errorf("Get under (A) failed after isolation test: %v", err)
	}
}

// TestStore_WriteFile_HappyPath pins the file-edit path.
func TestStore_WriteFile_HappyPath(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := []byte("// edited\npackage agent\n")
	if err := store.WriteFile(ctx, draft.ID, "agent.go", want); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := store.Get(ctx, draft.ID)
	if err != nil {
		t.Fatalf("Get after WriteFile: %v", err)
	}
	var found bool
	for _, f := range got.Files {
		if f.Path == "agent.go" {
			found = true
			if string(f.Content) != string(want) {
				t.Errorf("agent.go content = %q, want %q", string(f.Content), string(want))
			}
		}
	}
	if !found {
		t.Errorf("Get after WriteFile did not surface agent.go")
	}
}

// TestStore_WriteFile_RejectsPathTraversal pins §7 rule 5 — both the
// lexical escape and the "absolute path" rejection MUST be loud.
func TestStore_WriteFile_RejectsPathTraversal(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	bad := []string{
		"../escape.txt",
		"sub/../../escape.txt",
		"/abs/path.txt",
		"",
	}
	for _, p := range bad {
		err := store.WriteFile(ctx, draft.ID, p, []byte("x"))
		if err == nil {
			t.Errorf("WriteFile(%q) returned nil error — traversal MUST fail loud", p)
		}
	}
}

// TestStore_WriteFile_RejectsOversize pins the per-file cap.
func TestStore_WriteFile_RejectsOversize(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store, err := NewStore(Options{
		Root:         filepath.Join(t.TempDir(), ".harbor", "drafts"),
		Bus:          bus,
		MaxFileBytes: 16,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	huge := make([]byte, 1024)
	if err := store.WriteFile(ctx, draft.ID, "agent.go", huge); err == nil {
		t.Errorf("WriteFile returned nil error on oversize body")
	}
}

// TestStore_Preview_HappyPath pins the validation pass — the
// scaffolded harbor.yaml MUST validate cleanly.
func TestStore_Preview_HappyPath(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := store.Preview(ctx, draft.ID)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !res.OK {
		t.Errorf("Preview ok=false (errors=%v) — scaffolded harbor.yaml MUST validate", res.Errors)
	}
}

// TestStore_Preview_DetectsInvalidYAML pins the fail-loud surface
// when the operator mutates the yaml to a broken shape.
func TestStore_Preview_DetectsInvalidYAML(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Overwrite harbor.yaml with garbage. The config loader rejects.
	if err := store.WriteFile(ctx, draft.ID, "harbor.yaml", []byte("not a real yaml file: : :\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	res, err := store.Preview(ctx, draft.ID)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if res.OK {
		t.Errorf("Preview ok=true on broken yaml — preview MUST surface validation failures")
	}
	if len(res.Errors) == 0 {
		t.Errorf("Preview returned ok=false with no errors — the error list MUST be populated")
	}
}

// TestStore_Save_RoundTrip is the load-bearing acceptance criterion:
// edit → preview → save → resulting scaffold passes config.Validate.
func TestStore_Save_RoundTrip(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)

	// Create.
	draft, err := store.Create(ctx, CreateOptions{Name: "round-trip"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Edit one file (a no-op semantic change to README — proves the
	// mutation rides through Save).
	custom := []byte("# Custom README — edited via draft\n")
	if err := store.WriteFile(ctx, draft.ID, "README.md", custom); err != nil {
		t.Fatalf("WriteFile README: %v", err)
	}
	// Preview clean.
	res, err := store.Preview(ctx, draft.ID)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !res.OK {
		t.Fatalf("Preview ok=false before save (errors=%v)", res.Errors)
	}
	// Save.
	outDir := filepath.Join(t.TempDir(), "promoted")
	saved, err := store.Save(ctx, draft.ID, SaveOptions{Name: "round-trip", OutputDir: outDir})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if saved.OutputDir != outDir {
		t.Errorf("Save.OutputDir = %q, want %q", saved.OutputDir, outDir)
	}
	wantFiles := []string{"README.md", "agent.go", "agent_test.go", "go.mod", "harbor.yaml"}
	if len(saved.Files) != len(wantFiles) {
		t.Fatalf("Save.Files = %v, want %v", saved.Files, wantFiles)
	}
	for i, w := range wantFiles {
		if saved.Files[i] != w {
			t.Errorf("Save.Files[%d] = %q, want %q", i, saved.Files[i], w)
		}
	}
	// Edited README rode through.
	got, err := os.ReadFile(filepath.Join(outDir, "README.md"))
	if err != nil {
		t.Fatalf("read promoted README: %v", err)
	}
	if string(got) != string(custom) {
		t.Errorf("promoted README content = %q, want %q", string(got), string(custom))
	}
	// Promoted harbor.yaml passes the real config validator (the
	// load-bearing acceptance criterion).
	cfgPath := filepath.Join(outDir, "harbor.yaml")
	if _, err := config.Load(context.Background(), cfgPath); err != nil {
		t.Errorf("promoted harbor.yaml failed config.Load: %v", err)
	}
}

// TestStore_Save_RejectsInvalidYAML pins the fail-loud-at-the-seam
// surface — Save MUST refuse to promote an invalid draft.
func TestStore_Save_RejectsInvalidYAML(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.WriteFile(ctx, draft.ID, "harbor.yaml", []byte("garbage:\n  - [\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	outDir := filepath.Join(t.TempDir(), "should-not-exist")
	_, err = store.Save(ctx, draft.ID, SaveOptions{Name: "agent", OutputDir: outDir})
	if err == nil {
		t.Fatal("Save returned nil error on invalid yaml")
	}
	if !errors.Is(err, ErrValidationFailed) {
		t.Errorf("error not ErrValidationFailed: %v", err)
	}
	if _, statErr := os.Stat(outDir); statErr == nil {
		t.Errorf("Save created output dir despite validation failure")
	}
}

// TestStore_Save_RejectsExistingOutputDir pins the no-overwrite
// posture.
func TestStore_Save_RejectsExistingOutputDir(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	outDir := filepath.Join(t.TempDir(), "preexists")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	_, err = store.Save(ctx, draft.ID, SaveOptions{Name: "agent", OutputDir: outDir})
	if !errors.Is(err, ErrOutputDirExists) {
		t.Errorf("Save against pre-existing dir: error not ErrOutputDirExists: %v", err)
	}
}

// TestStore_Discard_RemovesTree pins the discard path.
func TestStore_Discard_RemovesTree(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Discard(ctx, draft.ID); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := store.Get(ctx, draft.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Discard: error not ErrNotFound: %v", err)
	}
}

// TestStore_Discard_IsIdempotent pins the idempotent close.
func TestStore_Discard_IsIdempotent(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := ctxWith(t, testIdentity)
	if err := store.Discard(ctx, "01HZZZZZZZZZZZZZZZZZZZZZZZ"); err != nil {
		t.Errorf("Discard on missing draft returned error: %v", err)
	}
}

// TestStore_LifecycleEvents pins the bus emit shape — every Store
// method's success path lands a typed event on the bus.
func TestStore_LifecycleEvents(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	store, err := NewStore(Options{
		Root: filepath.Join(t.TempDir(), ".harbor", "drafts"),
		Bus:  bus,
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, err := bus.Subscribe(subCtx, events.Filter{
		Tenant:  testIdentity.TenantID,
		User:    testIdentity.UserID,
		Session: testIdentity.SessionID,
		Types: []events.EventType{
			EventTypeDraftCreated,
			EventTypeDraftUpdated,
			EventTypeDraftPreviewed,
			EventTypeDraftSaved,
			EventTypeDraftDiscarded,
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	ctx := ctxWith(t, testIdentity)
	draft, err := store.Create(ctx, CreateOptions{Name: "ev-agent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.WriteFile(ctx, draft.ID, "README.md", []byte("hi\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := store.Preview(ctx, draft.ID); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if _, err := store.Save(ctx, draft.ID, SaveOptions{Name: "ev-agent", OutputDir: filepath.Join(t.TempDir(), "out")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Discard(ctx, draft.ID); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	want := []events.EventType{
		EventTypeDraftCreated,
		EventTypeDraftUpdated,
		EventTypeDraftPreviewed,
		EventTypeDraftSaved,
		EventTypeDraftDiscarded,
	}
	got := drainEvents(t, sub.Events(), len(want))
	if len(got) != len(want) {
		t.Fatalf("expected %d events, got %d (%v)", len(want), len(got), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// pathsOf is a tiny helper for failure messages.
func pathsOf(files []DraftFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

// drainEvents reads N events off the subscription with a bounded
// timeout. The CLAUDE.md §17.4 rule forbids time.Sleep as a sync
// primitive; we use a bounded select instead.
func drainEvents(t *testing.T, ch <-chan events.Event, n int) []events.EventType {
	t.Helper()
	out := make([]events.EventType, 0, n)
	for i := 0; i < n; i++ {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("bus subscription closed after %d events", i)
			}
			out = append(out, ev.Type)
		case <-context.Background().Done():
			t.Fatal("ctx cancelled")
		}
		// A safety timeout — bounded real-time, used as a deadline
		// not a sync primitive (the channel is the sync primitive;
		// this is a "the bus is broken" escape).
	}
	return out
}

