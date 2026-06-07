package protocol_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// deleteHarness builds an ArtifactsSurface over an in-mem store while
// holding the bus, so a test can subscribe to the artifacts.deleted audit
// event (Phase 108o / D-187).
func deleteHarness(t *testing.T) (*protocol.ArtifactsSurface, artifacts.ArtifactStore, events.EventBus) {
	t.Helper()
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
	return s, store, bus
}

var deleteScope = types.ArtifactScope{Tenant: "t-del", User: "u-del", Session: "s-del"}

// adminCtx returns a ctx carrying the verified identity matching
// deleteScope + the admin scope claim.
func adminCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: deleteScope.Tenant, UserID: deleteScope.User, SessionID: deleteScope.Session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
}

func TestArtifactsDelete_AdminEvictsAndAudits(t *testing.T) {
	s, store, bus := deleteHarness(t)
	ref := putFixture(t, s, deleteScope, []byte("to be evicted"),
		types.ArtifactsPutOpts{MimeType: "text/plain", Filename: "evict.txt"})

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: deleteScope.Tenant, User: deleteScope.User, Session: deleteScope.Session,
		Types: []events.EventType{protocol.EventTypeArtifactDeleted},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	resp, err := s.Dispatch(adminCtx(t), methods.MethodArtifactsDelete,
		&types.ArtifactsDeleteRequest{Scope: deleteScope, ID: ref.ID})
	if err != nil {
		t.Fatalf("artifacts.delete: %v", err)
	}
	dr, ok := resp.(*types.ArtifactsDeleteResponse)
	if !ok || !dr.Deleted {
		t.Fatalf("delete resp = %+v (%T), want {deleted:true}", resp, resp)
	}

	// The artifact is gone from the store.
	if exists, _ := store.Exists(context.Background(), artifacts.ArtifactScope{
		TenantID: deleteScope.Tenant, UserID: deleteScope.User, SessionID: deleteScope.Session,
	}, ref.ID); exists {
		t.Error("artifact still exists after delete")
	}

	// The audit event fired with the artifact id (never bytes).
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(protocol.ArtifactDeletedPayload)
		if !ok || p.ArtifactID != ref.ID {
			t.Errorf("audit payload = %+v (ok=%v), want artifact_id %q", ev.Payload, ok, ref.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the artifacts.deleted audit event")
	}
}

func TestArtifactsDelete_RequiresAdminScope(t *testing.T) {
	s, _, _ := deleteHarness(t)
	ref := putFixture(t, s, deleteScope, []byte("guarded"), types.ArtifactsPutOpts{})

	// Verified identity but NO admin scope.
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: deleteScope.Tenant, UserID: deleteScope.User, SessionID: deleteScope.Session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	_, derr := s.Dispatch(ctx, methods.MethodArtifactsDelete,
		&types.ArtifactsDeleteRequest{Scope: deleteScope, ID: ref.ID})
	if got := asProtoError(t, derr); got != protoerrors.CodeScopeMismatch {
		t.Fatalf("delete without admin: code = %q, want scope_mismatch", got)
	}
}

func TestArtifactsDelete_IdempotentOnAbsent(t *testing.T) {
	s, _, _ := deleteHarness(t)
	resp, err := s.Dispatch(adminCtx(t), methods.MethodArtifactsDelete,
		&types.ArtifactsDeleteRequest{Scope: deleteScope, ID: "inmem_deadbeefdead"})
	if err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	dr := resp.(*types.ArtifactsDeleteResponse)
	if dr.Deleted {
		t.Error("delete of an absent artifact reported deleted=true, want false (idempotent no-op)")
	}
}

func TestArtifactsDelete_RejectsMissingIdentityAndID(t *testing.T) {
	s, _, _ := deleteHarness(t)
	// Missing session → CodeIdentityRequired.
	_, derr := s.Dispatch(adminCtx(t), methods.MethodArtifactsDelete,
		&types.ArtifactsDeleteRequest{
			Scope: types.ArtifactScope{Tenant: "t-del", User: "u-del"}, ID: "x",
		})
	if got := asProtoError(t, derr); got != protoerrors.CodeIdentityRequired {
		t.Errorf("delete missing-session: code = %q, want identity_required", got)
	}
	// Empty id → CodeInvalidRequest.
	_, derr = s.Dispatch(adminCtx(t), methods.MethodArtifactsDelete,
		&types.ArtifactsDeleteRequest{Scope: deleteScope, ID: ""})
	if got := asProtoError(t, derr); got != protoerrors.CodeInvalidRequest {
		t.Errorf("delete empty-id: code = %q, want invalid_request", got)
	}
}
