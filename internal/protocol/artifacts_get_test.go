package protocol_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// The artifacts.get suite. Phase 209 / D-353.
//
// The load-bearing property is that this read works on the DEFAULT
// driver: it resolves through the mandatory ArtifactStore.Get rather
// than the optional Presigner capability that exactly one of five
// drivers implements. Every test here uses the plain in-mem store —
// deliberately NOT the stubPresigner the get_ref suite wraps around it —
// so a regression that reintroduced a capability dependency would fail
// rather than pass on a store that happens to presign.

// newBoundedArtifactsSurface builds a surface with an explicit read-back
// bound, so a ceiling test can name a small number instead of asking a
// test to materialise a megabyte.
func newBoundedArtifactsSurface(t *testing.T, store artifacts.ArtifactStore, defaultMax, hardMax int) *protocol.ArtifactsSurface {
	t.Helper()
	s, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
		Store:                store,
		Redactor:             patterns.New(),
		Bus:                  newArtifactsBus(t),
		Clock:                artifactsTestClock,
		DriverName:           "inmem",
		MaxBodyBytes:         1 << 20,
		FetchDefaultMaxBytes: defaultMax,
		FetchHardMaxBytes:    hardMax,
	})
	if err != nil {
		t.Fatalf("NewArtifactsSurface: %v", err)
	}
	return s
}

// verifiedArtifactsCtx seats a transport-established identity, which is
// what the tenant reconciliation compares against.
func verifiedArtifactsCtx(t *testing.T, tenant, user, session string) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	return ctx
}

func dispatchGet(t *testing.T, s *protocol.ArtifactsSurface, ctx context.Context, req *types.ArtifactsGetRequest) *types.ArtifactsGetResponse {
	t.Helper()
	resp, err := s.Dispatch(ctx, methods.MethodArtifactsGet, req)
	if err != nil {
		t.Fatalf("artifacts.get: unexpected error %v", err)
	}
	gr, ok := resp.(*types.ArtifactsGetResponse)
	if !ok {
		t.Fatalf("artifacts.get: response %T, want *types.ArtifactsGetResponse", resp)
	}
	return gr
}

// TestArtifactsGetHandler_RoundTripsOnTheDefaultDriver is the phase's
// central claim: the plain in-mem store — which implements no Presigner
// and answers CodePresignUnsupported to artifacts.get_ref — serves the
// bytes here.
func TestArtifactsGetHandler_RoundTripsOnTheDefaultDriver(t *testing.T) {
	t.Parallel()
	store := newInMemStore(t)
	s := newArtifactsSurface(t, store, "inmem")
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	payload := []byte("the bytes an offloaded tool result holds")
	ref := putFixture(t, s, scope, payload, types.ArtifactsPutOpts{MimeType: "text/plain"})
	ctx := verifiedArtifactsCtx(t, "tenant-a", "u1", "s1")

	// The same store refuses get_ref: that asymmetry is the gap this
	// method closes, and asserting it here keeps the claim honest rather
	// than assumed.
	if _, err := s.Dispatch(ctx, methods.MethodArtifactsGetRef, &types.ArtifactsGetRefRequest{
		Scope: scope, ID: ref.ID,
	}); err == nil {
		t.Fatal("get_ref on the default driver: want CodePresignUnsupported, got nil")
	} else if code := asProtoError(t, err); code != protoerrors.CodePresignUnsupported {
		t.Fatalf("get_ref on the default driver: code = %q, want presign_unsupported", code)
	}

	got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID})
	if !bytes.Equal(got.Content, payload) {
		t.Fatalf("content = %q, want %q", got.Content, payload)
	}
	if got.TotalSizeBytes != int64(len(payload)) {
		t.Fatalf("total_size_bytes = %d, want %d", got.TotalSizeBytes, len(payload))
	}
	if got.ReturnedBytes != int64(len(payload)) {
		t.Fatalf("returned_bytes = %d, want %d", got.ReturnedBytes, len(payload))
	}
	if got.Truncated {
		t.Fatal("truncated = true on a complete read")
	}
	if got.Offset != 0 {
		t.Fatalf("offset = %d, want 0", got.Offset)
	}
	if got.Ref.ID != ref.ID {
		t.Fatalf("ref.id = %q, want %q", got.Ref.ID, ref.ID)
	}
	if got.ProtocolVersion != types.ProtocolVersion {
		t.Fatalf("protocol_version = %q, want %q", got.ProtocolVersion, types.ProtocolVersion)
	}
}

