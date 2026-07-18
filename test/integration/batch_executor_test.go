// batch_executor_test.go — end-to-end coverage for the Batch executor on
// the ASSEMBLED production stack with REAL drivers (CLAUDE.md §17.3): a
// real inprocess TaskRegistry over an inmem StateStore + a real inmem
// EventBus + artifact store + in-proc tool catalog, all wired exactly as
// assemble.go wires them (including the new dispatch.WithMaxBatchSpawns
// and the steering.WithHardCancelHook seam this phase closes).
//
// Covers: a mixed tool+spawn Batch dispatched through stack.Executor with
// identity propagating onto every spawned task record; two whole-batch
// failure modes (breadth cap, FailFast disagreement) with zero side
// effects; the cancellation hierarchy (run-level cascade via the same
// registry Cancel the hard-cancel hook wires + cross-session isolation +
// the "no uncancellable task" direct-cancel invariant); and N≥10
// concurrent sessions each dispatching a Batch with no cross-session
// leakage. Runs under -race.
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	// Production driver aggregator (D-196) — the single sanctioned
	// blank-import home; the same import a headless embedder adds.
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	// Dev-only mock LLM (D-089) — explicit opt-in, never in the aggregator.
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// batchExecutorTool is an in-proc tool that records the identity triple
// it observed on invocation, so the test can assert identity provenance
// flows onto every tool branch.
func batchExecutorTool(seen *sync.Map) tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Tool: tools.Tool{Name: "note"},
		Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			if q, ok := identity.QuadrupleFrom(ctx); ok {
				seen.Store(q.SessionID, true)
			}
			return tools.ToolResult{Value: map[string]any{"noted": true}}, nil
		},
	}
}

// buildBatchStack assembles a production stack with the mock LLM, the
// "note" tool pre-registered, and the given batch-spawn breadth cap
// (0 → the config default). Returns the stack and the identity-seen map.
func buildBatchStack(t *testing.T, maxBatchSpawns int) (*assemble.Stack, *sync.Map) {
	t.Helper()
	cfg := config.Defaults()
	cfg.LLM.Driver = "mock"
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.Timeout = 5 * time.Second
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{
		"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"},
	}
	cfg.Planner.MaxBatchSpawns = maxBatchSpawns
	if err := cfg.ValidateCore(); err != nil {
		t.Fatalf("ValidateCore: %v", err)
	}
	seen := &sync.Map{}
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		PreRegisterTools: []tools.ToolDescriptor{batchExecutorTool(seen)},
	})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close(context.Background()) })
	return stack, seen
}

// batchIDCtx returns a quadruple-scoped ctx (tool half reads the
// quadruple) and the matching RunContext for the given session + run.
func batchIDCtx(t *testing.T, id identity.Identity, runID string) (context.Context, planner.RunContext) {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx, planner.RunContext{Quadruple: identity.Quadruple{Identity: id, RunID: runID}}
}

// spawnParent creates a real parent run task under `id` and returns its
// id (used as the batch's RunID so batch-spawned children carry it as
// ParentTaskID and the run-level cascade reaches them).
func spawnParent(t *testing.T, reg tasks.TaskRegistry, ctx context.Context, id identity.Identity) tasks.TaskID {
	t.Helper()
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id},
		Kind:     tasks.KindForeground,
		Query:    "parent run",
	})
	if err != nil {
		t.Fatalf("spawn parent: %v", err)
	}
	return h.ID
}

func mixedBatch(tenant string) planner.Batch {
	return planner.Batch{
		Tools: []planner.CallTool{
			{Tool: "note", Args: json.RawMessage(`{}`), CallID: "t0"},
			{Tool: "note", Args: json.RawMessage(`{}`), CallID: "t1"},
		},
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "sub-a-" + tenant}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "sub-b-" + tenant}, CallID: "s1"},
			{Spec: planner.SpawnSpec{Query: "sub-c-" + tenant}, CallID: "s2"},
		},
	}
}

