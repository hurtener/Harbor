package inprocess_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestApplyGroup_SealAction covers the ActionSeal dispatch path.
func TestApplyGroup_SealAction(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyGroup(ctx, g.ID, tasks.ActionSeal); err != nil {
		t.Fatalf("ApplyGroup(seal): %v", err)
	}
	groups, err := r.ListGroups(ctx, tripleA().Identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Status != tasks.GroupSealed {
		t.Errorf("after ApplyGroup(seal): %+v", groups)
	}
}

// TestApplyGroup_CancelAction covers the ActionCancel dispatch.
func TestApplyGroup_CancelAction(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyGroup(ctx, g.ID, tasks.ActionCancel); err != nil {
		t.Fatalf("ApplyGroup(cancel): %v", err)
	}
	groups, err := r.ListGroups(ctx, tripleA().Identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Status != tasks.GroupCancelled {
		t.Errorf("after ApplyGroup(cancel): %+v", groups)
	}
}

// TestApplyGroup_ResolveAction covers ActionResolve on a sealed group.
func TestApplyGroup_ResolveAction(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Resolve on Open → ErrGroupNotSealed.
	err = r.ApplyGroup(ctx, g.ID, tasks.ActionResolve)
	if !errors.Is(err, tasks.ErrGroupNotSealed) {
		t.Errorf("Resolve on Open: err=%v, want ErrGroupNotSealed", err)
	}
	// Seal then resolve → Completed.
	if err := r.SealGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyGroup(ctx, g.ID, tasks.ActionResolve); err != nil {
		t.Fatalf("ApplyGroup(resolve) on sealed: %v", err)
	}
	groups, err := r.ListGroups(ctx, tripleA().Identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Status != tasks.GroupCompleted {
		t.Errorf("after resolve: %+v", groups)
	}
	// Resolve on terminal → ErrGroupInvalidTransition.
	err = r.ApplyGroup(ctx, g.ID, tasks.ActionResolve)
	if !errors.Is(err, tasks.ErrGroupInvalidTransition) {
		t.Errorf("Resolve on terminal: err=%v, want ErrGroupInvalidTransition", err)
	}
}

// TestApplyGroup_UnknownAction rejects bogus enum values.
func TestApplyGroup_UnknownAction(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = r.ApplyGroup(ctx, g.ID, tasks.GroupAction("bogus"))
	if !errors.Is(err, tasks.ErrGroupInvalidTransition) {
		t.Errorf("unknown action: err=%v, want ErrGroupInvalidTransition", err)
	}
}

// TestResolveOrCreateGroup_IdentityValidation covers the missing-
// identity branch.
func TestResolveOrCreateGroup_IdentityValidation(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	// Empty identity in request → ErrIdentityRequired.
	_, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{})
	if !errors.Is(err, tasks.ErrIdentityRequired) {
		t.Errorf("empty identity: err=%v, want ErrIdentityRequired", err)
	}
	// Request session != ctx session → ErrIdentityRequired.
	other := identity.Identity{TenantID: "other-T", UserID: "other-U", SessionID: "other-S"}
	_, err = r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: other,
	})
	if !errors.Is(err, tasks.ErrIdentityRequired) {
		t.Errorf("ctx-session mismatch: err=%v, want ErrIdentityRequired", err)
	}
}

// TestSealGroup_InvalidTransition covers the FSM rejection path.
func TestSealGroup_InvalidTransition(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SealGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	// Re-seal → invalid.
	err = r.SealGroup(ctx, g.ID)
	if !errors.Is(err, tasks.ErrGroupInvalidTransition) {
		t.Errorf("re-seal: err=%v, want ErrGroupInvalidTransition", err)
	}
}

// TestSealGroup_NotFound covers the missing-group path.
func TestSealGroup_NotFound(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	err := r.SealGroup(ctx, tasks.TaskGroupID("no-such-group"))
	if !errors.Is(err, tasks.ErrGroupNotFound) {
		t.Errorf("missing group: err=%v, want ErrGroupNotFound", err)
	}
}

