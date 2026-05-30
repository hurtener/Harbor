package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
)

// newSessionArtifactsTestDriver builds a minimal perTaskRunLoopDriver
// carrying only the fields resolveSessionArtifacts reads — a real
// in-memory artifact store (no mock at the seam, §17) and a discard
// logger.
func newSessionArtifactsTestDriver(t *testing.T, store artifacts.ArtifactStore) *perTaskRunLoopDriver {
	t.Helper()
	return &perTaskRunLoopDriver{
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		artifactStore: store,
	}
}

func newSessionArtifactsStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// TestResolveSessionArtifacts_BuildsManifestFromPriorTurns asserts the
// run loop lists a session's artifacts and builds the manifest with the
// right provenance for a user upload AND a tool-materialised result
// (AC-1, AC-2, AC-5). The store holds artifacts as if put on prior turns.
func TestResolveSessionArtifacts_BuildsManifestFromPriorTurns(t *testing.T) {
	store := newSessionArtifactsStore(t)
	ctx := context.Background()
	scope := artifacts.ArtifactScope{TenantID: "t1", UserID: "u1", SessionID: "s1"}

	upload, err := store.PutText(ctx, scope, "uploaded report", artifacts.PutOpts{
		Filename: "report.txt", MimeType: "text/plain",
		Source: map[string]any{"source": "user_upload"},
	})
	if err != nil {
		t.Fatalf("put upload: %v", err)
	}
	toolArt, err := store.PutText(ctx, scope, "{\"big\":\"result\"}", artifacts.PutOpts{
		Filename: "tool-result-web_search.json", MimeType: "application/json",
		Source: map[string]any{"source": "tool", "tool": "web_search"},
	})
	if err != nil {
		t.Fatalf("put tool artifact: %v", err)
	}

	d := newSessionArtifactsTestDriver(t, store)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}}
	manifest := d.resolveSessionArtifacts(ctx, q)

	if len(manifest) != 2 {
		t.Fatalf("manifest len = %d, want 2", len(manifest))
	}
	byRef := map[string]string{} // ref -> provenance
	mime := map[string]string{}
	for _, e := range manifest {
		byRef[e.Ref] = e.Provenance
		mime[e.Ref] = e.MIME
	}
	if byRef[upload.ID] != "user_upload" {
		t.Errorf("upload provenance = %q, want user_upload", byRef[upload.ID])
	}
	if byRef[toolArt.ID] != "tool" {
		t.Errorf("tool provenance = %q, want tool", byRef[toolArt.ID])
	}
	if mime[toolArt.ID] != "application/json" {
		t.Errorf("tool mime = %q, want application/json", mime[toolArt.ID])
	}
}

// TestResolveSessionArtifacts_IdentityScoped asserts session A's
// artifacts never appear in session B's manifest (AC-7). The List is
// scoped to the run's (tenant, user, session).
func TestResolveSessionArtifacts_IdentityScoped(t *testing.T) {
	store := newSessionArtifactsStore(t)
	ctx := context.Background()

	scopeA := artifacts.ArtifactScope{TenantID: "t1", UserID: "u1", SessionID: "sessA"}
	scopeB := artifacts.ArtifactScope{TenantID: "t1", UserID: "u1", SessionID: "sessB"}
	aRef, err := store.PutText(ctx, scopeA, "A only", artifacts.PutOpts{Source: map[string]any{"source": "user_upload"}})
	if err != nil {
		t.Fatalf("put A: %v", err)
	}
	if _, err := store.PutText(ctx, scopeB, "B only", artifacts.PutOpts{Source: map[string]any{"source": "user_upload"}}); err != nil {
		t.Fatalf("put B: %v", err)
	}

	d := newSessionArtifactsTestDriver(t, store)

	qA := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "sessA"}}
	manifestA := d.resolveSessionArtifacts(ctx, qA)
	if len(manifestA) != 1 {
		t.Fatalf("session A manifest len = %d, want 1", len(manifestA))
	}
	if manifestA[0].Ref != aRef.ID {
		t.Errorf("session A manifest ref = %q, want %q", manifestA[0].Ref, aRef.ID)
	}

	qB := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "sessB"}}
	manifestB := d.resolveSessionArtifacts(ctx, qB)
	for _, e := range manifestB {
		if e.Ref == aRef.ID {
			t.Errorf("session B manifest leaked session A artifact %q", aRef.ID)
		}
	}
}

// TestResolveSessionArtifacts_ListError_NoManifest asserts a List error
// yields NO manifest (fail-soft, never fabricated) and the turn can
// proceed (AC: §5 fail-loud-without-fabrication). A closed real store
// returns ErrStoreClosed from List — a real driver at the seam, not a
// mock.
func TestResolveSessionArtifacts_ListError_NoManifest(t *testing.T) {
	store, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	// Close so List returns ErrStoreClosed.
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	d := newSessionArtifactsTestDriver(t, store)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}}
	manifest := d.resolveSessionArtifacts(context.Background(), q)
	if manifest != nil {
		t.Fatalf("List error: manifest = %v, want nil (fail-soft, no fabrication)", manifest)
	}
}

// TestResolveSessionArtifacts_NilStore_NoManifest asserts a driver with
// no artifact store wired returns no manifest rather than panicking.
func TestResolveSessionArtifacts_NilStore_NoManifest(t *testing.T) {
	d := newSessionArtifactsTestDriver(t, nil)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}}
	if got := d.resolveSessionArtifacts(context.Background(), q); got != nil {
		t.Fatalf("nil store: manifest = %v, want nil", got)
	}
}
