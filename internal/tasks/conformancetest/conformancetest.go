// Package conformancetest exposes the canonical correctness suite
// every tasks.TaskRegistry driver must pass.
//
// The suite lives in a subpackage so the production-code path
// `internal/tasks` does not import the standard library `testing`
// package (precedent: `internal/state/conformancetest` and
// `internal/artifacts/conformancetest`).
//
// Downstream drivers (post-V1 durable queue at Phase 87+) consume
// it via:
//
//	import "github.com/hurtener/Harbor/internal/tasks/conformancetest"
//
//	func TestMyDriver_Conformance(t *testing.T) {
//	    conformancetest.Run(t, func() (tasks.TaskRegistry, func()) {
//	        s := mydriver.MustNew(t)
//	        return s, func() { _ = s.Close(context.Background()) }
//	    })
//	}
//
// The factory must return a fresh, empty TaskRegistry plus a
// cleanup callback. The suite uses the factory once per top-level
// subtest; invocations are independent.
package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Factory builds a fresh TaskRegistry and returns a cleanup closure.
type Factory func() (tasks.TaskRegistry, func())

// Run executes the canonical correctness suite.
//
// Phase 20 subtests (per-task surface):
//
//   - Spawn_AssignsTaskID
//   - Spawn_Idempotent_SameKeyReturnsSameHandle
//   - Spawn_DifferentSessionsCanReuseKey
//   - Spawn_EmptyKeyDisablesIdempotency
//   - Spawn_SameKeyDivergentRequestRejected
//   - Lifecycle_HappyPath
//   - Lifecycle_PauseResume
//   - Lifecycle_InvalidTransition_RejectsLoudly
//   - Lifecycle_TerminalIsTerminal
//   - Cancel_Cascade_PropagatesToChildren
//   - Cancel_Isolate_LeavesChildrenAlone
//   - Cancel_AlreadyTerminal_NoOp
//   - Cancel_DeepGrandchildren_AllReceiveCancellation
//   - Prioritize_UpdatesValue
//   - Identity_Mandatory
//   - CrossTenant_Isolation
//   - CrossSession_Isolation
//   - List_FiltersBySession
//   - List_FiltersByStatus
//   - List_FiltersByKind
//   - List_FiltersByParent
//   - Concurrent_SpawnGetCancel_NoRace (D-025)
//   - Close_Idempotent
//   - GoroutineLeak_AfterClose
//
// Phase 21 subtests (group + retain-turn + patches + WatchGroup):
//
//   - Group_ResolveOrCreate_Idempotent
//   - Group_Seal_FreezesMembership
//   - Group_RetainTurn_BlocksUntilTerminal
//   - Group_FailFast_OnFirstFailure_CancelsRest
//   - Group_Cancel_Cascade_PropagatesToMembers
//   - Group_Cancel_NoPropagate_LeavesMembersAlone
//   - WatchGroup_Push_DeliversCompletionPayload
//   - WatchGroup_Push_OnGroupCancelled_DeliversWithReason
//   - WatchGroup_Poll_GetReturnsTerminalAfterResolve
//   - WatchGroup_Hybrid_PushAndPollCoexist
//   - WatchGroup_Unsubscribe_BeforeResolve_NoLeak
//   - WatchGroup_AlreadyResolvedGroup_ReturnsErrGroupNotFound
//   - WatchGroup_MultipleSubscribers_AllReceive
//   - WatchGroup_RefShaped_MemberResultRoundTrips
//   - Patch_Apply_HappyPath
//   - Patch_Apply_Reject_HappyPath
//   - Patch_Apply_NotFound
//   - Acknowledge_Background_EmitsPerTaskEvents
//   - Group_CrossSession_Isolation
//   - Group_Concurrent_AddRemoveSeal_NoRace (D-025)
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("Spawn_AssignsTaskID", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		h, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		if h.ID == "" {
			t.Fatal("Spawn returned empty TaskID")
		}
		if h.Reused {
			t.Errorf("Spawn returned Reused=true on first call")
		}
	})

	t.Run("Spawn_Idempotent_SameKeyReturnsSameHandle", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		req := freshSpawnReq(tripleA())
		req.IdempotencyKey = "stable-key-001"
		h1, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatalf("Spawn 1: %v", err)
		}
		h2, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatalf("Spawn 2: %v", err)
		}
		if h1.ID != h2.ID {
			t.Errorf("Spawn 2 returned different ID: %q vs %q", h1.ID, h2.ID)
		}
		if !h2.Reused {
			t.Errorf("Spawn 2 did not flag Reused=true")
		}
		if h1.Reused {
			t.Errorf("Spawn 1 incorrectly flagged Reused=true")
		}
	})

	t.Run("Spawn_DifferentSessionsCanReuseKey", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()

		// Same key, different session IDs → two distinct tasks.
		idA := identity.Identity{TenantID: "T", UserID: "U", SessionID: "session-X"}
		idB := identity.Identity{TenantID: "T", UserID: "U", SessionID: "session-Y"}
		ctxXA, _ := identity.With(context.Background(), idA)
		ctxXB, _ := identity.With(context.Background(), idB)

		reqA := freshSpawnReq(identity.Quadruple{Identity: idA})
		reqA.IdempotencyKey = "shared-key"
		reqB := freshSpawnReq(identity.Quadruple{Identity: idB})
		reqB.IdempotencyKey = "shared-key"

		hA, err := r.Spawn(ctxXA, reqA)
		if err != nil {
			t.Fatalf("Spawn A: %v", err)
		}
		hB, err := r.Spawn(ctxXB, reqB)
		if err != nil {
			t.Fatalf("Spawn B: %v", err)
		}
		if hA.ID == hB.ID {
			t.Errorf("expected distinct TaskIDs for different sessions, got %q == %q", hA.ID, hB.ID)
		}
		if hA.Reused || hB.Reused {
			t.Errorf("expected Reused=false for both spawns; A.Reused=%v B.Reused=%v", hA.Reused, hB.Reused)
		}
	})

	t.Run("Spawn_EmptyKeyDisablesIdempotency", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		req := freshSpawnReq(tripleA())
		req.IdempotencyKey = "" // explicit empty
		h1, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatalf("Spawn 1: %v", err)
		}
		h2, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatalf("Spawn 2: %v", err)
		}
		if h1.ID == h2.ID {
			t.Errorf("empty IdempotencyKey collapsed two spawns into one ID: %q", h1.ID)
		}
		if h1.Reused || h2.Reused {
			t.Errorf("empty IdempotencyKey should never flag Reused; got %v %v", h1.Reused, h2.Reused)
		}
	})

	t.Run("Spawn_SameKeyDivergentRequestRejected", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		req := freshSpawnReq(tripleA())
		req.IdempotencyKey = "divergent-key"
		req.Priority = 1
		if _, err := r.Spawn(ctx, req); err != nil {
			t.Fatalf("Spawn 1: %v", err)
		}
		req.Priority = 99 // diverge
		_, err := r.Spawn(ctx, req)
		if !errors.Is(err, tasks.ErrIdempotencyConflict) {
			t.Fatalf("Spawn 2: err=%v, want errors.Is ErrIdempotencyConflict", err)
		}
	})

	t.Run("Lifecycle_HappyPath", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		h, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatalf("MarkRunning: %v", err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
			t.Fatalf("MarkComplete: %v", err)
		}
		got, err := r.Get(ctx, h.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != tasks.StatusComplete {
			t.Errorf("final status=%q, want %q", got.Status, tasks.StatusComplete)
		}
		if got.Result == nil || string(got.Result.Value) != `"ok"` {
			t.Errorf("Result=%+v, want value=%q", got.Result, `"ok"`)
		}
	})

	t.Run("Lifecycle_PauseResume", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		h, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkPaused(ctx, h.ID); err != nil {
			t.Fatalf("MarkPaused: %v", err)
		}
		mid, err := r.Get(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if mid.Status != tasks.StatusPaused {
			t.Errorf("intermediate status=%q, want %q", mid.Status, tasks.StatusPaused)
		}
		if err := r.MarkResumed(ctx, h.ID); err != nil {
			t.Fatalf("MarkResumed: %v", err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
			t.Fatal(err)
		}
		final, err := r.Get(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != tasks.StatusComplete {
			t.Errorf("final status=%q, want %q", final.Status, tasks.StatusComplete)
		}
	})

	t.Run("Lifecycle_InvalidTransition_RejectsLoudly", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		h, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		// Pending → Complete (skipping Running) is invalid.
		err = r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"x"`)})
		if !errors.Is(err, tasks.ErrInvalidTransition) {
			t.Fatalf("err=%v, want errors.Is ErrInvalidTransition", err)
		}
		// Likewise: Resume from Pending is invalid.
		err = r.MarkResumed(ctx, h.ID)
		if !errors.Is(err, tasks.ErrInvalidTransition) {
			t.Fatalf("MarkResumed from pending: err=%v, want ErrInvalidTransition", err)
		}
	})

	t.Run("Lifecycle_TerminalIsTerminal", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		h, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"x"`)}); err != nil {
			t.Fatal(err)
		}
		// Any Mark* on a terminal state must reject.
		if err := r.MarkRunning(ctx, h.ID); !errors.Is(err, tasks.ErrInvalidTransition) {
			t.Errorf("MarkRunning on Complete: err=%v, want ErrInvalidTransition", err)
		}
		if err := r.MarkPaused(ctx, h.ID); !errors.Is(err, tasks.ErrInvalidTransition) {
			t.Errorf("MarkPaused on Complete: err=%v, want ErrInvalidTransition", err)
		}
		if err := r.MarkFailed(ctx, h.ID, tasks.TaskError{Code: "x"}); !errors.Is(err, tasks.ErrInvalidTransition) {
			t.Errorf("MarkFailed on Complete: err=%v, want ErrInvalidTransition", err)
		}
	})

	t.Run("Cancel_Cascade_PropagatesToChildren", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		parent, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		// Mark parent running so the cancel transition is the cascade
		// trigger.
		if err := r.MarkRunning(ctx, parent.ID); err != nil {
			t.Fatal(err)
		}
		// Spawn 3 children, each cascade.
		var childIDs []tasks.TaskID
		for i := 0; i < 3; i++ {
			req := freshSpawnReq(tripleA())
			parentID := parent.ID
			req.ParentTaskID = &parentID
			req.PropagateOnCancel = tasks.PropagateCascade
			h, err := r.Spawn(ctx, req)
			if err != nil {
				t.Fatalf("Spawn child %d: %v", i, err)
			}
			if err := r.MarkRunning(ctx, h.ID); err != nil {
				t.Fatalf("MarkRunning child %d: %v", i, err)
			}
			childIDs = append(childIDs, h.ID)
		}
		// Cancel parent.
		ok, err := r.Cancel(ctx, parent.ID, "operator-cancel")
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if !ok {
			t.Fatal("Cancel returned false on first call to a running task")
		}
		// Parent + all 3 children must be StatusCancelled.
		got, err := r.Get(ctx, parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tasks.StatusCancelled {
			t.Errorf("parent status=%q, want %q", got.Status, tasks.StatusCancelled)
		}
		for _, cID := range childIDs {
			cgot, err := r.Get(ctx, cID)
			if err != nil {
				t.Fatal(err)
			}
			if cgot.Status != tasks.StatusCancelled {
				t.Errorf("child %q status=%q, want %q", cID, cgot.Status, tasks.StatusCancelled)
			}
		}
	})

	t.Run("Cancel_Isolate_LeavesChildrenAlone", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		// Parent spawned with isolate.
		parentReq := freshSpawnReq(tripleA())
		parentReq.PropagateOnCancel = tasks.PropagateIsolate
		parent, err := r.Spawn(ctx, parentReq)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, parent.ID); err != nil {
			t.Fatal(err)
		}
		// 3 children, each cascade. Isolate is on the parent's policy.
		var childIDs []tasks.TaskID
		for i := 0; i < 3; i++ {
			cReq := freshSpawnReq(tripleA())
			pid := parent.ID
			cReq.ParentTaskID = &pid
			cReq.PropagateOnCancel = tasks.PropagateCascade
			h, err := r.Spawn(ctx, cReq)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.MarkRunning(ctx, h.ID); err != nil {
				t.Fatal(err)
			}
			childIDs = append(childIDs, h.ID)
		}
		// Cancel parent → only parent transitions; children stay running.
		if _, err := r.Cancel(ctx, parent.ID, "isolate-test"); err != nil {
			t.Fatal(err)
		}
		got, err := r.Get(ctx, parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tasks.StatusCancelled {
			t.Errorf("parent status=%q, want %q", got.Status, tasks.StatusCancelled)
		}
		for _, cID := range childIDs {
			cgot, err := r.Get(ctx, cID)
			if err != nil {
				t.Fatal(err)
			}
			if cgot.Status != tasks.StatusRunning {
				t.Errorf("child %q status=%q, want %q (isolate must not cascade)",
					cID, cgot.Status, tasks.StatusRunning)
			}
		}
	})

	t.Run("Cancel_AlreadyTerminal_NoOp", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		h, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"x"`)}); err != nil {
			t.Fatal(err)
		}
		// Cancel after Complete: idempotent (false, nil).
		ok, err := r.Cancel(ctx, h.ID, "late-cancel")
		if err != nil {
			t.Fatalf("Cancel after terminal: %v", err)
		}
		if ok {
			t.Errorf("Cancel after terminal returned true; want false")
		}
		got, err := r.Get(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tasks.StatusComplete {
			t.Errorf("status changed after late Cancel: %q (expected Complete to be preserved)", got.Status)
		}
	})

	t.Run("Cancel_DeepGrandchildren_AllReceiveCancellation", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		// Build a 3-level tree: parent → child → grandchild.
		parent, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, parent.ID); err != nil {
			t.Fatal(err)
		}
		cReq := freshSpawnReq(tripleA())
		pid := parent.ID
		cReq.ParentTaskID = &pid
		child, err := r.Spawn(ctx, cReq)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, child.ID); err != nil {
			t.Fatal(err)
		}
		gReq := freshSpawnReq(tripleA())
		cid := child.ID
		gReq.ParentTaskID = &cid
		grand, err := r.Spawn(ctx, gReq)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, grand.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Cancel(ctx, parent.ID, "cascade-deep"); err != nil {
			t.Fatal(err)
		}
		for _, id := range []tasks.TaskID{parent.ID, child.ID, grand.ID} {
			got, err := r.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tasks.StatusCancelled {
				t.Errorf("task %q status=%q, want %q", id, got.Status, tasks.StatusCancelled)
			}
		}
	})

	t.Run("Prioritize_UpdatesValue", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		h, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		ok, err := r.Prioritize(ctx, h.ID, 7)
		if err != nil {
			t.Fatalf("Prioritize: %v", err)
		}
		if !ok {
			t.Errorf("Prioritize returned false on a real change")
		}
		got, err := r.Get(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Priority != 7 {
			t.Errorf("priority=%d, want 7", got.Priority)
		}
		// Same value → (false, nil).
		ok, err = r.Prioritize(ctx, h.ID, 7)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Errorf("Prioritize returned true on a no-op")
		}
	})

	t.Run("Identity_Mandatory", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()

		// Spawn with each identity component empty.
		incomplete := []identity.Quadruple{
			{Identity: identity.Identity{UserID: "U", SessionID: "S"}},
			{Identity: identity.Identity{TenantID: "T", SessionID: "S"}},
			{Identity: identity.Identity{TenantID: "T", UserID: "U"}},
			{}, // all empty
		}
		ctx := context.Background()
		for i, q := range incomplete {
			req := freshSpawnReq(q)
			_, err := r.Spawn(ctx, req)
			if !errors.Is(err, tasks.ErrIdentityRequired) {
				t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, q, err)
			}
		}
		// Get / Cancel without ctx identity → ErrIdentityRequired.
		// First, spawn a real task so the (id, registry) lookup
		// would otherwise succeed.
		h, err := r.Spawn(ctxA(), freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Get(context.Background(), h.ID); !errors.Is(err, tasks.ErrIdentityRequired) {
			t.Errorf("Get without ctx identity: err=%v, want ErrIdentityRequired", err)
		}
		if _, err := r.Cancel(context.Background(), h.ID, "no-id"); !errors.Is(err, tasks.ErrIdentityRequired) {
			t.Errorf("Cancel without ctx identity: err=%v, want ErrIdentityRequired", err)
		}
	})

	t.Run("CrossTenant_Isolation", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		// Spawn under tenant A.
		idA := identity.Identity{TenantID: "tenant-A", UserID: "U", SessionID: "S"}
		idB := identity.Identity{TenantID: "tenant-B", UserID: "U", SessionID: "S"}
		ctxA, _ := identity.With(context.Background(), idA)
		ctxB, _ := identity.With(context.Background(), idB)
		h, err := r.Spawn(ctxA, freshSpawnReq(identity.Quadruple{Identity: idA}))
		if err != nil {
			t.Fatal(err)
		}
		// Tenant B reading A's task → ErrNotFound.
		if _, err := r.Get(ctxB, h.ID); !errors.Is(err, tasks.ErrNotFound) {
			t.Errorf("cross-tenant Get: err=%v, want ErrNotFound", err)
		}
		// Tenant B cancelling A's task → ErrNotFound.
		if _, err := r.Cancel(ctxB, h.ID, "cross"); !errors.Is(err, tasks.ErrNotFound) {
			t.Errorf("cross-tenant Cancel: err=%v, want ErrNotFound", err)
		}
		// Tenant A's view is intact.
		gotA, err := r.Get(ctxA, h.ID)
		if err != nil {
			t.Fatalf("tenant-A Get: %v", err)
		}
		if gotA.Identity.TenantID != "tenant-A" {
			t.Errorf("tenant-A leaked tenant-B's identity: %+v", gotA.Identity)
		}
	})

	t.Run("CrossSession_Isolation", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		idA := identity.Identity{TenantID: "T", UserID: "U", SessionID: "session-1"}
		idB := identity.Identity{TenantID: "T", UserID: "U", SessionID: "session-2"}
		ctxAA, _ := identity.With(context.Background(), idA)
		ctxBB, _ := identity.With(context.Background(), idB)
		hA, err := r.Spawn(ctxAA, freshSpawnReq(identity.Quadruple{Identity: idA}))
		if err != nil {
			t.Fatal(err)
		}
		hB, err := r.Spawn(ctxBB, freshSpawnReq(identity.Quadruple{Identity: idB}))
		if err != nil {
			t.Fatal(err)
		}
		// Session A cannot read session B's task.
		if _, err := r.Get(ctxAA, hB.ID); !errors.Is(err, tasks.ErrNotFound) {
			t.Errorf("cross-session Get: err=%v, want ErrNotFound", err)
		}
		// Each session sees only its own task in List.
		listA, err := r.List(ctxAA, idA, tasks.TaskFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(listA) != 1 || listA[0].ID != hA.ID {
			t.Errorf("session A list=%+v, want exactly hA=%q", listA, hA.ID)
		}
		listB, err := r.List(ctxBB, idB, tasks.TaskFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(listB) != 1 || listB[0].ID != hB.ID {
			t.Errorf("session B list=%+v, want exactly hB=%q", listB, hB.ID)
		}
	})

	t.Run("List_FiltersBySession", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		// Multiple tasks across two sessions; List filters by session.
		idA := identity.Identity{TenantID: "T", UserID: "U", SessionID: "sess-A"}
		idB := identity.Identity{TenantID: "T", UserID: "U", SessionID: "sess-B"}
		ctxAA, _ := identity.With(context.Background(), idA)
		ctxBB, _ := identity.With(context.Background(), idB)
		for i := 0; i < 3; i++ {
			if _, err := r.Spawn(ctxAA, freshSpawnReq(identity.Quadruple{Identity: idA})); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 2; i++ {
			if _, err := r.Spawn(ctxBB, freshSpawnReq(identity.Quadruple{Identity: idB})); err != nil {
				t.Fatal(err)
			}
		}
		listA, err := r.List(ctxAA, idA, tasks.TaskFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(listA) != 3 {
			t.Errorf("session A list count=%d, want 3", len(listA))
		}
		listB, err := r.List(ctxBB, idB, tasks.TaskFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(listB) != 2 {
			t.Errorf("session B list count=%d, want 2", len(listB))
		}
	})

	t.Run("List_FiltersByStatus", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		h1, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		h2, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		_ = h2
		// Mark h1 running, leave h2 pending.
		if err := r.MarkRunning(ctx, h1.ID); err != nil {
			t.Fatal(err)
		}
		want := tasks.StatusRunning
		filter := tasks.TaskFilter{Status: &want}
		got, err := r.List(ctx, tripleA().Identity, filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != h1.ID {
			t.Errorf("list-by-status=%+v, want exactly h1=%q", got, h1.ID)
		}
	})

	t.Run("List_FiltersByKind", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		fg := freshSpawnReq(tripleA())
		fg.Kind = tasks.KindForeground
		bg := freshSpawnReq(tripleA())
		bg.Kind = tasks.KindBackground
		hF, err := r.Spawn(ctx, fg)
		if err != nil {
			t.Fatal(err)
		}
		hB, err := r.Spawn(ctx, bg)
		if err != nil {
			t.Fatal(err)
		}
		want := tasks.KindBackground
		got, err := r.List(ctx, tripleA().Identity, tasks.TaskFilter{Kind: &want})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != hB.ID {
			t.Errorf("list-by-kind=%+v, want exactly hB=%q (hF=%q)", got, hB.ID, hF.ID)
		}
	})

	t.Run("List_FiltersByParent", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		parent, err := r.Spawn(ctx, freshSpawnReq(tripleA()))
		if err != nil {
			t.Fatal(err)
		}
		var childIDs []tasks.TaskID
		for i := 0; i < 2; i++ {
			req := freshSpawnReq(tripleA())
			pid := parent.ID
			req.ParentTaskID = &pid
			h, err := r.Spawn(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			childIDs = append(childIDs, h.ID)
		}
		_, err = r.Spawn(ctx, freshSpawnReq(tripleA())) // unrelated
		if err != nil {
			t.Fatal(err)
		}
		pid := parent.ID
		got, err := r.List(ctx, tripleA().Identity, tasks.TaskFilter{ParentID: &pid})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("list-by-parent count=%d, want 2 (got %+v)", len(got), got)
		}
		seen := map[tasks.TaskID]bool{}
		for _, s := range got {
			seen[s.ID] = true
		}
		for _, id := range childIDs {
			if !seen[id] {
				t.Errorf("expected child %q in list, missing", id)
			}
		}
	})

	t.Run("Concurrent_SpawnGetCancel_NoRace", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		baseline := runtime.NumGoroutine()
		const goroutines = 128
		const opsPerGo = 8

		var wg sync.WaitGroup
		var errs atomic.Int64
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			i := i
			go func() {
				defer wg.Done()
				ident := identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", i%17),
					UserID:    fmt.Sprintf("u-%d", i%41),
					SessionID: fmt.Sprintf("s-%d", i),
				}
				ctx, ierr := identity.With(context.Background(), ident)
				if ierr != nil {
					errs.Add(1)
					return
				}
				for j := 0; j < opsPerGo; j++ {
					req := freshSpawnReq(identity.Quadruple{Identity: ident})
					h, err := r.Spawn(ctx, req)
					if err != nil {
						errs.Add(1)
						return
					}
					// Identity check: the returned task carries this
					// goroutine's identity, never another's.
					got, err := r.Get(ctx, h.ID)
					if err != nil {
						errs.Add(1)
						return
					}
					if got.Identity.SessionID != ident.SessionID {
						errs.Add(1)
						return
					}
					// Mix of Mark*/Cancel to exercise the FSM under
					// load. Some iterations cancel from pending; some
					// move through running before completing.
					if j%2 == 0 {
						if _, err := r.Cancel(ctx, h.ID, "concurrent"); err != nil {
							errs.Add(1)
							return
						}
					} else {
						if err := r.MarkRunning(ctx, h.ID); err != nil {
							errs.Add(1)
							return
						}
						if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
							errs.Add(1)
							return
						}
					}
				}
			}()
		}
		wg.Wait()
		if n := errs.Load(); n != 0 {
			t.Fatalf("%d concurrent operations errored", n)
		}
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 0 {
			t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
		}
	})

	t.Run("Close_Idempotent", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close 1: %v", err)
		}
		if err := r.Close(ctx); err != nil {
			t.Fatalf("Close 2 (idempotent): %v", err)
		}
		// Subsequent ops must return ErrRegistryClosed.
		_, err := r.Spawn(ctxA(), freshSpawnReq(tripleA()))
		if !errors.Is(err, tasks.ErrRegistryClosed) {
			t.Errorf("Spawn after Close: err=%v, want ErrRegistryClosed", err)
		}
	})

	t.Run("GoroutineLeak_AfterClose", func(t *testing.T) {
		r, cleanup := factory()
		baseline := runtime.NumGoroutine()
		ctx := ctxA()
		// Spawn a few tasks so any internal goroutines have kicked in
		// (there are none for inprocess; future drivers may spin pumps).
		for i := 0; i < 8; i++ {
			if _, err := r.Spawn(ctx, freshSpawnReq(tripleA())); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		cleanup()
		// Bounded wait for goroutines to settle. Gosched-only —
		// time.Sleep is forbidden as a synchronisation primitive per
		// AGENTS.md §11.
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 0 {
			t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
		}
	})

	// --- Phase 21 subtests --------------------------------------------------

	runGroupSubtests(t, factory)
}

// --- Test helpers ----------------------------------------------------------

// tripleA is the canonical identity used by single-session subtests.
func tripleA() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1"},
	}
}

// ctxA returns a context with tripleA's identity attached.
func ctxA() context.Context {
	ctx, err := identity.With(context.Background(), tripleA().Identity)
	if err != nil {
		panic(fmt.Sprintf("ctxA: %v", err))
	}
	return ctx
}

// freshSpawnReq builds a SpawnRequest with the given identity and
// no idempotency key (so every call yields a fresh task).
func freshSpawnReq(q identity.Quadruple) tasks.SpawnRequest {
	return tasks.SpawnRequest{
		Identity:    q,
		Kind:        tasks.KindForeground,
		Description: "test task",
		Query:       "test query",
		Priority:    0,
	}
}