// TestCancelGroup_IdempotentOnTerminal covers the already-terminal
// no-op path.
func TestCancelGroup_IdempotentOnTerminal(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CancelGroup(ctx, g.ID, "first", true); err != nil {
		t.Fatal(err)
	}
	// Second cancel → idempotent (no error).
	if err := r.CancelGroup(ctx, g.ID, "second", true); err != nil {
		t.Errorf("second cancel on terminal: err=%v, want nil (idempotent)", err)
	}
}

// TestSpawn_GroupNotFound covers the SpawnRequest.GroupID rollback
// path when the group doesn't exist.
func TestSpawn_GroupNotFound(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	req := tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
		GroupID:  tasks.TaskGroupID("no-such-group"),
	}
	_, err := r.Spawn(ctx, req)
	if !errors.Is(err, tasks.ErrGroupNotFound) {
		t.Errorf("Spawn into missing group: err=%v, want ErrGroupNotFound", err)
	}
}

// TestSpawn_GroupSealedRollback covers the rollback path when the
// group is sealed.
func TestSpawn_GroupSealedRollback(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SealGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	req := tasks.SpawnRequest{
		Identity:       tripleA(),
		Kind:           tasks.KindForeground,
		GroupID:        g.ID,
		IdempotencyKey: "rollback-key",
	}
	_, err = r.Spawn(ctx, req)
	if !errors.Is(err, tasks.ErrGroupSealed) {
		t.Fatalf("Spawn into sealed: err=%v, want ErrGroupSealed", err)
	}
	// The rollback must have unwound the idempotency index — a fresh
	// spawn with the same key on an Open group should succeed.
	g2, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	req2 := req
	req2.GroupID = g2.ID
	if _, err := r.Spawn(ctx, req2); err != nil {
		t.Errorf("post-rollback Spawn: err=%v (idempotency index leaked from rollback)", err)
	}
}

// TestApplyPatch_AlreadyTerminal covers the idempotent re-apply
// and the invalid-transition path.
func TestApplyPatch_AlreadyTerminal(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	// Cast to access the driver-internal CreatePendingPatch helper.
	seeder, ok := r.(interface {
		CreatePendingPatch(ctx context.Context, sessionID identity.Identity, patchID string, payload []byte) (*tasks.Patch, error)
	})
	if !ok {
		t.Fatal("inprocess driver does not expose CreatePendingPatch")
	}
	if _, err := seeder.CreatePendingPatch(ctx, tripleA().Identity, "p-mixed", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	// Reject first.
	if _, err := r.ApplyPatch(ctx, tripleA().Identity, "p-mixed", tasks.PatchReject); err != nil {
		t.Fatal(err)
	}
	// Then accept → ErrGroupInvalidTransition (rejected → applied not valid).
	_, err := r.ApplyPatch(ctx, tripleA().Identity, "p-mixed", tasks.PatchAccept)
	if !errors.Is(err, tasks.ErrGroupInvalidTransition) {
		t.Errorf("rejected→accept: err=%v, want ErrGroupInvalidTransition", err)
	}
}

// TestAcknowledgeBackground_SkipsNonBackground exercises the
// non-background skip branch.
func TestAcknowledgeBackground_SkipsNonBackground(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	// Foreground task — should be skipped by Acknowledge.
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
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
	count, err := r.AcknowledgeBackground(ctx, tripleA().Identity, []tasks.TaskID{h.ID})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("foreground task ack count=%d, want 0 (must be skipped)", count)
	}
}

// TestAcknowledgeBackground_SkipsNonTerminal exercises the
// non-terminal skip branch.
func TestAcknowledgeBackground_SkipsNonTerminal(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindBackground,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Still Pending — ack should skip.
	count, err := r.AcknowledgeBackground(ctx, tripleA().Identity, []tasks.TaskID{h.ID})
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("pending bg task ack count=%d, want 0", count)
	}
}

