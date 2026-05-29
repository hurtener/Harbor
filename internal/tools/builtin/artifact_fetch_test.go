package builtin

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
)

// artifactFetchTestStore returns a fresh inmem ArtifactStore wired
// up the same way the dev binary does in `bootDevStack`. Closes via
// t.Cleanup so the test never leaks a driver.
func artifactFetchTestStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artifacts.Open(t.Context(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// artifactFetchTestCtx builds an identity-scoped ctx mirroring how
// the runtime tool-executor calls into the meta-tool. All three
// identity components MUST be non-empty.
func artifactFetchTestCtx(t *testing.T, tenant, user, session, run string) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, run)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// seedArtifact writes a payload under the canonical test (tenant,
// user, session) scope ("tA"/"uA"/"sA") and returns the stable ref.
// The runtime tool-executor leaves `TaskID` empty when projecting
// heavy results; the test mirrors that so the meta-tool reads back
// through the same scope shape. Cross-identity cases vary the FETCH
// scope, not the seed scope, so the seed identity is fixed here.
func seedArtifact(t *testing.T, store artifacts.ArtifactStore, payload []byte) artifacts.ArtifactRef {
	t.Helper()
	ref, err := store.PutBytes(t.Context(),
		artifacts.ArtifactScope{TenantID: "tA", UserID: "uA", SessionID: "sA"},
		payload,
		artifacts.PutOpts{
			Namespace: "test.fixture",
			MimeType:  "application/json",
		})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	return ref
}

// TestArtifactFetch_HappyPath_ReturnsContent — case 1. Store has the
// ref under the caller's scope and the payload is under the default
// max_bytes ceiling: Content == full payload, Truncated == false.
func TestArtifactFetch_HappyPath_ReturnsContent(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	payload := []byte(`{"hello":"world","n":42}`)
	ref := seedArtifact(t, store, payload)

	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")
	out, err := artifactFetch(ctx, store, ArtifactFetchArgs{Ref: ref.ID})
	if err != nil {
		t.Fatalf("artifactFetch: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("Error = %q, want empty on happy path", out.Error)
	}
	if out.Ref != ref.ID {
		t.Errorf("Ref = %q, want %q", out.Ref, ref.ID)
	}
	if out.Content != string(payload) {
		t.Errorf("Content = %q, want %q", out.Content, string(payload))
	}
	if out.Truncated {
		t.Errorf("Truncated = true, want false on under-cap payload")
	}
	if out.SizeBytes != int64(len(payload)) {
		t.Errorf("SizeBytes = %d, want %d", out.SizeBytes, len(payload))
	}
	if out.MIME != "application/json" {
		t.Errorf("MIME = %q, want application/json", out.MIME)
	}
}

// TestArtifactFetch_Truncates_WhenPayloadExceedsMaxBytes — case 2.
// max_bytes is honoured; Truncated flags the LLM that there is more.
func TestArtifactFetch_Truncates_WhenPayloadExceedsMaxBytes(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	payload := bytes.Repeat([]byte("A"), 4096)
	ref := seedArtifact(t, store, payload)

	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")
	out, err := artifactFetch(ctx, store, ArtifactFetchArgs{Ref: ref.ID, MaxBytes: 100})
	if err != nil {
		t.Fatalf("artifactFetch: %v", err)
	}
	if !out.Truncated {
		t.Errorf("Truncated = false, want true on over-cap payload")
	}
	if len(out.Content) != 100 {
		t.Errorf("len(Content) = %d, want 100", len(out.Content))
	}
	if out.SizeBytes != int64(len(payload)) {
		t.Errorf("SizeBytes = %d, want %d (the FULL artifact size, not the truncated slice)", out.SizeBytes, len(payload))
	}
}

// TestArtifactFetch_MissingRef_ReturnsSoftError — case 3. Unknown ref
// surfaces as `Error` (not as a hard error). The runtime can choose
// to repair without aborting the run.
func TestArtifactFetch_MissingRef_ReturnsSoftError(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")
	out, err := artifactFetch(ctx, store, ArtifactFetchArgs{Ref: "does_not_exist_aaa"})
	if err != nil {
		t.Fatalf("artifactFetch: unexpected hard error: %v", err)
	}
	if out.Error == "" {
		t.Fatalf("Error empty, want soft-error message")
	}
	if !strings.Contains(out.Error, "not found") {
		t.Errorf("Error = %q, want substring 'not found'", out.Error)
	}
	if out.Content != "" {
		t.Errorf("Content = %q, want empty on soft error", out.Content)
	}
}

// TestArtifactFetch_MissingIdentity_FailsLoud — case 4. CLAUDE.md
// §6 rule 9 + §13: missing identity is a hard error, never a
// silently-degraded fetch.
func TestArtifactFetch_MissingIdentity_FailsLoud(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	_, err := artifactFetch(context.Background(), store, ArtifactFetchArgs{Ref: "any"})
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("want ErrIdentityRequired, got %v", err)
	}
}

// TestArtifactFetch_CrossIdentity_RejectedByStore — case 5. SECURITY
// REGRESSION GATE. Tenant A writes an artifact; tenant B attempts to
// fetch it. The store rejects with found-false; the meta-tool returns
// a soft error WITHOUT exposing the bytes. The artifact_fetch layer
// does NOT add a redundant guard — the store IS the cross-tenant
// boundary (the keyed `(tenant, user, session, task)` storage layout
// makes a cross-scope hit impossible by construction). This test pins
// that the store + meta-tool together produce the right shape; if
// either layer drifts the test fails.
func TestArtifactFetch_CrossIdentity_RejectedByStore(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	payload := []byte(`{"secret":"only-for-tenant-A"}`)
	refA := seedArtifact(t, store, payload)

	// Tenant B attempts to fetch tenant A's ref.
	ctxB := artifactFetchTestCtx(t, "tB", "uB", "sB", "r1")
	out, err := artifactFetch(ctxB, store, ArtifactFetchArgs{Ref: refA.ID})
	if err != nil {
		t.Fatalf("artifactFetch (tenant B): unexpected hard error: %v", err)
	}
	if out.Content != "" {
		t.Fatalf("SECURITY: tenant B saw tenant A's content: %q", out.Content)
	}
	if out.Error == "" {
		t.Fatal("SECURITY: tenant B got empty Error; expected the same not-found soft error tenant B sees for a non-existent ref")
	}
	// Belt-and-braces: the soft-error wording does not leak the
	// content. (`not found in this session's scope` is the canonical
	// shape.)
	if strings.Contains(out.Error, "only-for-tenant-A") {
		t.Fatalf("SECURITY: soft-error message leaked content: %q", out.Error)
	}

	// And sanity-check the legitimate owner still reads it.
	ctxA := artifactFetchTestCtx(t, "tA", "uA", "sA", "r2")
	outA, err := artifactFetch(ctxA, store, ArtifactFetchArgs{Ref: refA.ID})
	if err != nil {
		t.Fatalf("artifactFetch (tenant A): %v", err)
	}
	if outA.Content != string(payload) {
		t.Errorf("tenant A reads back content = %q, want %q", outA.Content, string(payload))
	}
}

// TestArtifactFetch_NilStore_FailsLoud — case 6. Operator
// misconfiguration: `builtin.RegistryContext.ArtifactStore` was not
// threaded. Fails with a hard error whose message names the
// misconfiguration so the operator can fix the wiring.
func TestArtifactFetch_NilStore_FailsLoud(t *testing.T) {
	t.Parallel()
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")
	_, err := artifactFetch(ctx, nil, ArtifactFetchArgs{Ref: "any"})
	if err == nil {
		t.Fatal("want hard error on nil store, got nil")
	}
	if !strings.Contains(err.Error(), "ArtifactStore is nil") {
		t.Errorf("want operator-readable nil-store message, got %v", err)
	}
}

// TestArtifactFetch_EmptyRef_ReturnsSoftError — empty ref is a soft
// error, NOT a hard error. The planner's repair loop can handle this
// the same way it handles a typo'd tool argument.
func TestArtifactFetch_EmptyRef_ReturnsSoftError(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")
	out, err := artifactFetch(ctx, store, ArtifactFetchArgs{Ref: "   "})
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	if out.Error == "" || !strings.Contains(out.Error, "empty") {
		t.Errorf("Error = %q, want substring 'empty'", out.Error)
	}
}

// TestArtifactFetch_HardMaxBytes_ClampsTo1MiB — max_bytes above the
// hard ceiling is silently clamped (not an error). Keeps the LLM from
// accidentally requesting hundreds of MB.
func TestArtifactFetch_HardMaxBytes_ClampsTo1MiB(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	// Seed a 2 MiB payload so the clamp visibly truncates.
	payload := bytes.Repeat([]byte("B"), 2*1024*1024)
	ref := seedArtifact(t, store, payload)

	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")
	out, err := artifactFetch(ctx, store, ArtifactFetchArgs{
		Ref:      ref.ID,
		MaxBytes: 10 * 1024 * 1024, // request 10 MiB; clamped to 1 MiB
	})
	if err != nil {
		t.Fatalf("artifactFetch: %v", err)
	}
	if !out.Truncated {
		t.Errorf("Truncated = false, want true (1 MiB cap < 2 MiB payload)")
	}
	if len(out.Content) != 1024*1024 {
		t.Errorf("len(Content) = %d, want 1 MiB", len(out.Content))
	}
}
