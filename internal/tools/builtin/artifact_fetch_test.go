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

// defaultTestFetchBounds is the read-back bound a test exercises under
// unless it deliberately narrows it: the built-in default and ceiling,
// which is what an operator who configured neither gets.
func defaultTestFetchBounds() fetchBounds {
	return resolveFetchBounds(0, 0)
}

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
	out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: ref.ID})
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
	out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: ref.ID, MaxBytes: 100})
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
	out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: "does_not_exist_aaa"})
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
	_, err := artifactFetch(context.Background(), store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: "any"})
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
	out, err := artifactFetch(ctxB, store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: refA.ID})
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
	outA, err := artifactFetch(ctxA, store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: refA.ID})
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
	_, err := artifactFetch(ctx, nil, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: "any"})
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
	out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: "   "})
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
	out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{
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

// The byte-offset windowing + operator-ceiling half (Phase 209 / D-353).
//
// The tool and the Protocol byte read answer to ONE operator policy and
// ONE response field set, so these tests assert the same properties the
// surface's own suite asserts — a divergence between the two would mean
// a model and a Console client reading the same artifact disagreed about
// what "truncated" means.

// TestArtifactFetch_OffsetWindows walks every interesting position of the
// window, including the LAST one — where a `truncated` computed as
// `returned < total` rather than `offset + returned < total` gives the
// wrong answer and a paging model loops forever.
func TestArtifactFetch_OffsetWindows(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	payload := []byte("0123456789")
	ref := seedArtifact(t, store, payload)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	for _, tc := range []struct {
		name          string
		offset        int
		maxBytes      int
		wantContent   string
		wantTruncated bool
	}{
		{"head", 0, 4, "0123", true},
		{"middle", 4, 3, "456", true},
		{"last window exactly", 7, 3, "789", false},
		{"last window over-asked", 7, 100, "789", false},
		{"offset at end", 10, 4, "", false},
		{"offset past end", 99, 4, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{
				Ref: ref.ID, Offset: tc.offset, MaxBytes: tc.maxBytes,
			})
			if err != nil {
				t.Fatalf("artifactFetch: %v", err)
			}
			if out.Error != "" {
				t.Fatalf("unexpected soft error: %s", out.Error)
			}
			if out.Content != tc.wantContent {
				t.Errorf("Content = %q, want %q", out.Content, tc.wantContent)
			}
			if out.Truncated != tc.wantTruncated {
				t.Errorf("Truncated = %v, want %v", out.Truncated, tc.wantTruncated)
			}
			if out.Offset != int64(tc.offset) {
				t.Errorf("Offset = %d, want %d (the response must echo the window's start)", out.Offset, tc.offset)
			}
			if out.ReturnedBytes != int64(len(tc.wantContent)) {
				t.Errorf("ReturnedBytes = %d, want %d", out.ReturnedBytes, len(tc.wantContent))
			}
			if out.TotalSizeBytes != int64(len(payload)) {
				t.Errorf("TotalSizeBytes = %d, want %d", out.TotalSizeBytes, len(payload))
			}
			if out.SizeBytes != out.TotalSizeBytes {
				t.Errorf("SizeBytes %d and TotalSizeBytes %d disagree — they are documented as the same number",
					out.SizeBytes, out.TotalSizeBytes)
			}
		})
	}
}

// TestArtifactFetch_PagesTheWholeArtifact walks the artifact the way the
// tool's own description tells a model to: re-call at
// offset + returned_bytes while truncated is true. A field set that is
// not self-consistent either loops forever or drops bytes.
func TestArtifactFetch_PagesTheWholeArtifact(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	payload := bytes.Repeat([]byte("abcdefghij"), 10) // 100 bytes
	ref := seedArtifact(t, store, payload)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")
	bounds := resolveFetchBounds(7, 7)

	var assembled []byte
	offset := 0
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("paging did not terminate — the truncated/offset contract is not self-consistent")
		}
		out, err := artifactFetch(ctx, store, bounds, ArtifactFetchArgs{Ref: ref.ID, Offset: offset})
		if err != nil {
			t.Fatalf("artifactFetch: %v", err)
		}
		assembled = append(assembled, out.Content...)
		if !out.Truncated {
			break
		}
		offset += int(out.ReturnedBytes)
	}
	if !bytes.Equal(assembled, payload) {
		t.Fatalf("paged read assembled %q, want %q", assembled, payload)
	}
}

