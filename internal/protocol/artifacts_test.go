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
		Store:        store,
		Redactor:     patterns.New(),
		Bus:          newArtifactsBus(t),
		Clock:        artifactsTestClock,
		DriverName:   driverName,
		MaxBodyBytes: 1 << 20,
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
	ctx, err := identity.With(context.Background(), identity.Identity{
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
	ctx, err := identity.With(context.Background(), identity.Identity{
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
	ctx, err := identity.With(context.Background(), identity.Identity{
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
		Store:        store,
		Redactor:     patterns.New(),
		Bus:          newArtifactsBus(t),
		Clock:        artifactsTestClock,
		DriverName:   "inmem",
		MaxBodyBytes: 16,
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
		Store:        store,
		Redactor:     patterns.New(),
		Bus:          bus,
		Clock:        artifactsTestClock,
		DriverName:   "inmem",
		MaxBodyBytes: 1 << 20,
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
