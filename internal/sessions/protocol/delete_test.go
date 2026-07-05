package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
)

// fakeEraser is the deterministic stand-in for the cascade orchestrator
// the Service-level Delete tests need. It is NOT a re-implementation of
// the cascade (CLAUDE.md §17.4) — the integration test drives the real
// CascadeEraser over real stores. It records the identity it was called
// with and returns a configurable result.
type fakeEraser struct {
	calledWith identity.Identity
	resp       prototypes.SessionsDeleteResponse
	err        error
}

func (f *fakeEraser) Erase(_ context.Context, id identity.Identity) (prototypes.SessionsDeleteResponse, error) {
	f.calledWith = id
	if f.err != nil {
		return prototypes.SessionsDeleteResponse{}, f.err
	}
	return f.resp, nil
}

// scope builds a wire IdentityScope. The current scope-matrix cases fix the
// tenant and vary user/session; the param stays explicit to document the
// triple shape.
func scope(tenant, u, s string) prototypes.IdentityScope { //nolint:unparam // tenant fixed by the current cases; kept explicit for the triple shape
	return prototypes.IdentityScope{Tenant: tenant, User: u, Session: s}
}

// TestService_Delete_OwnSession_Success pins the happy path: a valid
// identity reaches the eraser, which is called with exactly the caller's
// verified triple, and the deletion telemetry is returned.
func TestService_Delete_OwnSession_Success(t *testing.T) {
	t.Parallel()
	er := &fakeEraser{resp: prototypes.SessionsDeleteResponse{
		SessionID: "s1", Deleted: true, StateRecordsDeleted: 3, ArtifactsDeleted: 1, MemoryPurged: true,
	}}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithEraser(er))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := svc.Delete(context.Background(), prototypes.SessionsDeleteRequest{Identity: scope("t1", "u1", "s1")})
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !resp.Deleted || resp.StateRecordsDeleted != 3 || resp.ArtifactsDeleted != 1 {
		t.Errorf("response = %+v, want the eraser's telemetry", resp)
	}
	if er.calledWith != (identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}) {
		t.Errorf("eraser called with %+v, want the verified triple", er.calledWith)
	}
}

// TestService_Delete_IncompleteIdentity_Rejected pins the
// identity-mandatory gate: an incomplete triple fails closed with
// ErrIdentityRequired and the eraser is NEVER reached.
func TestService_Delete_IncompleteIdentity_Rejected(t *testing.T) {
	t.Parallel()
	er := &fakeEraser{}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithEraser(er))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, derr := svc.Delete(context.Background(), prototypes.SessionsDeleteRequest{Identity: scope("t1", "", "s1")})
	if !errors.Is(derr, sessionsprotocol.ErrIdentityRequired) {
		t.Fatalf("Delete err=%v, want ErrIdentityRequired", derr)
	}
	if er.calledWith != (identity.Identity{}) {
		t.Errorf("eraser was reached on an incomplete identity: %+v", er.calledWith)
	}
}

// TestService_Delete_RunningTask_Mapped pins the running-task refusal:
// the eraser's sessions.ErrSessionRunning is mapped to the Service's
// ErrSessionRunning (the handler turns it into a 409).
func TestService_Delete_RunningTask_Mapped(t *testing.T) {
	t.Parallel()
	er := &fakeEraser{err: sessions.ErrSessionRunning}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithEraser(er))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, derr := svc.Delete(context.Background(), prototypes.SessionsDeleteRequest{Identity: scope("t1", "u1", "s1")})
	if !errors.Is(derr, sessionsprotocol.ErrSessionRunning) {
		t.Fatalf("Delete err=%v, want ErrSessionRunning", derr)
	}
}

// TestService_Delete_NotFound_Mapped pins the absent-session mapping.
func TestService_Delete_NotFound_Mapped(t *testing.T) {
	t.Parallel()
	er := &fakeEraser{err: sessions.ErrSessionNotFound}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithEraser(er))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, derr := svc.Delete(context.Background(), prototypes.SessionsDeleteRequest{Identity: scope("t1", "u1", "s1")})
	if !errors.Is(derr, sessionsprotocol.ErrSessionNotFound) {
		t.Fatalf("Delete err=%v, want ErrSessionNotFound", derr)
	}
}

// TestService_Delete_ErasureRecordFailed_Mapped pins D-286's new mapping:
// the eraser's sessions.ErrErasureRecordFailed (destructive steps done,
// the durable record-of-fact could not be completed) is mapped to the
// Service's own ErrErasureRecordFailed (the handler turns it into a
// 500 naming the record failure, distinct from the generic default).
func TestService_Delete_ErasureRecordFailed_Mapped(t *testing.T) {
	t.Parallel()
	er := &fakeEraser{err: sessions.ErrErasureRecordFailed}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithEraser(er))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, derr := svc.Delete(context.Background(), prototypes.SessionsDeleteRequest{Identity: scope("t1", "u1", "s1")})
	if !errors.Is(derr, sessionsprotocol.ErrErasureRecordFailed) {
		t.Fatalf("Delete err=%v, want ErrErasureRecordFailed", derr)
	}
}

// TestService_Delete_NoEraser_Unsupported pins capability gating: a
// Service built without an eraser answers ErrErasureUnsupported (the
// handler maps it to a 404) and HasEraser reports false.
func TestService_Delete_NoEraser_Unsupported(t *testing.T) {
	t.Parallel()
	svc, err := sessionsprotocol.NewService(&fakeProjector{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc.HasEraser() {
		t.Error("HasEraser() = true on a Service built without an eraser")
	}
	_, derr := svc.Delete(context.Background(), prototypes.SessionsDeleteRequest{Identity: scope("t1", "u1", "s1")})
	if !errors.Is(derr, sessionsprotocol.ErrErasureUnsupported) {
		t.Fatalf("Delete err=%v, want ErrErasureUnsupported", derr)
	}
}