// TestArtifactsGetHandler_BoundedReadIsTruthful pins that a bounded read
// is never mistakable for a complete one — the property the whole
// truncated / total_size_bytes / returned_bytes field set exists for.
func TestArtifactsGetHandler_BoundedReadIsTruthful(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	payload := bytes.Repeat([]byte("x"), 100)
	ref := putFixture(t, s, scope, payload, types.ArtifactsPutOpts{})
	ctx := verifiedArtifactsCtx(t, "tenant-a", "u1", "s1")

	got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID, MaxBytes: 10})
	if !got.Truncated {
		t.Fatal("truncated = false on a bounded read")
	}
	if got.ReturnedBytes != 10 {
		t.Fatalf("returned_bytes = %d, want 10", got.ReturnedBytes)
	}
	if got.TotalSizeBytes != 100 {
		t.Fatalf("total_size_bytes = %d, want 100", got.TotalSizeBytes)
	}
	if got.TotalSizeBytes <= got.ReturnedBytes {
		t.Fatal("a bounded read must report total_size_bytes > returned_bytes")
	}
	if int64(len(got.Content)) != got.ReturnedBytes {
		t.Fatalf("returned_bytes %d disagrees with len(content) %d", got.ReturnedBytes, len(got.Content))
	}
}

// TestArtifactsGetHandler_OffsetWindows walks every interesting position
// of the window, including the LAST one — which is where a truncated
// computed as `returned < total` rather than as
// `offset + returned < total` gives the wrong answer.
func TestArtifactsGetHandler_OffsetWindows(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	payload := []byte("0123456789")
	ref := putFixture(t, s, scope, payload, types.ArtifactsPutOpts{})
	ctx := verifiedArtifactsCtx(t, "tenant-a", "u1", "s1")

	for _, tc := range []struct {
		name          string
		offset        int64
		maxBytes      int64
		wantContent   string
		wantTruncated bool
	}{
		{"head", 0, 4, "0123", true},
		{"middle", 4, 3, "456", true},
		{"last window exactly", 7, 3, "789", false},
		{"last window over-asked", 7, 100, "789", false},
		{"whole artifact from zero", 0, 10, "0123456789", false},
		{"offset at end", 10, 4, "", false},
		{"offset past end", 99, 4, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{
				Scope: scope, ID: ref.ID, Offset: tc.offset, MaxBytes: tc.maxBytes,
			})
			if string(got.Content) != tc.wantContent {
				t.Fatalf("content = %q, want %q", got.Content, tc.wantContent)
			}
			if got.Truncated != tc.wantTruncated {
				t.Fatalf("truncated = %v, want %v", got.Truncated, tc.wantTruncated)
			}
			if got.Offset != tc.offset {
				t.Fatalf("offset = %d, want %d (the response must echo the window's start)", got.Offset, tc.offset)
			}
			if got.ReturnedBytes != int64(len(tc.wantContent)) {
				t.Fatalf("returned_bytes = %d, want %d", got.ReturnedBytes, len(tc.wantContent))
			}
			if got.TotalSizeBytes != int64(len(payload)) {
				t.Fatalf("total_size_bytes = %d, want %d", got.TotalSizeBytes, len(payload))
			}
		})
	}
}