// TestRetainTurnWaiter_CancelBeforeResolve covers the cancel-before-
// resolve path.
func TestRetainTurnWaiter_CancelBeforeResolve(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	waiter, cancel := r.RegisterRetainTurnWaiter(tripleA().Identity)
	cancel()
	// Channel must be closed by the cancel func.
	select {
	case _, ok := <-waiter:
		if ok {
			t.Error("waiter delivered on cancel path; expected close-only")
		}
	default:
		t.Error("waiter channel was not closed by cancel")
	}
	// Second cancel is a no-op.
	cancel()
}

// TestAddMemberToGroup_Errors covers the driver-internal helper's
// error branches.
func TestAddMemberToGroup_Errors(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	type seeder interface {
		AddMemberToGroup(ctx context.Context, gid tasks.TaskGroupID, tid tasks.TaskID) error
	}
	s, ok := r.(seeder)
	if !ok {
		t.Fatal("driver does not expose AddMemberToGroup")
	}
	// Unknown group.
	err := s.AddMemberToGroup(ctx, tasks.TaskGroupID("no-group"), tasks.TaskID("no-task"))
	if !errors.Is(err, tasks.ErrGroupNotFound) {
		t.Errorf("unknown group: err=%v, want ErrGroupNotFound", err)
	}
	// Group exists but task doesn't.
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	err = s.AddMemberToGroup(ctx, g.ID, tasks.TaskID("no-task"))
	if !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("unknown task: err=%v, want ErrNotFound", err)
	}
	// Both exist, but the group has been sealed → ErrGroupSealed.
	h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: tripleA(), Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SealGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	err = s.AddMemberToGroup(ctx, g.ID, h.ID)
	if !errors.Is(err, tasks.ErrGroupSealed) {
		t.Errorf("sealed group: err=%v, want ErrGroupSealed", err)
	}
}

// TestListGroups_FiltersByStatus covers the status-filter branch.
func TestListGroups_FiltersByStatus(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g1, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	g2, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	// g1 sealed; g2 stays open.
	if err := r.SealGroup(ctx, g1.ID); err != nil {
		t.Fatal(err)
	}
	want := tasks.GroupSealed
	got, err := r.ListGroups(ctx, tripleA().Identity, &want)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != g1.ID {
		t.Errorf("filter=Sealed: %+v (want exactly g1=%q, g2=%q open)", got, g1.ID, g2.ID)
	}
}

// TestListGroups_RejectsMissingIdentity covers the identity-validation
// branch.
func TestListGroups_RejectsMissingIdentity(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	_, err := r.ListGroups(ctx, identity.Identity{}, nil)
	if !errors.Is(err, tasks.ErrIdentityRequired) {
		t.Errorf("ListGroups missing identity: err=%v, want ErrIdentityRequired", err)
	}
}

// TestOperationsAfterClose_GroupSurface covers each Phase 21 method's
// closed-check.
func TestOperationsAfterClose_GroupSurface(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(name string, err error) {
		if !errors.Is(err, tasks.ErrRegistryClosed) {
			t.Errorf("%s after Close: err=%v, want ErrRegistryClosed", name, err)
		}
	}
	_, err = r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	check("ResolveOrCreateGroup", err)
	check("SealGroup", r.SealGroup(ctx, g.ID))
	check("CancelGroup", r.CancelGroup(ctx, g.ID, "x", true))
	check("ApplyGroup", r.ApplyGroup(ctx, g.ID, tasks.ActionSeal))
	_, err = r.ListGroups(ctx, tripleA().Identity, nil)
	check("ListGroups", err)
	_, err = r.ApplyPatch(ctx, tripleA().Identity, "x", tasks.PatchAccept)
	check("ApplyPatch", err)
	_, err = r.AcknowledgeBackground(ctx, tripleA().Identity, nil)
	check("AcknowledgeBackground", err)
}

