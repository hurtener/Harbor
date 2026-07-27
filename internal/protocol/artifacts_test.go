package protocol_test

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// artifactsTestClock is the deterministic clock the artifacts surface tests use
// so the get_ref ExpiresAt stamp is reproducible.
func artifactsTestClock() time.Time {
	return time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
}

// stubPresigner is a test-only ArtifactStore that wraps the in-mem
// driver and additionally implements artifacts.Presigner. It emits a
// deterministic URL so the resolver call site can be exercised end-to-
// end. THIS STUB IS TEST-ONLY — it lives in *_test.go, is never
// registered as a driver, and is never reachable from the production
// binary (CLAUDE.md §13 test-stub posture).
type stubPresigner struct {
	artifacts.ArtifactStore
}

func (s stubPresigner) PresignGet(_ context.Context, _ artifacts.ArtifactScope, id string, expiry time.Duration) (string, error) {
	if expiry < types.PresignExpiryMin || expiry > types.PresignExpiryMax {
		return "", fmt.Errorf("stub presigner: expiry out of range")
	}
	return "https://test-presigner.invalid/" + id + "?expires=" + fmt.Sprint(int64(expiry/time.Second)), nil
}

// newArtifactsBus builds a real in-mem event bus for the surface tests.
func newArtifactsBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// newInMemStore builds a fresh in-mem artifact store.
func newInMemStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// newArtifactsSurface builds an ArtifactsSurface over the given store.
func newArtifactsSurface(t *testing.T, store artifacts.ArtifactStore, driverName string) *protocol.ArtifactsSurface {
	t.Helper()
	s, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
		Store:                store,
		Redactor:             patterns.New(),
		Bus:                  newArtifactsBus(t),
		Clock:                artifactsTestClock,
		DriverName:           driverName,
		MaxBodyBytes:         1 << 20,
		FetchDefaultMaxBytes: config.DefaultArtifactFetchMaxBytes,
		FetchHardMaxBytes:    config.DefaultArtifactFetchHardMaxBytes,
	})
	if err != nil {
		t.Fatalf("NewArtifactsSurface: %v", err)
	}
	return s
}

// asProtoError extracts the canonical Code from a Dispatch error.
func asProtoError(t *testing.T, err error) protoerrors.Code {
	t.Helper()
	var perr *protoerrors.Error
	if !stderrors.As(err, &perr) {
		t.Fatalf("error %v is not a *protoerrors.Error", err)
	}
	return perr.Code
}

func putFixture(t *testing.T, s *protocol.ArtifactsSurface, scope types.ArtifactScope, bytes []byte, opts types.ArtifactsPutOpts) types.ArtifactRef {
	t.Helper()
	resp, err := s.Dispatch(context.Background(), methods.MethodArtifactsPut, &types.ArtifactsPutRequest{
		Scope: scope,
		Bytes: bytes,
		Opts:  opts,
	})
	if err != nil {
		t.Fatalf("artifacts.put: %v", err)
	}
	pr, ok := resp.(*types.ArtifactsPutResponse)
	if !ok {
		t.Fatalf("artifacts.put: response %T, want *types.ArtifactsPutResponse", resp)
	}
	return pr.Ref
}

func TestNewArtifactsSurface_FailsLoudOnMissingDep(t *testing.T) {
	t.Parallel()
	_, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{})
	if err == nil {
		t.Fatal("NewArtifactsSurface with empty deps: want error, got nil")
	}
	if !stderrors.Is(err, protocol.ErrArtifactsMisconfigured) {
		t.Fatalf("error %v does not wrap ErrArtifactsMisconfigured", err)
	}
}