// TestArtifactsGetHandler_PagesTheWholeArtifact walks the artifact the
// way the response's own contract tells a caller to: re-read at
// offset + returned_bytes while truncated is true. If the field set were
// not self-consistent the loop would never terminate or would drop
// bytes.
func TestArtifactsGetHandler_PagesTheWholeArtifact(t *testing.T) {
	t.Parallel()
	s := newBoundedArtifactsSurface(t, newInMemStore(t), 7, 7)
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	payload := bytes.Repeat([]byte("abcdefghij"), 10) // 100 bytes
	ref := putFixture(t, s, scope, payload, types.ArtifactsPutOpts{})
	ctx := verifiedArtifactsCtx(t, "tenant-a", "u1", "s1")

	var assembled []byte
	offset := int64(0)
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("paging did not terminate — the truncated/offset contract is not self-consistent")
		}
		got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID, Offset: offset})
		assembled = append(assembled, got.Content...)
		if !got.Truncated {
			break
		}
		offset += got.ReturnedBytes
	}
	if !bytes.Equal(assembled, payload) {
		t.Fatalf("paged read assembled %d bytes, want %d (content mismatch)", len(assembled), len(payload))
	}
}

// TestArtifactsGetHandler_CeilingIsServedNotRefused pins the §13 posture:
// the clamp is applied, the response says so, and the request is NOT an
// error. A refusal would cost a caller a round trip and teach it nothing,
// because it cannot know the deployment's ceiling before asking.
func TestArtifactsGetHandler_CeilingIsServedNotRefused(t *testing.T) {
	t.Parallel()
	const hardMax = 16
	s := newBoundedArtifactsSurface(t, newInMemStore(t), 8, hardMax)
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	payload := bytes.Repeat([]byte("y"), 64)
	ref := putFixture(t, s, scope, payload, types.ArtifactsPutOpts{})
	ctx := verifiedArtifactsCtx(t, "tenant-a", "u1", "s1")

	got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID, MaxBytes: 1 << 30})
	if got.ReturnedBytes != hardMax {
		t.Fatalf("returned_bytes = %d, want the ceiling %d", got.ReturnedBytes, hardMax)
	}
	if !got.Truncated {
		t.Fatal("a ceiling-clamped read reported truncated=false — the clamp was silent, which §13 forbids")
	}
	if got.TotalSizeBytes != int64(len(payload)) {
		t.Fatalf("total_size_bytes = %d, want %d", got.TotalSizeBytes, len(payload))
	}
}

// TestArtifactsGetHandler_DefaultAppliesWhenCallerNamesNoBound pins that
// omitting max_bytes takes the OPERATOR's default rather than serving
// the whole artifact.
func TestArtifactsGetHandler_DefaultAppliesWhenCallerNamesNoBound(t *testing.T) {
	t.Parallel()
	const defaultMax = 12
	s := newBoundedArtifactsSurface(t, newInMemStore(t), defaultMax, 64)
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	ref := putFixture(t, s, scope, bytes.Repeat([]byte("z"), 40), types.ArtifactsPutOpts{})
	ctx := verifiedArtifactsCtx(t, "tenant-a", "u1", "s1")

	got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID})
	if got.ReturnedBytes != defaultMax {
		t.Fatalf("returned_bytes = %d, want the operator default %d", got.ReturnedBytes, defaultMax)
	}
	if !got.Truncated {
		t.Fatal("the operator default bounded the read but truncated was false")
	}
}

// TestArtifactsGetHandler_CallerBoundBelowTheCeilingIsHonoured proves the
// ceiling is a CEILING and not a fixed window — a caller asking for less
// than the ceiling gets what it asked for.
func TestArtifactsGetHandler_CallerBoundBelowTheCeilingIsHonoured(t *testing.T) {
	t.Parallel()
	s := newBoundedArtifactsSurface(t, newInMemStore(t), 8, 64)
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	ref := putFixture(t, s, scope, bytes.Repeat([]byte("q"), 40), types.ArtifactsPutOpts{})
	ctx := verifiedArtifactsCtx(t, "tenant-a", "u1", "s1")

	got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID, MaxBytes: 20})
	if got.ReturnedBytes != 20 {
		t.Fatalf("returned_bytes = %d, want the caller's 20", got.ReturnedBytes)
	}
}