// TestApplyPatch_Cross_Session covers ApplyPatch's identity gate.
func TestApplyPatch_CrossSession(t *testing.T) {
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
	if _, err := s.CreatePendingPatch(ctx, tripleA().Identity, "p-cross", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	other := identity.Identity{TenantID: "other-T", UserID: "other-U", SessionID: "other-S"}
	otherCtx, _ := identity.With(context.Background(), other)
	// Cross-session apply: ctx identity != session identity argument
	// → ErrIdentityRequired (we don't leak existence to other
	// sessions, but the visibility check fires first).
	_, err := r.ApplyPatch(otherCtx, tripleA().Identity, "p-cross", tasks.PatchAccept)
	if !errors.Is(err, tasks.ErrIdentityRequired) {
		t.Errorf("cross-session ApplyPatch: err=%v, want ErrIdentityRequired", err)
	}
}

// TestSpawnTool_WithGroupID covers SpawnTool's group wiring.
func TestSpawnTool_WithGroupID(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.SpawnTool(ctx, tasks.SpawnToolRequest{
		Identity: tripleA(),
		ToolName: "x.y",
		GroupID:  g.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Verify the task landed in the group's Members list.
	groups, err := r.ListGroups(ctx, tripleA().Identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("ListGroups returned %d, want 1", len(groups))
	}
	found := false
	for _, m := range groups[0].Members {
		if m == h.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SpawnTool with GroupID did not add task to group; members=%+v", groups[0].Members)
	}
}

// TestCancelGroup_WithPropagate_CascadesToTaskChildren covers
// cancelTaskLocked's children-cascade walk.
func TestCancelGroup_WithPropagate_CascadesToTaskChildren(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	// Member task with a child of its own.
	memReq := tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
		GroupID:  g.ID,
	}
	mem, err := r.Spawn(ctx, memReq)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, mem.ID); err != nil {
		t.Fatal(err)
	}
	pid := mem.ID
	child, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity:     tripleA(),
		Kind:         tasks.KindForeground,
		ParentTaskID: &pid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	// CancelGroup with propagate cancels member; member's cascade
	// reaches child.
	if err := r.CancelGroup(ctx, g.ID, "cascade-test", true); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.StatusCancelled {
		t.Errorf("child status=%q, want Cancelled (parent-member cascade should reach child)", got.Status)
	}
}

// TestRetainTurnWaiter_DoubleWakeIsNoOp covers the close-once
// invariant under back-to-back retain-turn groups.
func TestRetainTurnWaiter_DoubleWakeIsNoOp(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	g1, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID:  tripleA().Identity,
		RetainTurn: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	g2, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID:  tripleA().Identity,
		RetainTurn: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter, cancel := r.RegisterRetainTurnWaiter(tripleA().Identity)
	defer cancel()

	// Cancel g1 — the waiter receives the resolved gid and closes.
	if err := r.CancelGroup(ctx, g1.ID, "first", true); err != nil {
		t.Fatal(err)
	}
	select {
	case gid := <-waiter:
		_ = gid // either zero (closed) or g1.ID (delivered)
	default:
		// resolve happened; the channel should be readable
		// (delivered + closed).
		t.Error("waiter has no value after first resolve")
	}
	// Cancelling g2 must NOT panic on the (already-closed) waiter
	// channel. The driver's close-once guard prevents re-send.
	if err := r.CancelGroup(ctx, g2.ID, "second", true); err != nil {
		t.Fatal(err)
	}
}

// TestCreatePendingPatch_Idempotent covers the same-id re-create
// branch.
func TestCreatePendingPatch_Idempotent(t *testing.T) {
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
	p1, err := s.CreatePendingPatch(ctx, tripleA().Identity, "p-idem", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.CreatePendingPatch(ctx, tripleA().Identity, "p-idem", []byte(`{"different":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if p1.ID != p2.ID {
		t.Errorf("idempotent create returned different IDs: %q vs %q", p1.ID, p2.ID)
	}
	if string(p1.Bytes) != string(p2.Bytes) {
		// Idempotent return MUST yield the original bytes, not the
		// later call's payload. This is the contract — same as
		// SpawnRequest's idempotency.
		if !strings.Contains(string(p2.Bytes), "{}") {
			t.Errorf("second call returned mutated bytes: %s", p2.Bytes)
		}
	}
}