func TestArtifactsListHandler_FilterShape_Extends(t *testing.T) {
	t.Parallel()
	store := newInMemStore(t)
	s := newArtifactsSurface(t, store, "inmem")
	scope := types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"}

	putFixture(t, s, scope, []byte("small text"), types.ArtifactsPutOpts{
		MimeType: "text/plain", Tags: []string{"alpha"}, Source: types.ArtifactSourceUserUpload,
	})
	putFixture(t, s, scope, []byte("a much larger image payload xxxxxxxxxxxxxxxx"), types.ArtifactsPutOpts{
		MimeType: "image/png", Tags: []string{"beta"}, Source: types.ArtifactSourceTool,
	})

	// MIME filter narrows to one row.
	resp, err := s.Dispatch(context.Background(), methods.MethodArtifactsList, &types.ArtifactsListRequest{
		Scope:    scope,
		MimeType: []string{"image/png"},
	})
	if err != nil {
		t.Fatalf("artifacts.list: %v", err)
	}
	lr := resp.(*types.ArtifactsListResponse)
	if len(lr.Rows) != 1 || lr.Rows[0].Ref.MimeType != "image/png" {
		t.Fatalf("mime filter: got %d rows, want 1 image/png row", len(lr.Rows))
	}

	// Source filter narrows to the tool row.
	resp, err = s.Dispatch(context.Background(), methods.MethodArtifactsList, &types.ArtifactsListRequest{
		Scope:  scope,
		Source: []types.ArtifactSource{types.ArtifactSourceTool},
	})
	if err != nil {
		t.Fatalf("artifacts.list source filter: %v", err)
	}
	lr = resp.(*types.ArtifactsListResponse)
	if len(lr.Rows) != 1 || lr.Rows[0].Source != types.ArtifactSourceTool {
		t.Fatalf("source filter: got %d rows, want 1 tool row", len(lr.Rows))
	}

	// Tag filter narrows to the alpha row.
	resp, err = s.Dispatch(context.Background(), methods.MethodArtifactsList, &types.ArtifactsListRequest{
		Scope: scope,
		Tags:  []string{"alpha"},
	})
	if err != nil {
		t.Fatalf("artifacts.list tag filter: %v", err)
	}
	lr = resp.(*types.ArtifactsListResponse)
	if len(lr.Rows) != 1 {
		t.Fatalf("tag filter: got %d rows, want 1", len(lr.Rows))
	}

	// Size filter (min 20 bytes) narrows to the image row.
	minBytes := int64(20)
	resp, err = s.Dispatch(context.Background(), methods.MethodArtifactsList, &types.ArtifactsListRequest{
		Scope:     scope,
		SizeRange: &types.SizeRange{MinBytes: &minBytes},
	})
	if err != nil {
		t.Fatalf("artifacts.list size filter: %v", err)
	}
	lr = resp.(*types.ArtifactsListResponse)
	if len(lr.Rows) != 1 || lr.Rows[0].Ref.MimeType != "image/png" {
		t.Fatalf("size filter: got %d rows, want 1 image row", len(lr.Rows))
	}
}