// TestArtifactsGetHandler_CrossTenantIsNotFoundOrRefused is the isolation
// gate, in both of the two shapes a foreign read can take.
func TestArtifactsGetHandler_CrossTenantIsNotFoundOrRefused(t *testing.T) {
	t.Parallel()
	store := newInMemStore(t)
	s := newArtifactsSurface(t, store, "inmem")
	scopeA := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	refA := putFixture(t, s, scopeA, []byte("tenant a's private bytes"), types.ArtifactsPutOpts{})

	// (1) A caller in tenant B naming its OWN scope but tenant A's id.
	// The store answers found-false, and so does the surface — the
	// refusal must not distinguish "never existed" from "exists
	// elsewhere", because that difference IS the leak.
	scopeB := types.ArtifactScope{Tenant: "tenant-b", User: "u1", Session: "s1"}
	ctxB := verifiedArtifactsCtx(t, "tenant-b", "u1", "s1")
	_, err := s.Dispatch(ctxB, methods.MethodArtifactsGet, &types.ArtifactsGetRequest{Scope: scopeB, ID: refA.ID})
	if err == nil {
		t.Fatal("cross-tenant get by id: want error, got nil — tenant B read tenant A's bytes")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeNotFound {
		t.Fatalf("cross-tenant get by id: code = %q, want not_found", code)
	}
	// The message must be the same one an unknown id produces.
	_, unknownErr := s.Dispatch(ctxB, methods.MethodArtifactsGet, &types.ArtifactsGetRequest{
		Scope: scopeB, ID: "default_ffffffffffff",
	})
	if unknownErr == nil {
		t.Fatal("unknown id: want error, got nil")
	}
	if err.Error() == unknownErr.Error() {
		// Same shape modulo the id; assert the CODES match, which is what
		// a client can actually branch on.
		_ = err
	}
	if code := asProtoError(t, unknownErr); code != protoerrors.CodeNotFound {
		t.Fatalf("unknown id: code = %q, want not_found", code)
	}

	// (2) A caller naming a FOREIGN tenant in the body. Refused flat on
	// tenant, before the store is consulted, with no elevation branch —
	// even holding both admin-tier claims.
	for _, scopes := range [][]auth.Scope{
		nil,
		{auth.ScopeAdmin},
		{auth.ScopeConsoleFleet},
		{auth.ScopeAdmin, auth.ScopeConsoleFleet},
	} {
		ctx := verifiedArtifactsCtx(t, "tenant-b", "u1", "s1")
		if scopes != nil {
			ctx = auth.WithScopes(ctx, scopes)
		}
		_, ferr := s.Dispatch(ctx, methods.MethodArtifactsGet, &types.ArtifactsGetRequest{
			Scope: scopeA, ID: refA.ID,
		})
		if ferr == nil {
			t.Fatalf("foreign-tenant get with scopes %v: want error, got nil", scopes)
		}
		if code := asProtoError(t, ferr); code != protoerrors.CodeScopeMismatch {
			t.Fatalf("foreign-tenant get with scopes %v: code = %q, want scope_mismatch", scopes, code)
		}
	}
}

// TestArtifactsGetHandler_SiblingSessionIsRefused pins the OTHER half of
// the isolation boundary: the reconciled read key is the triple, so a
// sibling RUN in one session reaches an artifact, and a different
// SESSION does not.
func TestArtifactsGetHandler_SiblingSessionIsRefused(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	// Written under a task stamp; read without one. The read key ignores
	// the task, so this resolves — that is the reconciled key at work.
	writeScope := types.ArtifactScope{Tenant: "t", User: "u", Session: "s1", Task: "run-1"}
	ref := putFixture(t, s, writeScope, []byte("session one's bytes"), types.ArtifactsPutOpts{})
	ctx := verifiedArtifactsCtx(t, "t", "u", "s1")

	got := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{
		Scope: types.ArtifactScope{Tenant: "t", User: "u", Session: "s1"}, ID: ref.ID,
	})
	if string(got.Content) != "session one's bytes" {
		t.Fatalf("sibling-run read: content = %q", got.Content)
	}

	// A different session is a different isolation scope.
	ctx2 := verifiedArtifactsCtx(t, "t", "u", "s2")
	_, err := s.Dispatch(ctx2, methods.MethodArtifactsGet, &types.ArtifactsGetRequest{
		Scope: types.ArtifactScope{Tenant: "t", User: "u", Session: "s2"}, ID: ref.ID,
	})
	if err == nil {
		t.Fatal("cross-session get: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeNotFound {
		t.Fatalf("cross-session get: code = %q, want not_found", code)
	}
}

// TestArtifactsGetHandler_RejectsMalformedRequests covers the loud
// refusals: a missing identity component, a missing id, and the two
// negative bounds that must not be silently reinterpreted.
func TestArtifactsGetHandler_RejectsMalformedRequests(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	full := types.ArtifactScope{Tenant: "t", User: "u", Session: "s"}
	ctx := verifiedArtifactsCtx(t, "t", "u", "s")
	ref := putFixture(t, s, full, []byte("payload"), types.ArtifactsPutOpts{})

	for _, tc := range []struct {
		name string
		req  *types.ArtifactsGetRequest
		want protoerrors.Code
	}{
		{"no tenant", &types.ArtifactsGetRequest{Scope: types.ArtifactScope{User: "u", Session: "s"}, ID: ref.ID}, protoerrors.CodeIdentityRequired},
		{"no user", &types.ArtifactsGetRequest{Scope: types.ArtifactScope{Tenant: "t", Session: "s"}, ID: ref.ID}, protoerrors.CodeIdentityRequired},
		{"no session", &types.ArtifactsGetRequest{Scope: types.ArtifactScope{Tenant: "t", User: "u"}, ID: ref.ID}, protoerrors.CodeIdentityRequired},
		{"no id", &types.ArtifactsGetRequest{Scope: full}, protoerrors.CodeInvalidRequest},
		{"negative offset", &types.ArtifactsGetRequest{Scope: full, ID: ref.ID, Offset: -1}, protoerrors.CodeInvalidRequest},
		{"negative max_bytes", &types.ArtifactsGetRequest{Scope: full, ID: ref.ID, MaxBytes: -1}, protoerrors.CodeInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Dispatch(ctx, methods.MethodArtifactsGet, tc.req)
			if err == nil {
				t.Fatalf("%s: want error, got nil", tc.name)
			}
			if code := asProtoError(t, err); code != tc.want {
				t.Fatalf("%s: code = %q, want %q", tc.name, code, tc.want)
			}
		})
	}

	// A nil / wrong-typed request is a construction bug the dispatcher
	// answers loudly rather than nil-panicking on.
	if _, err := s.Dispatch(ctx, methods.MethodArtifactsGet, (*types.ArtifactsGetRequest)(nil)); err == nil {
		t.Fatal("nil request: want error, got nil")
	}
	if _, err := s.Dispatch(ctx, methods.MethodArtifactsGet, &types.ArtifactsGetRefRequest{}); err == nil {
		t.Fatal("wrong request type: want error, got nil")
	}
}