// TestBatchExecutor_MixedDispatch_IdentityPropagation — a Batch mixing 2
// tool branches + 3 spawns dispatches all 5; identity propagates onto
// every spawned task record (triple + ParentTaskID) and every tool
// invocation's provenance; the 3 ungrouped spawns share ONE auto-group.
func TestBatchExecutor_MixedDispatch_IdentityPropagation(t *testing.T) {
	t.Parallel()
	stack, seen := buildBatchStack(t, 0)
	id := identity.Identity{TenantID: "acme", UserID: "u1", SessionID: "sess-mixed"}
	// A triple-scoped ctx for registry reads/parent spawn.
	tripleCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	parentID := spawnParent(t, stack.Tasks, tripleCtx, id)

	ctx, rc := batchIDCtx(t, id, string(parentID))
	rawAny, _, err := stack.Executor.ExecuteDecision(ctx, rc, mixedBatch("acme"))
	if err != nil {
		t.Fatalf("ExecuteDecision(Batch): %v", err)
	}
	raw := rawAny.(planner.BatchObservation)
	if len(raw.Tools) != 2 || len(raw.Spawns) != 3 {
		t.Fatalf("observation tools=%d spawns=%d, want 2/3", len(raw.Tools), len(raw.Spawns))
	}
	// Tool provenance: the tool observed THIS session's identity.
	if _, ok := seen.Load(id.SessionID); !ok {
		t.Errorf("tool did not observe session %q via ctx identity", id.SessionID)
	}
	// Every spawn registered; all three share one auto-group.
	group := raw.Spawns[0].GroupID
	if group == "" {
		t.Fatal("auto-grouped spawns carry no group id")
	}
	for i, sp := range raw.Spawns {
		if sp.Error != "" || sp.TaskID == "" {
			t.Fatalf("spawn %d did not register: %+v", i, sp)
		}
		if sp.GroupID != group {
			t.Errorf("spawn %d group = %q, want the shared auto-group %q", i, sp.GroupID, group)
		}
		// Identity propagation onto the spawned task record.
		task, gErr := stack.Tasks.Get(tripleCtx, tasks.TaskID(sp.TaskID))
		if gErr != nil {
			t.Fatalf("Get(%s): %v", sp.TaskID, gErr)
		}
		if task.Identity.Identity != id {
			t.Errorf("spawned task %s identity = %+v, want %+v", sp.TaskID, task.Identity.Identity, id)
		}
		if task.ParentTaskID == nil || *task.ParentTaskID != parentID {
			t.Errorf("spawned task %s ParentTaskID = %v, want %s", sp.TaskID, task.ParentTaskID, parentID)
		}
	}
}