func TestArtifactsListHandler_RejectsUnknownSource(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	_, err := s.Dispatch(context.Background(), methods.MethodArtifactsList, &types.ArtifactsListRequest{
		Scope:  types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"},
		Source: []types.ArtifactSource{"bogus"},
	})
	if err == nil {
		t.Fatal("unknown source: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeInvalidRequest {
		t.Fatalf("unknown source: code = %q, want invalid_request", code)
	}
}

func TestArtifactsListHandler_RejectsMissingTenant(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	_, err := s.Dispatch(context.Background(), methods.MethodArtifactsList, &types.ArtifactsListRequest{
		Scope: types.ArtifactScope{},
	})
	if err == nil {
		t.Fatal("missing tenant: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeIdentityRequired {
		t.Fatalf("missing tenant: code = %q, want identity_required", code)
	}
}

func TestArtifactsListHandler_RejectsCrossTenant_WithoutAdmin(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	// Verified identity = tenant A; request scope = tenant B; no admin.
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: "tenant-a", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	_, err = s.Dispatch(ctx, methods.MethodArtifactsList, &types.ArtifactsListRequest{
		Scope: types.ArtifactScope{Tenant: "tenant-b", User: "u1", Session: "s1"},
	})
	if err == nil {
		t.Fatal("cross-tenant list without admin: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeScopeMismatch {
		t.Fatalf("cross-tenant list: code = %q, want scope_mismatch", code)
	}
}

func TestArtifactsListHandler_AllowsCrossTenant_WithAdmin(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: "tenant-a", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
	_, err = s.Dispatch(ctx, methods.MethodArtifactsList, &types.ArtifactsListRequest{
		Scope: types.ArtifactScope{Tenant: "tenant-b", User: "u1", Session: "s1"},
	})
	if err != nil {
		t.Fatalf("cross-tenant list with admin: unexpected error %v", err)
	}
}

// listOwnerIDs seeds two users' artifacts in one tenant and returns the
// surface plus the set of ids each user owns, so a listing assertion can
// name WHOSE rows came back rather than only counting them.
func seedTwoOwners(t *testing.T) (s *protocol.ArtifactsSurface, mine, theirs map[string]bool) {
	t.Helper()
	s = newArtifactsSurface(t, newInMemStore(t), "inmem")
	mine, theirs = map[string]bool{}, map[string]bool{}

	// The caller's own rows, deliberately spread across TWO of the
	// caller's own sessions — an elided session must still return both.
	for _, sess := range []string{"s-mine-1", "s-mine-2"} {
		ref := putFixture(t, s, types.ArtifactScope{Tenant: "t-x", User: "u-mine", Session: sess},
			[]byte("mine "+sess), types.ArtifactsPutOpts{MimeType: "text/plain"})
		mine[ref.ID] = true
	}
	// A DIFFERENT user's row in the SAME tenant.
	ref := putFixture(t, s, types.ArtifactScope{Tenant: "t-x", User: "u-theirs", Session: "s-theirs"},
		[]byte("theirs"), types.ArtifactsPutOpts{MimeType: "text/plain"})
	theirs[ref.ID] = true
	return s, mine, theirs
}

// listAs dispatches artifacts.list under a verified identity plus the
// given scope claims and returns the response rows.
func listAs(t *testing.T, s *protocol.ArtifactsSurface, verified identity.Identity,
	scopes []auth.Scope, reqScope types.ArtifactScope,
) ([]types.ArtifactRow, error) {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), verified)
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	if len(scopes) > 0 {
		ctx = auth.WithScopes(ctx, scopes)
	}
	resp, err := s.Dispatch(ctx, methods.MethodArtifactsList, &types.ArtifactsListRequest{Scope: reqScope})
	if err != nil {
		return nil, err
	}
	lr, ok := resp.(*types.ArtifactsListResponse)
	if !ok {
		t.Fatalf("artifacts.list: response %T, want *types.ArtifactsListResponse", resp)
	}
	return lr.Rows, nil
}

// TestArtifactsListHandler_ElidedUser_FoldsToCaller pins the listing's
// identity bound on the axis that carries it: a caller who names only a
// tenant gets THEIR OWN artifacts, never the tenant's. Counting rows is
// not enough — the assertion names the owning user on every row, because
// a listing row carries the owner's user id, session id and content
// digest, and those are the caller's to see only for the caller's own.
//
// Mirrors TestEventsList_CrossUserSameTenantWithoutScope_403 in
// internal/protocol/transports/stream: same widening, same claim.
func TestArtifactsListHandler_ElidedUser_FoldsToCaller(t *testing.T) {
	t.Parallel()
	s, mine, theirs := seedTwoOwners(t)
	caller := identity.Identity{TenantID: "t-x", UserID: "u-mine", SessionID: "s-mine-1"}

	rows, err := listAs(t, s, caller, nil, types.ArtifactScope{Tenant: "t-x"})
	if err != nil {
		t.Fatalf("tenant-only list: unexpected error %v", err)
	}
	if len(rows) != len(mine) {
		t.Fatalf("tenant-only list returned %d rows, want %d (the caller's own)", len(rows), len(mine))
	}
	for _, r := range rows {
		if theirs[r.Ref.ID] {
			t.Fatalf("tenant-only list returned another user's row: id=%s scope.user=%s",
				r.Ref.ID, r.Ref.Scope.User)
		}
		if r.Ref.Scope.User != caller.UserID {
			t.Fatalf("row %s has scope.user %q, want the caller's %q",
				r.Ref.ID, r.Ref.Scope.User, caller.UserID)
		}
		if !mine[r.Ref.ID] {
			t.Fatalf("row %s is not one of the caller's seeded artifacts", r.Ref.ID)
		}
	}
}

// TestArtifactsListHandler_NamedForeignUser_RequiresClaim pins the other
// half of the same bound: naming somebody else outright is refused with
// the events listing's code, not silently narrowed to an empty page.
func TestArtifactsListHandler_NamedForeignUser_RequiresClaim(t *testing.T) {
	t.Parallel()
	s, _, _ := seedTwoOwners(t)
	caller := identity.Identity{TenantID: "t-x", UserID: "u-mine", SessionID: "s-mine-1"}

	_, err := listAs(t, s, caller, nil, types.ArtifactScope{Tenant: "t-x", User: "u-theirs"})
	if err == nil {
		t.Fatal("cross-user list without a claim: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("cross-user list: code = %q, want identity_scope_required", code)
	}
}

// TestArtifactsListHandler_CrossUser_PermittedWithClaim proves the claim
// is what widens: BOTH admin-tier claims reopen the tenant-wide fan-in
// (elided user) and the named-foreign-user read. Without this the fold
// above could be an unconditional narrowing rather than a gate.
func TestArtifactsListHandler_CrossUser_PermittedWithClaim(t *testing.T) {
	t.Parallel()
	for _, claim := range []auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet} {
		t.Run(string(claim), func(t *testing.T) {
			t.Parallel()
			s, mine, theirs := seedTwoOwners(t)
			caller := identity.Identity{TenantID: "t-x", UserID: "u-mine", SessionID: "s-mine-1"}
			scopes := []auth.Scope{claim}

			// Elided user under the claim = the tenant-wide fan-in.
			rows, err := listAs(t, s, caller, scopes, types.ArtifactScope{Tenant: "t-x"})
			if err != nil {
				t.Fatalf("tenant-wide list with %s: unexpected error %v", claim, err)
			}
			if want := len(mine) + len(theirs); len(rows) != want {
				t.Fatalf("tenant-wide list with %s returned %d rows, want %d", claim, len(rows), want)
			}

			// A named foreign user under the claim resolves to that user.
			rows, err = listAs(t, s, caller, scopes, types.ArtifactScope{Tenant: "t-x", User: "u-theirs"})
			if err != nil {
				t.Fatalf("cross-user list with %s: unexpected error %v", claim, err)
			}
			if len(rows) != len(theirs) {
				t.Fatalf("cross-user list with %s returned %d rows, want %d", claim, len(rows), len(theirs))
			}
			for _, r := range rows {
				if !theirs[r.Ref.ID] {
					t.Fatalf("cross-user list with %s returned an unexpected row %s", claim, r.Ref.ID)
				}
			}
		})
	}
}

// TestArtifactsListHandler_OwnSessionsStayWildcard pins the axis the
// bound deliberately does NOT close: a session is not an isolation
// boundary within one user, so an elided session still spans every
// session of the caller's, and naming one of the caller's OWN other
// sessions needs no claim. The everyday "show me my artifacts" flow —
// the Console Artifacts page — depends on both.
func TestArtifactsListHandler_OwnSessionsStayWildcard(t *testing.T) {
	t.Parallel()
	s, mine, _ := seedTwoOwners(t)
	// The caller is seated in s-mine-1 but seeded rows in s-mine-2 too.
	caller := identity.Identity{TenantID: "t-x", UserID: "u-mine", SessionID: "s-mine-1"}

	// Elided session spans BOTH of the caller's sessions.
	rows, err := listAs(t, s, caller, nil, types.ArtifactScope{Tenant: "t-x", User: "u-mine"})
	if err != nil {
		t.Fatalf("own-user list: unexpected error %v", err)
	}
	if len(rows) != len(mine) {
		t.Fatalf("own-user list returned %d rows, want %d across both own sessions", len(rows), len(mine))
	}

	// Naming the caller's OWN other session is not a crossing.
	rows, err = listAs(t, s, caller, nil, types.ArtifactScope{Tenant: "t-x", Session: "s-mine-2"})
	if err != nil {
		t.Fatalf("own other-session list: unexpected error %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("own other-session list returned %d rows, want 1", len(rows))
	}
	if rows[0].Ref.Scope.Session != "s-mine-2" {
		t.Fatalf("own other-session list returned session %q, want s-mine-2", rows[0].Ref.Scope.Session)
	}

	// A FOREIGN session under the folded own-user filter yields nothing
	// rather than another user's rows — the user fold closes it.
	rows, err = listAs(t, s, caller, nil, types.ArtifactScope{Tenant: "t-x", Session: "s-theirs"})
	if err != nil {
		t.Fatalf("foreign-session list: unexpected error %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("foreign-session list returned %d rows, want 0", len(rows))
	}
}

func TestArtifactsPutHandler_RoundTrip_InMem(t *testing.T) {
	t.Parallel()
	store := newInMemStore(t)
	s := newArtifactsSurface(t, store, "inmem")
	scope := types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"}

	ref := putFixture(t, s, scope, []byte("hello world"), types.ArtifactsPutOpts{
		MimeType: "text/plain", Filename: "greeting.txt", Tags: []string{"x"},
	})
	if ref.ID == "" {
		t.Fatal("put: empty ref ID")
	}
	if ref.SizeBytes != 11 {
		t.Fatalf("put: SizeBytes = %d, want 11", ref.SizeBytes)
	}

	resp, err := s.Dispatch(context.Background(), methods.MethodArtifactsList, &types.ArtifactsListRequest{Scope: scope})
	if err != nil {
		t.Fatalf("artifacts.list: %v", err)
	}
	lr := resp.(*types.ArtifactsListResponse)
	if len(lr.Rows) != 1 {
		t.Fatalf("list after put: got %d rows, want 1", len(lr.Rows))
	}
	if lr.Rows[0].Source != types.ArtifactSourceUserUpload {
		t.Fatalf("default source = %q, want user_upload", lr.Rows[0].Source)
	}
}

func TestArtifactsPutHandler_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	_, err := s.Dispatch(context.Background(), methods.MethodArtifactsPut, &types.ArtifactsPutRequest{
		Scope: types.ArtifactScope{Tenant: "t1"},
		Bytes: []byte("x"),
	})
	if err == nil {
		t.Fatal("put missing identity: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeIdentityRequired {
		t.Fatalf("put missing identity: code = %q, want identity_required", code)
	}
}

func TestArtifactsPutHandler_RejectsScopeMismatch(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: "tenant-a", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	_, err = s.Dispatch(ctx, methods.MethodArtifactsPut, &types.ArtifactsPutRequest{
		Scope: types.ArtifactScope{Tenant: "tenant-b", User: "u1", Session: "s1"},
		Bytes: []byte("x"),
	})
	if err == nil {
		t.Fatal("put cross-tenant: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeScopeMismatch {
		t.Fatalf("put cross-tenant: code = %q, want scope_mismatch", code)
	}
}

func TestArtifactsPutHandler_RejectsOversizeBody(t *testing.T) {
	t.Parallel()
	store := newInMemStore(t)
	s, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
		Store:                store,
		Redactor:             patterns.New(),
		Bus:                  newArtifactsBus(t),
		Clock:                artifactsTestClock,
		DriverName:           "inmem",
		MaxBodyBytes:         16,
		FetchDefaultMaxBytes: config.DefaultArtifactFetchMaxBytes,
		FetchHardMaxBytes:    config.DefaultArtifactFetchHardMaxBytes,
	})
	if err != nil {
		t.Fatalf("NewArtifactsSurface: %v", err)
	}
	_, err = s.Dispatch(context.Background(), methods.MethodArtifactsPut, &types.ArtifactsPutRequest{
		Scope: types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"},
		Bytes: []byte("this body is definitely larger than sixteen bytes"),
	})
	if err == nil {
		t.Fatal("oversize body: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeRequestTooLarge {
		t.Fatalf("oversize body: code = %q, want request_too_large", code)
	}
}

func TestArtifactsPutHandler_EmitsArtifactUploaded(t *testing.T) {
	t.Parallel()
	store := newInMemStore(t)
	bus := newArtifactsBus(t)
	s, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
		Store:                store,
		Redactor:             patterns.New(),
		Bus:                  bus,
		Clock:                artifactsTestClock,
		DriverName:           "inmem",
		MaxBodyBytes:         1 << 20,
		FetchDefaultMaxBytes: config.DefaultArtifactFetchMaxBytes,
		FetchHardMaxBytes:    config.DefaultArtifactFetchHardMaxBytes,
	})
	if err != nil {
		t.Fatalf("NewArtifactsSurface: %v", err)
	}

	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}}
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: q.TenantID, User: q.UserID, Session: q.SessionID,
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}

	_, err = s.Dispatch(context.Background(), methods.MethodArtifactsPut, &types.ArtifactsPutRequest{
		Scope: types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"},
		Bytes: []byte("uploaded payload"),
	})
	if err != nil {
		t.Fatalf("artifacts.put: %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != protocol.EventTypeArtifactUploaded {
			t.Fatalf("event type = %q, want %q", ev.Type, protocol.EventTypeArtifactUploaded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for artifacts.uploaded event")
	}
}

func TestArtifactsGetRefHandler_ReturnsPresignUnsupported_InMem(t *testing.T) {
	t.Parallel()
	store := newInMemStore(t)
	s := newArtifactsSurface(t, store, "inmem")
	scope := types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"}
	ref := putFixture(t, s, scope, []byte("payload"), types.ArtifactsPutOpts{})

	_, err := s.Dispatch(context.Background(), methods.MethodArtifactsGetRef, &types.ArtifactsGetRefRequest{
		Scope: scope, ID: ref.ID,
	})
	if err == nil {
		t.Fatal("get_ref on in-mem driver: want CodePresignUnsupported, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodePresignUnsupported {
		t.Fatalf("get_ref on in-mem: code = %q, want presign_unsupported", code)
	}
}

func TestArtifactsGetRefHandler_ReturnsPresigned_S3LikeDriver(t *testing.T) {
	t.Parallel()
	store := stubPresigner{ArtifactStore: newInMemStore(t)}
	s := newArtifactsSurface(t, store, "s3-stub")
	scope := types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"}
	ref := putFixture(t, s, scope, []byte("payload"), types.ArtifactsPutOpts{})

	resp, err := s.Dispatch(context.Background(), methods.MethodArtifactsGetRef, &types.ArtifactsGetRefRequest{
		Scope: scope, ID: ref.ID, Expiry: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("get_ref on presigner driver: %v", err)
	}
	gr := resp.(*types.ArtifactsGetRefResponse)
	if gr.PresignedURL == "" {
		t.Fatal("get_ref: empty presigned URL")
	}
	wantExpiry := artifactsTestClock().Add(30 * time.Minute)
	if !gr.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("get_ref ExpiresAt = %v, want %v", gr.ExpiresAt, wantExpiry)
	}
}

func TestArtifactsGetRefHandler_RejectsOutOfRangeExpiry(t *testing.T) {
	t.Parallel()
	store := stubPresigner{ArtifactStore: newInMemStore(t)}
	s := newArtifactsSurface(t, store, "s3-stub")
	scope := types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"}
	ref := putFixture(t, s, scope, []byte("payload"), types.ArtifactsPutOpts{})

	for _, tc := range []struct {
		name   string
		expiry time.Duration
	}{
		{"below floor", 30 * time.Second},
		{"above ceiling", 14 * 24 * time.Hour},
	} {

		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Dispatch(context.Background(), methods.MethodArtifactsGetRef, &types.ArtifactsGetRefRequest{
				Scope: scope, ID: ref.ID, Expiry: tc.expiry,
			})
			if err == nil {
				t.Fatal("out-of-range expiry: want error, got nil")
			}
			if code := asProtoError(t, err); code != protoerrors.CodeInvalidRequest {
				t.Fatalf("out-of-range expiry: code = %q, want invalid_request", code)
			}
		})
	}
}