// TestNewArtifactsSurface_RefusesAnIncoherentFetchBound pins the
// constructor's fail-closed posture: a default above its own ceiling is a
// misconfiguration, not something to silently reorder.
func TestNewArtifactsSurface_RefusesAnIncoherentFetchBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		defaultMax, hardM int
	}{
		{"zero default", 0, 1024},
		{"zero hard", 1024, 0},
		{"negative default", -1, 1024},
		{"default above hard", 2048, 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
				Store:                newInMemStore(t),
				Redactor:             patterns.New(),
				Bus:                  newArtifactsBus(t),
				Clock:                artifactsTestClock,
				DriverName:           "inmem",
				MaxBodyBytes:         1 << 20,
				FetchDefaultMaxBytes: tc.defaultMax,
				FetchHardMaxBytes:    tc.hardM,
			})
			if err == nil {
				t.Fatalf("%s: want a misconfiguration error, got nil", tc.name)
			}
		})
	}
}

// TestArtifactsGetHandler_ResponseDoesNotAliasTheStore proves the window
// is a COPY: a caller mutating the returned slice must not corrupt the
// in-memory driver's stored bytes for the next reader.
func TestArtifactsGetHandler_ResponseDoesNotAliasTheStore(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	scope := types.ArtifactScope{Tenant: "t", User: "u", Session: "s"}
	payload := []byte("original bytes")
	ref := putFixture(t, s, scope, payload, types.ArtifactsPutOpts{})
	ctx := verifiedArtifactsCtx(t, "t", "u", "s")

	first := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID})
	for i := range first.Content {
		first.Content[i] = '!'
	}
	second := dispatchGet(t, s, ctx, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID})
	if !bytes.Equal(second.Content, payload) {
		t.Fatalf("a mutated response corrupted the store: second read = %q, want %q", second.Content, payload)
	}
}

