package conformancetest

// Phase 21 conformance subtests for the group + retain-turn + patch
// + WatchGroup surface. Subtests live in this file for navigability
// — the Phase 20 per-task suite stays in `conformancetest.go`.
//
// `runGroupSubtests` is invoked by the main `Run` function so
// downstream drivers (post-V1 durable queue at Phase 87+) inherit
// both halves of the suite verbatim.
//
// Two driver-internal helpers (`AddMemberToGroup`,
// `CreatePendingPatch`) are required to seed test fixtures without
// reaching for production wiring that doesn't exist yet (the
// `SpawnRequest.GroupID` seam IS used; for tests that need to drive
// a patch through `ApplyPatch`, drivers must expose
// `CreatePendingPatch`). The suite probes via the
// `groupSeeder` / `patchSeeder` interfaces so a downstream driver
// can satisfy them without committing to the in-process driver's
// concrete shape.

import (
	"context"
	"encoding/json"
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

// groupSeeder is implemented by drivers that expose the test-only
// "wire a freshly-spawned task into a group" helper. The in-process
// driver implements this; downstream drivers that follow the same
// pattern can inherit the test suite by satisfying this interface
// on their driver type.
//
// The seam exists because Phase 21 doesn't wire `SpawnTool`'s tool
// dispatch yet — the conformance suite seeds members directly to
// exercise the resolve gate.
type groupSeeder interface {
	AddMemberToGroup(ctx context.Context, gid tasks.TaskGroupID, tid tasks.TaskID) error
}

// patchSeeder is implemented by drivers that expose the test-only
// "create a pending patch" helper. Phase 21 ships the transition
// surface (ApplyPatch); the seeder exists so the conformance suite
// can drive transitions without coupling to a planner concrete.
type patchSeeder interface {
	CreatePendingPatch(ctx context.Context, sessionID identity.Identity, patchID string, payload []byte) (*tasks.Patch, error)
}

func runGroupSubtests(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("Group_ResolveOrCreate_Idempotent", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		req := tasks.GroupRequest{
			ID:          tasks.TaskGroupID("g-idem-1"),
			SessionID:   tripleA().Identity,
			Description: "idempotent group",
		}
		g1, err := r.ResolveOrCreateGroup(ctx, req)
		if err != nil {
			t.Fatalf("ResolveOrCreate 1: %v", err)
		}
		g2, err := r.ResolveOrCreateGroup(ctx, req)
		if err != nil {
			t.Fatalf("ResolveOrCreate 2: %v", err)
		}
		if g1.ID != g2.ID {
			t.Errorf("ResolveOrCreate returned different IDs: %q vs %q", g1.ID, g2.ID)
		}
		if g1.Status != tasks.GroupOpen || g2.Status != tasks.GroupOpen {
			t.Errorf("expected GroupOpen on both returns; got %q / %q", g1.Status, g2.Status)
		}
	})

	t.Run("Group_Seal_FreezesMembership", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Spawn one member while Open.
		req := freshSpawnReq(tripleA())
		req.GroupID = g.ID
		if _, err := r.Spawn(ctx, req); err != nil {
			t.Fatalf("Spawn member (open): %v", err)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatalf("SealGroup: %v", err)
		}
		// Spawn after seal → ErrGroupSealed.
		req2 := freshSpawnReq(tripleA())
		req2.GroupID = g.ID
		_, err = r.Spawn(ctx, req2)
		if !errors.Is(err, tasks.ErrGroupSealed) {
			t.Errorf("Spawn into sealed group: err=%v, want ErrGroupSealed", err)
		}
	})

	t.Run("Group_RetainTurn_BlocksUntilTerminal", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID:  tripleA().Identity,
			RetainTurn: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		waiter, cancelWaiter := r.RegisterRetainTurnWaiter(tripleA().Identity)
		defer cancelWaiter()

		// Spawn 3 members; mark each running; mark each complete.
		var members []tasks.TaskID
		for i := 0; i < 3; i++ {
			req := freshSpawnReq(tripleA())
			req.GroupID = g.ID
			h, err := r.Spawn(ctx, req)
			if err != nil {
				t.Fatalf("Spawn child %d: %v", i, err)
			}
			members = append(members, h.ID)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatalf("SealGroup: %v", err)
		}
		// Mark first 2 running + complete; waiter MUST NOT have fired.
		for i := 0; i < 2; i++ {
			if err := r.MarkRunning(ctx, members[i]); err != nil {
				t.Fatal(err)
			}
			if err := r.MarkComplete(ctx, members[i], tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
				t.Fatal(err)
			}
			select {
			case _, ok := <-waiter:
				if ok {
					t.Errorf("waiter received value after member %d completed; expected to remain open until all members terminal", i)
				} else {
					t.Errorf("waiter closed prematurely after member %d completed", i)
				}
			default:
			}
		}
		// Mark third running + complete → waiter MUST fire.
		if err := r.MarkRunning(ctx, members[2]); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, members[2], tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
			t.Fatal(err)
		}
		select {
		case gid, ok := <-waiter:
			if ok && gid != g.ID {
				t.Errorf("waiter received gid=%q, want %q", gid, g.ID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("waiter did not fire after all members terminal")
		}
	})

	t.Run("Group_FailFast_OnFirstFailure_CancelsRest", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
			FailFast:  true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var members []tasks.TaskID
		for i := 0; i < 3; i++ {
			req := freshSpawnReq(tripleA())
			req.GroupID = g.ID
			h, err := r.Spawn(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.MarkRunning(ctx, h.ID); err != nil {
				t.Fatal(err)
			}
			members = append(members, h.ID)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		// Fail the first member → fail-fast cascades cancel to siblings.
		if err := r.MarkFailed(ctx, members[0], tasks.TaskError{Code: "boom"}); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
		// Other members must be Cancelled.
		for i := 1; i < 3; i++ {
			got, err := r.Get(ctx, members[i])
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tasks.StatusCancelled {
				t.Errorf("member %d: status=%q, want Cancelled (fail-fast)", i, got.Status)
			}
		}
		// Group must be Cancelled.
		groups, err := r.ListGroups(ctx, tripleA().Identity, nil)
		if err != nil {
			t.Fatal(err)
		}
		var found *tasks.TaskGroup
		for i := range groups {
			if groups[i].ID == g.ID {
				gg := groups[i]
				found = &gg
				break
			}
		}
		if found == nil {
			t.Fatalf("group %q not in ListGroups output", g.ID)
		}
		if found.Status != tasks.GroupCancelled {
			t.Errorf("group status=%q, want GroupCancelled", found.Status)
		}
	})

	t.Run("Group_Cancel_Cascade_PropagatesToMembers", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		var members []tasks.TaskID
		for i := 0; i < 2; i++ {
			req := freshSpawnReq(tripleA())
			req.GroupID = g.ID
			h, err := r.Spawn(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.MarkRunning(ctx, h.ID); err != nil {
				t.Fatal(err)
			}
			members = append(members, h.ID)
		}
		if err := r.CancelGroup(ctx, g.ID, "user-cancelled", true); err != nil {
			t.Fatalf("CancelGroup: %v", err)
		}
		for _, mid := range members {
			got, err := r.Get(ctx, mid)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tasks.StatusCancelled {
				t.Errorf("member %q status=%q, want Cancelled", mid, got.Status)
			}
		}
	})

	t.Run("Group_Cancel_NoPropagate_LeavesMembersAlone", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		var members []tasks.TaskID
		for i := 0; i < 2; i++ {
			req := freshSpawnReq(tripleA())
			req.GroupID = g.ID
			h, err := r.Spawn(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.MarkRunning(ctx, h.ID); err != nil {
				t.Fatal(err)
			}
			members = append(members, h.ID)
		}
		if err := r.CancelGroup(ctx, g.ID, "no-propagate", false); err != nil {
			t.Fatal(err)
		}
		for _, mid := range members {
			got, err := r.Get(ctx, mid)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tasks.StatusRunning {
				t.Errorf("member %q status=%q, want Running (no-propagate)", mid, got.Status)
			}
		}
	})

	t.Run("WatchGroup_Push_DeliversCompletionPayload", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID:   tripleA().Identity,
			Description: "watch-push",
		})
		if err != nil {
			t.Fatal(err)
		}
		var members []tasks.TaskID
		for i := 0; i < 3; i++ {
			req := freshSpawnReq(tripleA())
			req.GroupID = g.ID
			h, err := r.Spawn(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			members = append(members, h.ID)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		watch, cancel, err := r.WatchGroup(tripleA().Identity, g.ID)
		if err != nil {
			t.Fatalf("WatchGroup: %v", err)
		}
		defer cancel()
		for i, mid := range members {
			if err := r.MarkRunning(ctx, mid); err != nil {
				t.Fatal(err)
			}
			if err := r.MarkComplete(ctx, mid, tasks.TaskResult{
				Value: []byte(fmt.Sprintf(`{"member":%d}`, i)),
			}); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case completion, ok := <-watch:
			if !ok {
				t.Fatal("WatchGroup channel closed without delivery")
			}
			if completion.GroupID != g.ID {
				t.Errorf("completion.GroupID=%q, want %q", completion.GroupID, g.ID)
			}
			if completion.FinalStatus != tasks.GroupCompleted {
				t.Errorf("FinalStatus=%q, want GroupCompleted", completion.FinalStatus)
			}
			if len(completion.Members) != 3 {
				t.Errorf("len(Members)=%d, want 3", len(completion.Members))
			}
			for i, m := range completion.Members {
				if m.Status != tasks.StatusComplete {
					t.Errorf("member %d Status=%q, want StatusComplete", i, m.Status)
				}
				if m.Result == nil {
					t.Errorf("member %d Result=nil, want populated", i)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatal("WatchGroup did not deliver after all members terminal")
		}
		// Channel must be closed after the delivery (close-once
		// invariant).
		select {
		case _, ok := <-watch:
			if ok {
				t.Error("WatchGroup channel re-delivered after close")
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("WatchGroup channel was not closed after delivery")
		}
	})

	t.Run("WatchGroup_RefShaped_MemberResultRoundTrips", func(t *testing.T) {
		// MemberOutcome.Result is ref-shaped per D-022 / D-026. Producers
		// (tools, sub-tasks) must already be substituting heavy outputs
		// with ArtifactRefs upstream; the registry round-trips whatever
		// the caller put in. This subtest stuffs an ArtifactRef-shaped
		// JSON into a member's TaskResult.Value and asserts the
		// GroupCompletion payload carries it unchanged — proving the
		// payload is NOT byte-bound (no re-inlining at the wake-up
		// edge).
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := freshSpawnReq(tripleA())
		req.GroupID = g.ID
		h, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		watch, cancel, err := r.WatchGroup(tripleA().Identity, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		// ArtifactRef-shaped JSON. The actual ArtifactRef shape lives
		// in `internal/artifacts`; the conformance suite mirrors the
		// relevant top-level keys without importing the package (the
		// surface is byte-shaped from the tasks registry's POV).
		refJSON := []byte(`{"artifact_ref":"image_abc123","mime":"image/png","size_bytes":1048576,"hash":"sha256:beef","summary":"large payload"}`)
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: refJSON}); err != nil {
			t.Fatal(err)
		}
		select {
		case completion, ok := <-watch:
			if !ok {
				t.Fatal("WatchGroup channel closed without delivery")
			}
			if len(completion.Members) != 1 {
				t.Fatalf("len(Members)=%d, want 1", len(completion.Members))
			}
			mo := completion.Members[0]
			if mo.Result == nil {
				t.Fatal("MemberOutcome.Result=nil; want ref-shaped JSON")
			}
			var got map[string]any
			if err := json.Unmarshal(mo.Result.Value, &got); err != nil {
				t.Fatalf("unmarshal stored result: %v", err)
			}
			// The audit redactor MAY rewrite caller-controlled string
			// values, but the structural keys (`artifact_ref`,
			// `size_bytes`) remain — verifying the payload is NOT
			// re-inlined and IS ref-shaped.
			if _, has := got["artifact_ref"]; !has {
				t.Errorf("artifact_ref key missing from stored member result: %v", got)
			}
			if _, has := got["size_bytes"]; !has {
				t.Errorf("size_bytes key missing from stored member result: %v", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("WatchGroup did not deliver")
		}
	})

	t.Run("WatchGroup_Push_OnGroupCancelled_DeliversWithReason", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := freshSpawnReq(tripleA())
		req.GroupID = g.ID
		h, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		watch, cancel, err := r.WatchGroup(tripleA().Identity, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		if err := r.CancelGroup(ctx, g.ID, "user-cancelled", true); err != nil {
			t.Fatal(err)
		}
		select {
		case completion, ok := <-watch:
			if !ok {
				t.Fatal("channel closed without delivery on cancel")
			}
			if completion.FinalStatus != tasks.GroupCancelled {
				t.Errorf("FinalStatus=%q, want GroupCancelled", completion.FinalStatus)
			}
			if completion.Reason != "user-cancelled" {
				t.Errorf("Reason=%q, want %q", completion.Reason, "user-cancelled")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("WatchGroup did not deliver on group cancel")
		}
	})

	t.Run("WatchGroup_Poll_GetReturnsTerminalAfterResolve", func(t *testing.T) {
		// Pure poll mode: no WatchGroup subscription; the planner
		// would call ListGroups (or Get for individual tasks) until
		// terminal. Proves the deterministic poll mode works against
		// the same registry surface with no extra primitives.
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := freshSpawnReq(tripleA())
		req.GroupID = g.ID
		h, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
			t.Fatal(err)
		}
		// Poll for terminal status. Bounded retry; no time.Sleep
		// dependency for synchronization (per AGENTS.md §11).
		deadline := time.Now().Add(2 * time.Second)
		var status tasks.TaskGroupStatus
		for time.Now().Before(deadline) {
			groups, err := r.ListGroups(ctx, tripleA().Identity, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, gg := range groups {
				if gg.ID == g.ID {
					status = gg.Status
				}
			}
			if status == tasks.GroupCompleted {
				break
			}
			runtime.Gosched()
		}
		if status != tasks.GroupCompleted {
			t.Errorf("poll-mode terminal status: got %q, want GroupCompleted", status)
		}
	})

	t.Run("WatchGroup_Hybrid_PushAndPollCoexist", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := freshSpawnReq(tripleA())
		req.GroupID = g.ID
		h, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		watch, cancelWatch, err := r.WatchGroup(tripleA().Identity, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer cancelWatch()

		// Sidecar poll goroutine: scans the group's status every ~10ms
		// and records the first terminal observation. Uses a stop
		// channel so we can join cleanly without time.Sleep
		// dependencies.
		stop := make(chan struct{})
		pollObserved := make(chan tasks.TaskGroupStatus, 1)
		go func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					groups, err := r.ListGroups(ctx, tripleA().Identity, nil)
					if err != nil {
						return
					}
					for _, gg := range groups {
						if gg.ID == g.ID && (gg.Status == tasks.GroupCompleted || gg.Status == tasks.GroupCancelled) {
							select {
							case pollObserved <- gg.Status:
							default:
							}
							return
						}
					}
				}
			}
		}()
		defer close(stop)

		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
			t.Fatal(err)
		}
		// Push side: WatchGroup delivers.
		select {
		case completion := <-watch:
			if completion.FinalStatus != tasks.GroupCompleted {
				t.Errorf("push: FinalStatus=%q, want GroupCompleted", completion.FinalStatus)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("push side did not deliver")
		}
		// Poll side: sidecar observes terminal status.
		select {
		case status := <-pollObserved:
			if status != tasks.GroupCompleted {
				t.Errorf("poll: status=%q, want GroupCompleted", status)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("poll side did not observe terminal status")
		}
	})

	t.Run("WatchGroup_Unsubscribe_BeforeResolve_NoLeak", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := freshSpawnReq(tripleA())
		req.GroupID = g.ID
		h, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		watch, cancelWatch, err := r.WatchGroup(tripleA().Identity, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Unsubscribe BEFORE the resolve path runs.
		cancelWatch()
		// The cancel func MUST have closed the channel exactly once;
		// the subsequent resolve path MUST NOT send on the now-closed
		// channel.
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
			t.Fatal(err)
		}
		// Drain: the channel should be closed (the cancel path closed
		// it without delivering), so the receive yields the zero value
		// with `ok=false`.
		select {
		case completion, ok := <-watch:
			if ok {
				t.Errorf("cancelled subscription received a delivery: %+v", completion)
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("cancelled subscription channel was not closed")
		}
		// Second cancel must be a no-op (no panic on double-close).
		cancelWatch()
	})

	t.Run("WatchGroup_AlreadyResolvedGroup_ReturnsErrGroupNotFound", func(t *testing.T) {
		// Two sub-cases per the plan:
		//   (a) Resolved-but-still-tracked group: WatchGroup returns
		//       a channel pre-primed with the cached completion.
		//   (b) Truly unknown group ID: returns ErrGroupNotFound.
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := freshSpawnReq(tripleA())
		req.GroupID = g.ID
		h, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
			t.Fatal(err)
		}
		// (a) Resolved-but-still-tracked: WatchGroup returns the
		// already-primed channel (cached completion).
		watch, cancelLate, err := r.WatchGroup(tripleA().Identity, g.ID)
		if err != nil {
			t.Fatalf("late WatchGroup on resolved-tracked group: err=%v, want nil + primed channel", err)
		}
		defer cancelLate()
		select {
		case completion, ok := <-watch:
			if !ok {
				t.Error("late subscriber received closed channel without cached completion")
			}
			if completion.FinalStatus != tasks.GroupCompleted {
				t.Errorf("late subscriber FinalStatus=%q, want GroupCompleted", completion.FinalStatus)
			}
		case <-time.After(500 * time.Millisecond):
			t.Error("late subscriber's primed channel did not deliver cached completion")
		}
		// (b) Truly unknown group ID → ErrGroupNotFound.
		_, _, err = r.WatchGroup(tripleA().Identity, tasks.TaskGroupID("no-such-group"))
		if !errors.Is(err, tasks.ErrGroupNotFound) {
			t.Errorf("unknown-id: err=%v, want ErrGroupNotFound", err)
		}
	})

	t.Run("WatchGroup_MultipleSubscribers_AllReceive", func(t *testing.T) {
		// D-025 concurrent-reuse angle on the new subscriber path.
		// N=4 concurrent WatchGroup calls; all 4 must receive the same
		// payload at resolve time. Mirrors Phase 20's
		// Concurrent_SpawnGetCancel_NoRace but targets the wake-up
		// surface introduced in Phase 21.
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
			SessionID: tripleA().Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		req := freshSpawnReq(tripleA())
		req.GroupID = g.ID
		h, err := r.Spawn(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.SealGroup(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		const subscribers = 4
		channels := make([]<-chan tasks.GroupCompletion, subscribers)
		cancels := make([]func(), subscribers)
		for i := 0; i < subscribers; i++ {
			ch, cancel, err := r.WatchGroup(tripleA().Identity, g.ID)
			if err != nil {
				t.Fatalf("subscriber %d: %v", i, err)
			}
			channels[i] = ch
			cancels[i] = cancel
		}
		defer func() {
			for _, c := range cancels {
				c()
			}
		}()
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
			t.Fatal(err)
		}
		var first tasks.GroupCompletion
		for i, ch := range channels {
			select {
			case completion := <-ch:
				if i == 0 {
					first = completion
				} else {
					if completion.GroupID != first.GroupID {
						t.Errorf("subscriber %d GroupID=%q, want %q", i, completion.GroupID, first.GroupID)
					}
					if completion.FinalStatus != first.FinalStatus {
						t.Errorf("subscriber %d FinalStatus=%q, want %q", i, completion.FinalStatus, first.FinalStatus)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("subscriber %d did not receive completion", i)
			}
		}
	})

	t.Run("Patch_Apply_HappyPath", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		seeder, ok := r.(patchSeeder)
		if !ok {
			t.Skip("driver does not implement patchSeeder; skip patch subtests")
		}
		if _, err := seeder.CreatePendingPatch(ctx, tripleA().Identity, "p-1", []byte(`{"k":"v"}`)); err != nil {
			t.Fatal(err)
		}
		applied, err := r.ApplyPatch(ctx, tripleA().Identity, "p-1", tasks.PatchAccept)
		if err != nil {
			t.Fatalf("ApplyPatch: %v", err)
		}
		if !applied {
			t.Error("ApplyPatch returned false on a real transition")
		}
		// Idempotent re-apply: same target → (false, nil).
		applied, err = r.ApplyPatch(ctx, tripleA().Identity, "p-1", tasks.PatchAccept)
		if err != nil {
			t.Fatal(err)
		}
		if applied {
			t.Error("ApplyPatch returned true on idempotent re-apply")
		}
	})

	t.Run("Patch_Apply_Reject_HappyPath", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		seeder, ok := r.(patchSeeder)
		if !ok {
			t.Skip("driver does not implement patchSeeder; skip patch subtests")
		}
		if _, err := seeder.CreatePendingPatch(ctx, tripleA().Identity, "p-rej", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		rejected, err := r.ApplyPatch(ctx, tripleA().Identity, "p-rej", tasks.PatchReject)
		if err != nil {
			t.Fatalf("ApplyPatch reject: %v", err)
		}
		if !rejected {
			t.Error("ApplyPatch returned false on a real reject transition")
		}
	})

	t.Run("Patch_Apply_NotFound", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		_, err := r.ApplyPatch(ctx, tripleA().Identity, "no-such-patch", tasks.PatchAccept)
		if !errors.Is(err, tasks.ErrPatchNotFound) {
			t.Errorf("ApplyPatch missing: err=%v, want ErrPatchNotFound", err)
		}
	})

	t.Run("Acknowledge_Background_EmitsPerTaskEvents", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		var ids []tasks.TaskID
		for i := 0; i < 3; i++ {
			req := freshSpawnReq(tripleA())
			req.Kind = tasks.KindBackground
			h, err := r.Spawn(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.MarkRunning(ctx, h.ID); err != nil {
				t.Fatal(err)
			}
			if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, h.ID)
		}
		count, err := r.AcknowledgeBackground(ctx, tripleA().Identity, ids)
		if err != nil {
			t.Fatalf("AcknowledgeBackground: %v", err)
		}
		if count != 3 {
			t.Errorf("ack count=%d, want 3", count)
		}
		// Re-ack: idempotent (no transition; count=0).
		count, err = r.AcknowledgeBackground(ctx, tripleA().Identity, ids)
		if err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("re-ack count=%d, want 0 (idempotent)", count)
		}
	})

	t.Run("Group_CrossSession_Isolation", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		idA := identity.Identity{TenantID: "T", UserID: "U", SessionID: "sess-A"}
		idB := identity.Identity{TenantID: "T", UserID: "U", SessionID: "sess-B"}
		ctxAA, _ := identity.With(context.Background(), idA)
		ctxBB, _ := identity.With(context.Background(), idB)
		g, err := r.ResolveOrCreateGroup(ctxAA, tasks.GroupRequest{
			SessionID: idA,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Session B cannot seal A's group.
		if err := r.SealGroup(ctxBB, g.ID); !errors.Is(err, tasks.ErrGroupNotFound) {
			t.Errorf("cross-session SealGroup: err=%v, want ErrGroupNotFound", err)
		}
		// Session B cannot cancel A's group.
		if err := r.CancelGroup(ctxBB, g.ID, "x", true); !errors.Is(err, tasks.ErrGroupNotFound) {
			t.Errorf("cross-session CancelGroup: err=%v, want ErrGroupNotFound", err)
		}
		// Session B cannot watch A's group.
		_, _, err = r.WatchGroup(idB, g.ID)
		if !errors.Is(err, tasks.ErrGroupNotFound) {
			t.Errorf("cross-session WatchGroup: err=%v, want ErrGroupNotFound", err)
		}
		// Session B's ListGroups output excludes A's group.
		listB, err := r.ListGroups(ctxBB, idB, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, gg := range listB {
			if gg.ID == g.ID {
				t.Errorf("cross-session list leaked group %q", g.ID)
			}
		}
	})

	t.Run("Group_Concurrent_AddRemoveSeal_NoRace", func(t *testing.T) {
		// D-025: N≥64 concurrent group operations against a single
		// shared TaskRegistry under -race. Asserts no data races, no
		// goroutine leak after teardown.
		r, cleanup := factory()
		defer cleanup()
		baseline := runtime.NumGoroutine()
		const goroutines = 64
		const opsPerGo = 4

		var wg sync.WaitGroup
		var errs atomic.Int64
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			i := i
			go func() {
				defer wg.Done()
				ident := identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", i%7),
					UserID:    fmt.Sprintf("u-%d", i%13),
					SessionID: fmt.Sprintf("s-%d", i),
				}
				ctx, ierr := identity.With(context.Background(), ident)
				if ierr != nil {
					errs.Add(1)
					return
				}
				for j := 0; j < opsPerGo; j++ {
					g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
						SessionID: ident,
					})
					if err != nil {
						errs.Add(1)
						return
					}
					// Spawn a member, seal, then mark terminal.
					req := tasks.SpawnRequest{
						Identity: identity.Quadruple{Identity: ident},
						Kind:     tasks.KindForeground,
						GroupID:  g.ID,
					}
					h, err := r.Spawn(ctx, req)
					if err != nil {
						errs.Add(1)
						return
					}
					if err := r.SealGroup(ctx, g.ID); err != nil {
						errs.Add(1)
						return
					}
					if err := r.MarkRunning(ctx, h.ID); err != nil {
						errs.Add(1)
						return
					}
					if j%2 == 0 {
						if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
							errs.Add(1)
							return
						}
					} else {
						if err := r.CancelGroup(ctx, g.ID, "test", true); err != nil {
							errs.Add(1)
							return
						}
					}
				}
			}()
		}
		wg.Wait()
		if n := errs.Load(); n != 0 {
			t.Fatalf("%d concurrent group operations errored", n)
		}
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 0 {
			t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
		}
	})

	// Silence unused-imports complaint when no group seeders are wired.
	_ = (groupSeeder)(nil)
}