func TestArtifactsGetRefHandler_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	_, err := s.Dispatch(context.Background(), methods.MethodArtifactsGetRef, &types.ArtifactsGetRefRequest{
		Scope: types.ArtifactScope{Tenant: "t1"}, ID: "x",
	})
	if err == nil {
		t.Fatal("get_ref missing identity: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeIdentityRequired {
		t.Fatalf("get_ref missing identity: code = %q, want identity_required", code)
	}
}

// TestArtifactsGetRefHandler_ForeignTenant_Refused pins the tenant
// reconciliation on artifacts.get_ref. Unlike artifacts.list, this
// method offers NO admin elevation, so the refusal holds for every scope
// claim — the sub-tests carry admin and console:fleet respectively and
// are refused identically. The artifact really exists under tenant-b, so
// a refusal proves the check runs BEFORE the store read rather than
// falling out of a not-found.
func TestArtifactsGetRefHandler_ForeignTenant_Refused(t *testing.T) {
	t.Parallel()
	for name, scopes := range map[string][]auth.Scope{
		"no scopes":     nil,
		"admin":         {auth.ScopeAdmin},
		"console:fleet": {auth.ScopeConsoleFleet},
		"both":          {auth.ScopeAdmin, auth.ScopeConsoleFleet},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := stubPresigner{ArtifactStore: newInMemStore(t)}
			s := newArtifactsSurface(t, store, "s3-stub")
			scope := types.ArtifactScope{Tenant: "tenant-b", User: "u1", Session: "s1"}
			ref := putFixture(t, s, scope, []byte("payload"), types.ArtifactsPutOpts{})

			// Verified identity = tenant A; request scope = tenant B.
			ctx, err := identity.WithVerified(context.Background(), identity.Identity{
				TenantID: "tenant-a", UserID: "u1", SessionID: "s1",
			})
			if err != nil {
				t.Fatalf("identity.With: %v", err)
			}
			if scopes != nil {
				ctx = auth.WithScopes(ctx, scopes)
			}
			_, err = s.Dispatch(ctx, methods.MethodArtifactsGetRef, &types.ArtifactsGetRefRequest{
				Scope: scope, ID: ref.ID, Expiry: 30 * time.Minute,
			})
			if err == nil {
				t.Fatal("foreign-tenant get_ref: want error, got nil")
			}
			if code := asProtoError(t, err); code != protoerrors.CodeScopeMismatch {
				t.Fatalf("foreign-tenant get_ref: code = %q, want scope_mismatch", code)
			}
		})
	}
}