// TestArtifactFetch_CeilingIsServedNotRefused pins the §13 posture: a
// max_bytes above the operator's ceiling is SERVED at the ceiling and
// reported through the same fields — a model cannot know the deployment's
// ceiling before asking, so a refusal would cost it a turn and teach it
// nothing.
func TestArtifactFetch_CeilingIsServedNotRefused(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	payload := bytes.Repeat([]byte("A"), 4096)
	ref := seedArtifact(t, store, payload)
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")
	bounds := resolveFetchBounds(64, 256)

	out, err := artifactFetch(ctx, store, bounds, ArtifactFetchArgs{Ref: ref.ID, MaxBytes: 1 << 30})
	if err != nil {
		t.Fatalf("artifactFetch: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("an over-ceiling request became an error: %s", out.Error)
	}
	if out.ReturnedBytes != 256 {
		t.Errorf("ReturnedBytes = %d, want the ceiling 256", out.ReturnedBytes)
	}
	if !out.Truncated {
		t.Error("a ceiling-clamped read reported Truncated=false — the clamp was silent, which §13 forbids")
	}
	if out.TotalSizeBytes != int64(len(payload)) {
		t.Errorf("TotalSizeBytes = %d, want %d", out.TotalSizeBytes, len(payload))
	}
}

// TestArtifactFetch_OperatorDefaultApplies pins that omitting max_bytes
// takes the OPERATOR's configured default, not the compile-time one and
// not the whole artifact.
func TestArtifactFetch_OperatorDefaultApplies(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ref := seedArtifact(t, store, bytes.Repeat([]byte("B"), 4096))
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	out, err := artifactFetch(ctx, store, resolveFetchBounds(48, 512), ArtifactFetchArgs{Ref: ref.ID})
	if err != nil {
		t.Fatalf("artifactFetch: %v", err)
	}
	if out.ReturnedBytes != 48 {
		t.Errorf("ReturnedBytes = %d, want the operator default 48", out.ReturnedBytes)
	}
	if !out.Truncated {
		t.Error("the operator default bounded the read but Truncated was false")
	}
}

// TestArtifactFetch_NegativeOffset_IsASoftError pins that a negative
// offset is refused rather than reinterpreted as "from the end" or
// silently floored — a guess the model could not detect.
func TestArtifactFetch_NegativeOffset_IsASoftError(t *testing.T) {
	t.Parallel()
	store := artifactFetchTestStore(t)
	ref := seedArtifact(t, store, []byte("payload"))
	ctx := artifactFetchTestCtx(t, "tA", "uA", "sA", "r1")

	out, err := artifactFetch(ctx, store, defaultTestFetchBounds(), ArtifactFetchArgs{Ref: ref.ID, Offset: -1})
	if err != nil {
		t.Fatalf("artifactFetch: hard error %v, want a soft one", err)
	}
	if out.Error == "" {
		t.Fatal("a negative offset produced no error — it was silently reinterpreted")
	}
	if out.Content != "" {
		t.Errorf("Content = %q on a refused request, want empty", out.Content)
	}
}

// TestResolveFetchBounds_NormalisesTheOperatorPair covers the resolution
// rules the operator-facing knobs carry, including the incoherent case:
// a default above its own ceiling clamps DOWN to the ceiling, because
// the ceiling is the operator's stated upper bound and the default is a
// convenience inside it.
func TestResolveFetchBounds_NormalisesTheOperatorPair(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		inDefault, inHard    int
		wantDefault, wantMax int
	}{
		{"both unset take the built-ins", 0, 0, defaultArtifactFetchMaxBytes, hardArtifactFetchMaxBytes},
		{"negative is treated as unset", -5, -5, defaultArtifactFetchMaxBytes, hardArtifactFetchMaxBytes},
		{"both configured", 128, 512, 128, 512},
		{"default above its ceiling clamps down", 900, 512, 512, 512},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := resolveFetchBounds(tc.inDefault, tc.inHard)
			if b.defaultMax != tc.wantDefault {
				t.Errorf("defaultMax = %d, want %d", b.defaultMax, tc.wantDefault)
			}
			if b.hardMax != tc.wantMax {
				t.Errorf("hardMax = %d, want %d", b.hardMax, tc.wantMax)
			}
			if b.effectiveMax(0) != tc.wantDefault {
				t.Errorf("effectiveMax(0) = %d, want the default %d", b.effectiveMax(0), tc.wantDefault)
			}
			if b.effectiveMax(1<<30) != tc.wantMax {
				t.Errorf("effectiveMax(huge) = %d, want the ceiling %d", b.effectiveMax(1<<30), tc.wantMax)
			}
		})
	}
}

// TestArtifactFetch_SingleSourcesItsBoundsOnConfig pins the anti-drift
// property by value: the tool's fallbacks ARE the configuration
// package's documented defaults, so an operator reading docs/CONFIG.md
// and a model calling the tool cannot be looking at two different
// numbers.
func TestArtifactFetch_SingleSourcesItsBoundsOnConfig(t *testing.T) {
	t.Parallel()
	if defaultArtifactFetchMaxBytes != config.DefaultArtifactFetchMaxBytes {
		t.Errorf("tool default %d != config default %d", defaultArtifactFetchMaxBytes, config.DefaultArtifactFetchMaxBytes)
	}
	if hardArtifactFetchMaxBytes != config.DefaultArtifactFetchHardMaxBytes {
		t.Errorf("tool ceiling %d != config ceiling %d", hardArtifactFetchMaxBytes, config.DefaultArtifactFetchHardMaxBytes)
	}
}