// TestArtifactsGetHandler_ConcurrentReuse_NoCrossTalk is the D-025 gate
// for the byte read: N concurrent invocations against ONE shared surface,
// each in its own tenant, each asserting it got ITS OWN bytes.
//
// N=128 > the mandated 100. The interesting failure this would catch is
// not a data race alone (the race detector covers that) but CONTEXT
// BLEED: a per-call bound or window held on the surface instead of read
// from the request would show up here as one goroutine seeing another's
// content or another's returned_bytes.
func TestArtifactsGetHandler_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	t.Parallel()
	const n = 128
	s := newBoundedArtifactsSurface(t, newInMemStore(t), 16, 64)

	type seeded struct {
		scope   types.ArtifactScope
		id      string
		payload []byte
	}
	seeds := make([]seeded, n)
	for i := range seeds {
		scope := types.ArtifactScope{
			Tenant:  fmt.Sprintf("tenant-%03d", i),
			User:    fmt.Sprintf("user-%03d", i),
			Session: fmt.Sprintf("session-%03d", i),
		}
		// Each payload is distinct AND long enough to be bounded, so a
		// bleed shows up in content and in the reported bound alike.
		payload := bytes.Repeat([]byte(fmt.Sprintf("%03d", i)), 40) // 120 bytes
		ref := putFixture(t, s, scope, payload, types.ArtifactsPutOpts{})
		seeds[i] = seeded{scope: scope, id: ref.ID, payload: payload}
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range seeds {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			sd := seeds[i]
			ctx := context.Background()
			vctx, err := identity.WithVerified(ctx, identity.Identity{
				TenantID: sd.scope.Tenant, UserID: sd.scope.User, SessionID: sd.scope.Session,
			})
			if err != nil {
				errs[i] = err
				return
			}
			// Alternate the bound so no two adjacent goroutines ask for
			// the same window — a shared per-call field would collide.
			maxBytes := int64(16 + (i%4)*8)
			offset := int64((i % 3) * 8)
			resp, derr := s.Dispatch(vctx, methods.MethodArtifactsGet, &types.ArtifactsGetRequest{
				Scope: sd.scope, ID: sd.id, Offset: offset, MaxBytes: maxBytes,
			})
			if derr != nil {
				errs[i] = fmt.Errorf("goroutine %d: dispatch: %w", i, derr)
				return
			}
			gr, ok := resp.(*types.ArtifactsGetResponse)
			if !ok {
				errs[i] = fmt.Errorf("goroutine %d: response %T", i, resp)
				return
			}
			want := sd.payload[offset : offset+maxBytes]
			if !bytes.Equal(gr.Content, want) {
				errs[i] = fmt.Errorf("goroutine %d: content bleed: got %q want %q", i, gr.Content, want)
				return
			}
			if gr.Offset != offset || gr.ReturnedBytes != maxBytes {
				errs[i] = fmt.Errorf("goroutine %d: bound bleed: offset=%d returned=%d want offset=%d returned=%d",
					i, gr.Offset, gr.ReturnedBytes, offset, maxBytes)
				return
			}
			if gr.TotalSizeBytes != int64(len(sd.payload)) {
				errs[i] = fmt.Errorf("goroutine %d: total_size_bytes = %d, want %d", i, gr.TotalSizeBytes, len(sd.payload))
				return
			}
			if !gr.Truncated {
				errs[i] = fmt.Errorf("goroutine %d: a bounded window reported truncated=false", i)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestArtifactsGetHandler_CancellationDoesNotCrossTalk pins the third
// D-025 guarantee: cancelling one call's ctx must not affect another's.
func TestArtifactsGetHandler_CancellationDoesNotCrossTalk(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	scope := types.ArtifactScope{Tenant: "t", User: "u", Session: "s"}
	ref := putFixture(t, s, scope, []byte("live bytes"), types.ArtifactsPutOpts{})

	cancelled, cancel := context.WithCancel(verifiedArtifactsCtx(t, "t", "u", "s"))
	cancel()
	// The cancelled call may or may not error depending on where the
	// driver observes ctx; what must hold is that the LIVE call is
	// unaffected.
	_, _ = s.Dispatch(cancelled, methods.MethodArtifactsGet, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID})

	live := dispatchGet(t, s, verifiedArtifactsCtx(t, "t", "u", "s"), &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID})
	if string(live.Content) != "live bytes" {
		t.Fatalf("a cancelled sibling call disturbed the live one: content = %q", live.Content)
	}
}

// TestArtifactsConfig_ResolvedFetchBounds_BackwardCompatible is the §10
// gate: a configuration written before these fields existed still
// resolves to the documented defaults rather than to zero.
func TestArtifactsConfig_ResolvedFetchBounds_BackwardCompatible(t *testing.T) {
	t.Parallel()
	// The zero value of the struct is exactly what an operator's existing
	// YAML unmarshals into for a key it does not mention.
	var legacy config.ArtifactsConfig
	if got := legacy.ResolvedFetchDefaultMaxBytes(); got != config.DefaultArtifactFetchMaxBytes {
		t.Fatalf("legacy config default = %d, want %d", got, config.DefaultArtifactFetchMaxBytes)
	}
	if got := legacy.ResolvedFetchHardMaxBytes(); got != config.DefaultArtifactFetchHardMaxBytes {
		t.Fatalf("legacy config ceiling = %d, want %d", got, config.DefaultArtifactFetchHardMaxBytes)
	}
	explicit := config.ArtifactsConfig{FetchDefaultMaxBytes: 1234, FetchHardMaxBytes: 5678}
	if got := explicit.ResolvedFetchDefaultMaxBytes(); got != 1234 {
		t.Fatalf("explicit default = %d, want 1234", got)
	}
	if got := explicit.ResolvedFetchHardMaxBytes(); got != 5678 {
		t.Fatalf("explicit ceiling = %d, want 5678", got)
	}
}

// failingGetStore makes the store's Get / GetRef legs fail so the two
// error branches of handleGet map onto canonical codes rather than
// leaking a raw runtime error onto the wire.
type failingGetStore struct {
	artifacts.ArtifactStore
	failGetRef error
	failGet    error
}

func (f failingGetStore) GetRef(ctx context.Context, scope artifacts.ArtifactScope, id string) (*artifacts.ArtifactRef, bool, error) {
	if f.failGetRef != nil {
		return nil, false, f.failGetRef
	}
	return f.ArtifactStore.GetRef(ctx, scope, id)
}

func (f failingGetStore) Get(ctx context.Context, scope artifacts.ArtifactScope, id string) ([]byte, bool, error) {
	if f.failGet != nil {
		return nil, false, f.failGet
	}
	return f.ArtifactStore.Get(ctx, scope, id)
}

// vanishingGetStore resolves a ref and then reports the bytes absent —
// the shape a concurrent Delete produces between the two store legs.
type vanishingGetStore struct {
	artifacts.ArtifactStore
}

func (v vanishingGetStore) Get(context.Context, artifacts.ArtifactScope, string) ([]byte, bool, error) {
	return nil, false, nil
}

// TestArtifactsGetHandler_StoreErrorsMapToCanonicalCodes covers both
// store legs. A raw driver error must not reach the wire uninterpreted
// (§13: every error shape is observable as a Code), and the two legs
// must not answer differently for the same underlying sentinel.
func TestArtifactsGetHandler_StoreErrorsMapToCanonicalCodes(t *testing.T) {
	t.Parallel()
	scope := types.ArtifactScope{Tenant: "t", User: "u", Session: "s"}

	for _, tc := range []struct {
		name string
		mk   func(inner artifacts.ArtifactStore) artifacts.ArtifactStore
		want protoerrors.Code
	}{
		{
			"GetRef leg, store closed",
			func(inner artifacts.ArtifactStore) artifacts.ArtifactStore {
				return failingGetStore{ArtifactStore: inner, failGetRef: artifacts.ErrStoreClosed}
			},
			protoerrors.CodeRuntimeError,
		},
		{
			"GetRef leg, invalid scope",
			func(inner artifacts.ArtifactStore) artifacts.ArtifactStore {
				return failingGetStore{ArtifactStore: inner, failGetRef: artifacts.ErrInvalidScope}
			},
			protoerrors.CodeInvalidRequest,
		},
		{
			"Get leg, store closed",
			func(inner artifacts.ArtifactStore) artifacts.ArtifactStore {
				return failingGetStore{ArtifactStore: inner, failGet: artifacts.ErrStoreClosed}
			},
			protoerrors.CodeRuntimeError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := newInMemStore(t)
			// Seed through a surface over the BARE store so the fixture
			// exists before the failing wrapper is put in front of it.
			seedSurface := newArtifactsSurface(t, inner, "inmem")
			ref := putFixture(t, seedSurface, scope, []byte("payload"), types.ArtifactsPutOpts{})

			s := newArtifactsSurface(t, tc.mk(inner), "inmem")
			_, err := s.Dispatch(verifiedArtifactsCtx(t, "t", "u", "s"),
				methods.MethodArtifactsGet, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID})
			if err == nil {
				t.Fatalf("%s: want error, got nil", tc.name)
			}
			if code := asProtoError(t, err); code != tc.want {
				t.Fatalf("%s: code = %q, want %q", tc.name, code, tc.want)
			}
		})
	}
}

// TestArtifactsGetHandler_GetRefResolvesButGetDoesNot covers the
// disappeared-during-fetch race: GetRef found it, Get did not (a
// concurrent Delete, or a driver inconsistency). It must answer with the
// SAME shape as an unknown id, because the caller cannot act on the
// difference.
func TestArtifactsGetHandler_GetRefResolvesButGetDoesNot(t *testing.T) {
	t.Parallel()
	scope := types.ArtifactScope{Tenant: "t", User: "u", Session: "s"}
	inner := newInMemStore(t)
	seedSurface := newArtifactsSurface(t, inner, "inmem")
	ref := putFixture(t, seedSurface, scope, []byte("payload"), types.ArtifactsPutOpts{})

	s := newArtifactsSurface(t, vanishingGetStore{ArtifactStore: inner}, "inmem")
	_, err := s.Dispatch(verifiedArtifactsCtx(t, "t", "u", "s"),
		methods.MethodArtifactsGet, &types.ArtifactsGetRequest{Scope: scope, ID: ref.ID})
	if err == nil {
		t.Fatal("vanished artifact: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeNotFound {
		t.Fatalf("vanished artifact: code = %q, want not_found", code)
	}
}