// TestBatchExecutor_BreadthCap_ZeroSideEffects — with max_batch_spawns=2
// a Batch carrying 3 spawns is rejected whole; no new tasks register
// (TaskRegistry.List shows no growth beyond the parent).
func TestBatchExecutor_BreadthCap_ZeroSideEffects(t *testing.T) {
	t.Parallel()
	stack, _ := buildBatchStack(t, 2)
	id := identity.Identity{TenantID: "acme", UserID: "u1", SessionID: "sess-cap"}
	tripleCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	parentID := spawnParent(t, stack.Tasks, tripleCtx, id)

	before, err := stack.Tasks.List(tripleCtx, id, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("List(before): %v", err)
	}

	ctx, rc := batchIDCtx(t, id, string(parentID))
	_, _, execErr := stack.Executor.ExecuteDecision(ctx, rc, mixedBatch("acme"))
	if execErr == nil {
		t.Fatal("over-cap Batch dispatched without error, want whole-batch rejection")
	}

	after, err := stack.Tasks.List(tripleCtx, id, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("List(after): %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("task count changed %d -> %d on a rejected batch; want zero side effects", len(before), len(after))
	}
}

// TestBatchExecutor_FailFastDisagreement_ZeroSideEffects — a Batch whose
// auto-grouped spawns disagree on FailFast is rejected whole; no new
// tasks register.
func TestBatchExecutor_FailFastDisagreement_ZeroSideEffects(t *testing.T) {
	t.Parallel()
	stack, _ := buildBatchStack(t, 0)
	id := identity.Identity{TenantID: "acme", UserID: "u1", SessionID: "sess-ff"}
	tripleCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	parentID := spawnParent(t, stack.Tasks, tripleCtx, id)

	before, err := stack.Tasks.List(tripleCtx, id, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("List(before): %v", err)
	}

	ctx, rc := batchIDCtx(t, id, string(parentID))
	d := planner.Batch{
		Spawns: []planner.SpawnTask{
			{Spec: planner.SpawnSpec{Query: "a", FailFast: true}, CallID: "s0"},
			{Spec: planner.SpawnSpec{Query: "b", FailFast: false}, CallID: "s1"},
		},
	}
	_, _, execErr := stack.Executor.ExecuteDecision(ctx, rc, d)
	if execErr == nil {
		t.Fatal("FailFast-disagreement Batch dispatched without error, want rejection")
	}

	after, err := stack.Tasks.List(tripleCtx, id, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("List(after): %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("task count changed %d -> %d on a rejected batch; want zero side effects", len(before), len(after))
	}
}

// TestBatchExecutor_CancelHierarchy — the cancellation hierarchy on real
// drivers: a run-level Cancel on the parent task (the SAME registry
// Cancel the assembled steering.WithHardCancelHook seam fires) cascades
// to every batch-spawned descendant (cascade default), a sibling run in a
// DIFFERENT session is untouched (cross-session isolation), and a direct
// Cancel on one descendant always succeeds (the "no uncancellable task"
// invariant — operator > agent > cascade defaults).
func TestBatchExecutor_CancelHierarchy(t *testing.T) {
	t.Parallel()
	stack, _ := buildBatchStack(t, 0)

	idA := identity.Identity{TenantID: "acme", UserID: "u1", SessionID: "sess-cancel-A"}
	idB := identity.Identity{TenantID: "acme", UserID: "u1", SessionID: "sess-cancel-B"}
	ctxA, _ := identity.With(context.Background(), idA)
	ctxB, _ := identity.With(context.Background(), idB)

	parentA := spawnParent(t, stack.Tasks, ctxA, idA)
	parentB := spawnParent(t, stack.Tasks, ctxB, idB)

	dispatchA, rcA := batchIDCtx(t, idA, string(parentA))
	dispatchB, rcB := batchIDCtx(t, idB, string(parentB))

	rawA, _, err := stack.Executor.ExecuteDecision(dispatchA, rcA, mixedBatch("A"))
	if err != nil {
		t.Fatalf("dispatch A: %v", err)
	}
	rawB, _, err := stack.Executor.ExecuteDecision(dispatchB, rcB, mixedBatch("B"))
	if err != nil {
		t.Fatalf("dispatch B: %v", err)
	}
	descA := rawA.(planner.BatchObservation).Spawns
	descB := rawB.(planner.BatchObservation).Spawns

	// Run-level hard cancel on parent A — exactly what the wired
	// hard-cancel hook does: TaskRegistry.Cancel(runID) with the default
	// cascade. Every batch-spawned descendant of A must transition to
	// Cancelled.
	if _, cErr := stack.Tasks.Cancel(ctxA, parentA, "hard-cancel: run-level interrupt"); cErr != nil {
		t.Fatalf("Cancel(parentA): %v", cErr)
	}
	for _, sp := range descA {
		task, gErr := stack.Tasks.Get(ctxA, tasks.TaskID(sp.TaskID))
		if gErr != nil {
			t.Fatalf("Get(descA %s): %v", sp.TaskID, gErr)
		}
		if task.Status != tasks.StatusCancelled {
			t.Errorf("descendant A %s status = %q, want cancelled (run-level cascade)", sp.TaskID, task.Status)
		}
	}

	// Cross-session isolation: session B's descendants are untouched.
	for _, sp := range descB {
		task, gErr := stack.Tasks.Get(ctxB, tasks.TaskID(sp.TaskID))
		if gErr != nil {
			t.Fatalf("Get(descB %s): %v", sp.TaskID, gErr)
		}
		if task.Status == tasks.StatusCancelled {
			t.Errorf("descendant B %s was cancelled by A's run-level cancel — cross-session isolation breach", sp.TaskID)
		}
	}

	// No uncancellable task: a direct operator Cancel on one of B's
	// descendants succeeds regardless of the run's own state.
	ok, cErr := stack.Tasks.Cancel(ctxB, tasks.TaskID(descB[0].TaskID), "operator: direct cancel")
	if cErr != nil {
		t.Fatalf("direct Cancel(descB[0]): %v", cErr)
	}
	if !ok {
		t.Errorf("direct Cancel on a live descendant returned (false, nil); want a real transition")
	}
}

// TestBatchExecutor_ConcurrentSessions_NoLeakage — N concurrent sessions
// each dispatch a Batch against ONE shared assembled stack; each
// session's spawned tasks stay scoped to that session (no cross-session
// leakage), CLAUDE.md §11.
func TestBatchExecutor_ConcurrentSessions_NoLeakage(t *testing.T) {
	t.Parallel()
	const n = 12
	stack, _ := buildBatchStack(t, 0)

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "tenant-" + strconv.Itoa(idx),
				UserID:    "user-" + strconv.Itoa(idx),
				SessionID: "session-" + strconv.Itoa(idx),
			}
			ctx, rc := batchIDCtx(t, id, "run-"+strconv.Itoa(idx))
			rawAny, _, err := stack.Executor.ExecuteDecision(ctx, rc, mixedBatch(strconv.Itoa(idx)))
			if err != nil {
				errCh <- fmt.Errorf("session %d: %w", idx, err)
				return
			}
			raw := rawAny.(planner.BatchObservation)
			if len(raw.Spawns) != 3 {
				errCh <- fmt.Errorf("session %d: spawns=%d, want 3", idx, len(raw.Spawns))
				return
			}
			// This session's List shows exactly its 3 spawns — no leakage.
			tripleCtx, wErr := identity.With(context.Background(), id)
			if wErr != nil {
				errCh <- wErr
				return
			}
			list, lErr := stack.Tasks.List(tripleCtx, id, tasks.TaskFilter{})
			if lErr != nil {
				errCh <- fmt.Errorf("session %d List: %w", idx, lErr)
				return
			}
			if len(list) != 3 {
				errCh <- fmt.Errorf("session %d: List returned %d tasks, want exactly its own 3 (cross-session leakage)", idx, len(list))
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
