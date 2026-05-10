package inprocess_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestSealGroup_FromCancelled_Invalid covers the FSM table entry
// `Cancelled → Sealed` which must be rejected.
func TestSealGroup_FromCancelled_Invalid(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CancelGroup(ctx, g.ID, "x", false); err != nil {
		t.Fatal(err)
	}
	err = r.SealGroup(ctx, g.ID)
	if !errors.Is(err, tasks.ErrGroupInvalidTransition) {
		t.Errorf("seal-after-cancel: err=%v, want ErrGroupInvalidTransition", err)
	}
}

// TestCancelGroup_FromOpen_NoMembers covers the cancel-from-open path
// with no propagate side-effects.
func TestCancelGroup_FromOpen_NoMembers(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CancelGroup(ctx, g.ID, "operator", false); err != nil {
		t.Fatalf("CancelGroup: %v", err)
	}
	groups, err := r.ListGroups(ctx, tripleA().Identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Status != tasks.GroupCancelled {
		t.Errorf("status=%+v, want Cancelled", groups)
	}
}

// TestCancelGroup_NotFound_OnUnknownID hits the not-found branch.
func TestCancelGroup_NotFound_OnUnknownID(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	err := r.CancelGroup(ctx, tasks.TaskGroupID("nope"), "x", true)
	if !errors.Is(err, tasks.ErrGroupNotFound) {
		t.Errorf("err=%v, want ErrGroupNotFound", err)
	}
}

// TestApplyGroup_NotFound_OnUnknownID covers the ApplyGroup not-found
// branch via the dispatch.
func TestApplyGroup_NotFound_OnUnknownID(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	err := r.ApplyGroup(ctx, tasks.TaskGroupID("nope"), tasks.ActionSeal)
	if !errors.Is(err, tasks.ErrGroupNotFound) {
		t.Errorf("err=%v, want ErrGroupNotFound", err)
	}
}

// TestApplyGroup_CrossSession covers the cross-session visibility
// check inside applyResolveAction.
func TestApplyGroup_CrossSession(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	other := identity.Identity{TenantID: "x", UserID: "x", SessionID: "x"}
	otherCtx, _ := identity.With(context.Background(), other)
	err = r.ApplyGroup(otherCtx, g.ID, tasks.ActionResolve)
	if !errors.Is(err, tasks.ErrGroupNotFound) {
		t.Errorf("cross-session ApplyGroup: err=%v, want ErrGroupNotFound", err)
	}
}

// TestResolveOrCreateGroup_CrossSessionLookup hits the
// existence-without-access branch (existing ID, different session).
func TestResolveOrCreateGroup_CrossSessionLookup(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctxA := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctxA, tasks.GroupRequest{
		ID:        tasks.TaskGroupID("shared-id"),
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	other := identity.Identity{TenantID: "T", UserID: "U", SessionID: "other-sess"}
	otherCtx, _ := identity.With(context.Background(), other)
	_, err = r.ResolveOrCreateGroup(otherCtx, tasks.GroupRequest{
		ID:        g.ID,
		SessionID: other,
	})
	if !errors.Is(err, tasks.ErrGroupNotFound) {
		t.Errorf("cross-session lookup: err=%v, want ErrGroupNotFound", err)
	}
}

// TestAcknowledgeBackground_SkipsUnknownTaskIDs hits the
// unknown-task skip branch.
func TestAcknowledgeBackground_SkipsUnknownTaskIDs(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	count, err := r.AcknowledgeBackground(ctx, tripleA().Identity,
		[]tasks.TaskID{tasks.TaskID("ghost")})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("ghost task ack count=%d, want 0", count)
	}
}

// TestAcknowledgeBackground_SkipsForeignTask hits the ctx-visibility
// skip branch.
func TestAcknowledgeBackground_SkipsForeignTask(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	// Spawn a background task in the canonical session.
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindBackground,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
		t.Fatal(err)
	}
	// Ack from a different session — must skip.
	other := identity.Identity{TenantID: "T", UserID: "U", SessionID: "other"}
	otherCtx, _ := identity.With(context.Background(), other)
	count, err := r.AcknowledgeBackground(otherCtx, other, []tasks.TaskID{h.ID})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("foreign-session ack count=%d, want 0", count)
	}
}

// TestApplyPatch_RejectsMissingIdentity covers identity-required.
func TestApplyPatch_RejectsMissingIdentity(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	_, err := r.ApplyPatch(ctx, identity.Identity{}, "x", tasks.PatchAccept)
	if !errors.Is(err, tasks.ErrIdentityRequired) {
		t.Errorf("missing identity: err=%v, want ErrIdentityRequired", err)
	}
}

// TestAcknowledgeBackground_RejectsMissingIdentity covers
// identity-required.
func TestAcknowledgeBackground_RejectsMissingIdentity(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	_, err := r.AcknowledgeBackground(ctx, identity.Identity{}, nil)
	if !errors.Is(err, tasks.ErrIdentityRequired) {
		t.Errorf("missing identity: err=%v, want ErrIdentityRequired", err)
	}
}

// TestAddMemberToGroup_CrossSession covers the cross-session
// not-found branch.
func TestAddMemberToGroup_CrossSession(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: tripleA(), Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	other := identity.Identity{TenantID: "x", UserID: "x", SessionID: "x"}
	otherCtx, _ := identity.With(context.Background(), other)
	type seeder interface {
		AddMemberToGroup(ctx context.Context, gid tasks.TaskGroupID, tid tasks.TaskID) error
	}
	s, ok := r.(seeder)
	if !ok {
		t.Fatal("driver does not expose AddMemberToGroup")
	}
	err = s.AddMemberToGroup(otherCtx, g.ID, h.ID)
	if !errors.Is(err, tasks.ErrGroupNotFound) {
		t.Errorf("cross-session AddMember: err=%v, want ErrGroupNotFound", err)
	}
}

// TestCreatePendingPatch_RejectsCrossSession covers the
// session-visibility check.
func TestCreatePendingPatch_RejectsCrossSession(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	type seeder interface {
		CreatePendingPatch(ctx context.Context, sessionID identity.Identity, patchID string, payload []byte) (*tasks.Patch, error)
	}
	s, ok := r.(seeder)
	if !ok {
		t.Skip("driver does not expose CreatePendingPatch")
	}
	other := identity.Identity{TenantID: "x", UserID: "x", SessionID: "x"}
	_, err := s.CreatePendingPatch(ctx, other, "p-fail", []byte(`{}`))
	if !errors.Is(err, tasks.ErrIdentityRequired) {
		t.Errorf("cross-session CreatePendingPatch: err=%v, want ErrIdentityRequired", err)
	}
}

// TestRetainTurnWaiter_DeliversGroupID covers the wake-up payload
// delivery (verifying the buffered-size-1 channel + the gid send).
func TestRetainTurnWaiter_DeliversGroupID(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID:  tripleA().Identity,
		RetainTurn: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter, cancel := r.RegisterRetainTurnWaiter(tripleA().Identity)
	defer cancel()
	if err := r.CancelGroup(ctx, g.ID, "test", false); err != nil {
		t.Fatal(err)
	}
	select {
	case gid := <-waiter:
		if gid != g.ID {
			t.Errorf("waiter delivered gid=%q, want %q", gid, g.ID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("waiter did not deliver gid")
	}
}