// TestArtifactsGetRefHandler_SameTenant_Allowed pins the golden path:
// a verified identity resolving a ref in its own tenant is unaffected.
func TestArtifactsGetRefHandler_SameTenant_Allowed(t *testing.T) {
	t.Parallel()
	store := stubPresigner{ArtifactStore: newInMemStore(t)}
	s := newArtifactsSurface(t, store, "s3-stub")
	scope := types.ArtifactScope{Tenant: "tenant-a", User: "u1", Session: "s1"}
	ref := putFixture(t, s, scope, []byte("payload"), types.ArtifactsPutOpts{})

	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: "tenant-a", UserID: "u1", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	resp, err := s.Dispatch(ctx, methods.MethodArtifactsGetRef, &types.ArtifactsGetRefRequest{
		Scope: scope, ID: ref.ID, Expiry: 30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("same-tenant get_ref: unexpected error %v", err)
	}
	if gr := resp.(*types.ArtifactsGetRefResponse); gr.PresignedURL == "" {
		t.Fatal("same-tenant get_ref: empty presigned URL")
	}
}

func TestArtifactsGetRefHandler_NotFound(t *testing.T) {
	t.Parallel()
	store := stubPresigner{ArtifactStore: newInMemStore(t)}
	s := newArtifactsSurface(t, store, "s3-stub")
	_, err := s.Dispatch(context.Background(), methods.MethodArtifactsGetRef, &types.ArtifactsGetRefRequest{
		Scope: types.ArtifactScope{Tenant: "t1", User: "u1", Session: "s1"}, ID: "default_deadbeef0000",
	})
	if err == nil {
		t.Fatal("get_ref missing artifact: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeNotFound {
		t.Fatalf("get_ref missing artifact: code = %q, want not_found", code)
	}
}

func TestArtifactsSurface_RejectsNonArtifactsMethod(t *testing.T) {
	t.Parallel()
	s := newArtifactsSurface(t, newInMemStore(t), "inmem")
	_, err := s.Dispatch(context.Background(), methods.MethodStart, &types.ArtifactsListRequest{})
	if err == nil {
		t.Fatal("non-artifacts method: want error, got nil")
	}
	if code := asProtoError(t, err); code != protoerrors.CodeUnknownMethod {
		t.Fatalf("non-artifacts method: code = %q, want unknown_method", code)
	}
}
