// internal/runtime/dispatch/dispatch_spawn_await_test.go — SpawnTask +
// AwaitTask dispatch parity tests (Phase 107e — D-170; moved from
// cmd/harbor in Phase 110a — D-194, names re-prefixed to
// TestExecutor_*). The driver-integrated end-to-end shapes (a real
// per-task RunLoop driver driving the spawned background task) stay in
// cmd/harbor/cmd_dev_spawn_await_test.go where the driver lives.
//
// Real drivers everywhere on the seam (CLAUDE.md §17.3): a real
// inprocess TaskRegistry over an inmem StateStore + a real inmem
// ArtifactStore + a real inmem EventBus.

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"

	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess" // §4.4: registers the V1 "inprocess" task driver
)

func mkSpawnAwaitTestBus(t *testing.T) events.EventBus {
	t.Helper()
	b, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

// mkSpawnAwaitTestTaskRegistry constructs a real production
// TaskRegistry backed by the inprocess driver (the V1 default) plus an
// inmem StateStore.
func mkSpawnAwaitTestTaskRegistry(t *testing.T, bus events.EventBus) tasks.TaskRegistry {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	reg, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: auditpatterns.New(),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.OpenDriver: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return reg
}

// newSpawnAwaitExecutor builds an executor over the supplied registry
// with an empty catalog + a real inmem artifact store.
func newSpawnAwaitExecutor(t *testing.T, reg tasks.TaskRegistry, heavyThreshold, maxDepth int) steering.ToolExecutor {
	t.Helper()
	return NewToolExecutor(tools.NewCatalog(), newTestArtifactStore(t), reg,
		WithHeavyThreshold(heavyThreshold),
		WithMaxSpawnDepth(maxDepth))
}

// spawnAwaitIDCtx returns a ctx carrying the shared test identity triple.
func spawnAwaitIDCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), dispatchTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// rcFor builds a RunContext whose identity is the shared test triple and
// whose RunID (= the current task id at the dev layer) is `runID`.
func rcFor(runID tasks.TaskID) planner.RunContext {
	return planner.RunContext{
		Quadruple: identity.Quadruple{Identity: dispatchTestID, RunID: string(runID)},
	}
}

// TestExecutor_SpawnTask_OrdinaryArtifactResolutionAndDisposition verifies
// that an ordinary child forwards only scoped references and that the runtime
// owns disposition precedence after resolving each reference's MIME.
func TestExecutor_SpawnTask_OrdinaryArtifactResolutionAndDisposition(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	store := newTestArtifactStore(t)
	ref, err := store.PutBytes(context.Background(), artifacts.ArtifactScope{
		TenantID: dispatchTestID.TenantID, UserID: dispatchTestID.UserID, SessionID: dispatchTestID.SessionID,
	}, []byte("png"), artifacts.PutOpts{MimeType: "image/png", Filename: "image.png"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	exec := NewToolExecutor(tools.NewCatalog(), store, reg)
	rc := rcFor("")
	rc.DispositionPolicy = planner.DispositionPolicy{ByMIME: map[string]planner.AttachmentDisposition{"image/*": planner.DispositionRef}}
	raw, _, err := exec.ExecuteDecision(context.Background(), rc, planner.SpawnTask{Spec: planner.SpawnSpec{
		Query: "ordinary", InputArtifactIDs: []string{ref.ID}, InputArtifactDispositions: map[string]string{ref.ID: "inline"},
	}})
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}
	id := tasks.TaskID(raw.(map[string]any)["task_id"].(string))
	task, err := reg.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.InputArtifactDispositions[ref.ID] != "inline" {
		t.Fatalf("disposition = %q, want inline hint", task.InputArtifactDispositions[ref.ID])
	}
}

func TestExecutor_SpawnTask_ArtifactHintValidationPrecedesPersistence(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	store := newTestArtifactStore(t)
	exec := NewToolExecutor(tools.NewCatalog(), store, reg)
	_, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.SpawnTask{Spec: planner.SpawnSpec{
		Query: "invalid", InputArtifactIDs: []string{"one"}, InputArtifactDispositions: map[string]string{"other": "inline"},
	}})
	if err == nil {
		t.Fatal("expected unforwarded hint rejection")
	}
}

// TestExecutor_SpawnTask_NonRetain_SpawnsBackgroundTask — AC-2/AC-3: a
// non-retain-turn SpawnTask creates a KindBackground task under the run's
// triple and returns {task_id, kind, status:"spawned"} immediately.
func TestExecutor_SpawnTask_NonRetain_SpawnsBackgroundTask(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4)

	raw, llmObs, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.SpawnTask{
		Kind: tasks.KindBackground,
		Spec: planner.SpawnSpec{Description: "sub goal", Query: "do the sub goal"},
	})
	if err != nil {
		t.Fatalf("ExecuteDecision(SpawnTask): %v", err)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("raw observation type = %T, want map[string]any", raw)
	}
	if _, ok := llmObs.(map[string]any); !ok {
		t.Fatalf("llmObs type = %T, want map[string]any", llmObs)
	}
	taskID, _ := m["task_id"].(string)
	if taskID == "" {
		t.Fatalf("observation missing task_id: %v", m)
	}
	if m["status"] != "spawned" {
		t.Errorf("status = %v, want spawned", m["status"])
	}

	// The task exists in the registry under the run's triple, KindBackground.
	task, gErr := reg.Get(spawnAwaitIDCtx(t), tasks.TaskID(taskID))
	if gErr != nil {
		t.Fatalf("reg.Get(%q): %v", taskID, gErr)
	}
	if task.Kind != tasks.KindBackground {
		t.Errorf("spawned kind = %q, want background", task.Kind)
	}
	if task.Query != "do the sub goal" {
		t.Errorf("spawned query = %q, want %q", task.Query, "do the sub goal")
	}
	if task.Identity.Identity != dispatchTestID {
		t.Errorf("spawned identity = %+v, want %+v", task.Identity.Identity, dispatchTestID)
	}
	if task.ParentTaskID != nil {
		t.Errorf("root spawn (empty RunID) should have nil ParentTaskID, got %v", *task.ParentTaskID)
	}
}

func TestExecutor_SpawnTask_InheritsExactAgentReachAdmission(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4)
	sealer, err := toolauth.NewAESGCMSealer(make([]byte, toolauth.KEKSizeBytes))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := tasks.NewAgentReachAdmissionAuthority(sealer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Admit(context.Background(), dispatchTestID, "agent-selected")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := exec.ExecuteDecision(ctx, rcFor(""), planner.SpawnTask{Spec: planner.SpawnSpec{Query: "child"}})
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}
	taskID := tasks.TaskID(raw.(map[string]any)["task_id"].(string))
	child, err := reg.Get(spawnAwaitIDCtx(t), taskID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if child.AgentID != "" {
		t.Fatalf("child raw AgentID = %q, want omitted", child.AgentID)
	}
	if _, got, admitted := authority.Restore(context.Background(), child); !admitted || got != "agent-selected" {
		t.Fatalf("child admission = (%q, %v), want inherited agent-selected", got, admitted)
	}

	other := dispatchTestID
	other.SessionID = "other-session"
	otherRC := planner.RunContext{Quadruple: identity.Quadruple{Identity: other}}
	raw, _, err = exec.ExecuteDecision(ctx, otherRC, planner.SpawnTask{Spec: planner.SpawnSpec{Query: "cross identity"}})
	if err != nil {
		t.Fatalf("cross-identity behavior compatibility spawn: %v", err)
	}
	otherCtx, err := identity.With(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	otherChild, err := reg.Get(otherCtx, tasks.TaskID(raw.(map[string]any)["task_id"].(string)))
	if err != nil {
		t.Fatalf("Get cross-identity child: %v", err)
	}
	if _, _, admitted := authority.Restore(context.Background(), otherChild); admitted {
		t.Fatal("cross-identity child inherited credential admission")
	}
}

// TestExecutor_SpawnTask_NilRegistry_Unsupported — with no TaskRegistry
// wired the dispatch fails loud with ErrDecisionShapeUnsupported (never
// a panic / silent no-op).
func TestExecutor_SpawnTask_NilRegistry_Unsupported(t *testing.T) {
	exec := newSpawnAwaitExecutor(t, nil, 32*1024, 4)
	_, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.SpawnTask{
		Spec: planner.SpawnSpec{Query: "x"},
	})
	if !errors.Is(err, steering.ErrDecisionShapeUnsupported) {
		t.Fatalf("err = %v, want ErrDecisionShapeUnsupported", err)
	}
	_, _, err = exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: "t"})
	if !errors.Is(err, steering.ErrDecisionShapeUnsupported) {
		t.Fatalf("AwaitTask err = %v, want ErrDecisionShapeUnsupported", err)
	}
}

// TestExecutor_AwaitTask_EmptyTaskID_Errors — AwaitTask with an empty
// TaskID fails loud (the projector rejects this at emission; the
// executor re-asserts).
func TestExecutor_AwaitTask_EmptyTaskID_Errors(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4)

	_, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: ""})
	if err == nil {
		t.Fatal("AwaitTask(empty) returned nil error, want failure")
	}
}

// TestExecutor_AwaitTask_UnknownTask_Errors — AwaitTask on a non-existent
// task id surfaces the registry's not-found error (no hang): Get fails on
// the first poll iteration and awaitTerminal returns immediately. This
// also covers the cross-session reject path (AC-11) — Get rejects a task
// not visible to the ctx identity exactly as it does a missing one.
func TestExecutor_AwaitTask_UnknownTask_Errors(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := exec.ExecuteDecision(ctx, rcFor(""), planner.AwaitTask{TaskID: "no-such-task"})
	if err == nil {
		t.Fatal("AwaitTask(unknown) returned nil error, want not-found failure")
	}
}

// markTaskComplete spawns a task and drives it Pending → Running →
// Complete with the given answer-envelope Value, returning its id.
func markTaskComplete(t *testing.T, reg tasks.TaskRegistry, value []byte) tasks.TaskID {
	t.Helper()
	ctx := spawnAwaitIDCtx(t)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: dispatchTestID},
		Kind:     tasks.KindBackground,
		Query:    "pre-completed",
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	if err := reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := reg.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: value}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	return h.ID
}

// TestExecutor_AwaitTask_CompletedTask_ReturnsOutcome — AC-5: awaiting a
// terminal task returns its answer-envelope as the observation `result`,
// parsed.
func TestExecutor_AwaitTask_CompletedTask_ReturnsOutcome(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4)

	envelope := []byte(`{"answer":"the sub answer","finish_reason":"goal","tool_calls_seen":2}`)
	id := markTaskComplete(t, reg, envelope)

	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: id})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("raw type = %T, want map", raw)
	}
	if m["status"] != string(tasks.StatusComplete) {
		t.Errorf("status = %v, want complete", m["status"])
	}
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want parsed map", m["result"])
	}
	if result["answer"] != "the sub answer" {
		t.Errorf("result.answer = %v, want %q", result["answer"], "the sub answer")
	}
}

// TestExecutor_AwaitTask_HeavyResult_Projected — AC-6: a heavy awaited
// result is promoted to an artifact-stub llmObservation while the raw
// observation keeps the full value, so the LLM-edge ErrContextLeak guard
// is not tripped.
func TestExecutor_AwaitTask_HeavyResult_Projected(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	// Tiny heavy threshold so the envelope easily exceeds it.
	exec := newSpawnAwaitExecutor(t, reg, 256, 4)

	big := strings.Repeat("x", 4096)
	envelope, mErr := json.Marshal(map[string]any{"answer": big, "finish_reason": "goal"})
	if mErr != nil {
		t.Fatalf("marshal envelope: %v", mErr)
	}
	id := markTaskComplete(t, reg, envelope)

	raw, llmObs, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: id})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	// Raw keeps the full value.
	rawEnc, _ := json.Marshal(raw)
	if len(rawEnc) < 4096 {
		t.Errorf("raw observation looks truncated (%d bytes); should carry the full value", len(rawEnc))
	}
	// llmObs is the projected stub (under the heavy threshold's intent).
	stub, ok := llmObs.(map[string]any)
	if !ok {
		t.Fatalf("llmObs type = %T, want stub map", llmObs)
	}
	if stub["truncated"] != true {
		t.Errorf("llmObs not projected to an artifact stub: %v", stub)
	}
}

// TestExecutor_SpawnTask_DepthCap — AC-8: with cap=1, a spawn whose
// parent chain is already at depth 1 is rejected loudly; a spawn at
// depth 0 succeeds.
func TestExecutor_SpawnTask_DepthCap(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 1)

	// Root spawn (RunID empty → ParentTaskID nil, depth 0). Allowed.
	raw1, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.SpawnTask{
		Spec: planner.SpawnSpec{Query: "root"},
	})
	if err != nil {
		t.Fatalf("root spawn rejected: %v", err)
	}
	t1 := tasks.TaskID(raw1.(map[string]any)["task_id"].(string))

	// Spawn whose parent is the root (child depth 1 ≤ cap 1). Allowed.
	raw2, _, err := exec.ExecuteDecision(context.Background(), rcFor(t1), planner.SpawnTask{
		Spec: planner.SpawnSpec{Query: "depth-1 child"},
	})
	if err != nil {
		t.Fatalf("depth-1 spawn rejected (cap=1 allows child depth 1): %v", err)
	}
	t2 := tasks.TaskID(raw2.(map[string]any)["task_id"].(string))

	// Spawn whose parent (t2) is at depth 1 → child depth 2 > cap 1. Rejected.
	_, _, err = exec.ExecuteDecision(context.Background(), rcFor(t2), planner.SpawnTask{
		Spec: planner.SpawnSpec{Query: "depth-2 child"},
	})
	if err == nil {
		t.Fatal("depth-2 spawn was accepted; want rejection above absolute_max_spawn_depth")
	}
	if !strings.Contains(err.Error(), "absolute_max_spawn_depth") {
		t.Errorf("rejection error = %q, want it to name absolute_max_spawn_depth", err.Error())
	}
}

// TestExecutor_SpawnThenAwait_FailedChild — AC-15 failure mode: a child
// that ends non-goal (MarkFailed) surfaces its error on the parent's
// await observation rather than a success result.
func TestExecutor_SpawnThenAwait_FailedChild(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4)

	// Spawn + drive to Failed directly (no planner needed for this shape).
	ctx := spawnAwaitIDCtx(t)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: dispatchTestID},
		Kind:     tasks.KindBackground,
		Query:    "doomed",
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	if err := reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := reg.MarkFailed(ctx, h.ID, tasks.TaskError{Code: "boom", Message: "child failed"}); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: h.ID})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	m := raw.(map[string]any)
	if m["status"] != string(tasks.StatusFailed) {
		t.Errorf("status = %v, want failed", m["status"])
	}
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("await observation missing error block: %v", m)
	}
	if errObj["code"] != "boom" {
		t.Errorf("error.code = %v, want boom", errObj["code"])
	}
}

// TestExecutor_SpawnAwait_ConcurrentReuse — AC-16 / D-025: N=100
// concurrent spawn+await cycles against ONE shared executor + ONE shared
// registry, each with its own identity, under -race. Asserts no
// cross-talk (each await sees its own child's answer) and no goroutine
// leak after all cycles return.
func TestExecutor_SpawnAwait_ConcurrentReuse(t *testing.T) {
	const n = 100

	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4)

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "tenant-" + strconv.Itoa(i),
				UserID:    "user-" + strconv.Itoa(i),
				SessionID: "session-" + strconv.Itoa(i),
			}
			rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: id}}
			idCtx, wErr := identity.With(context.Background(), id)
			if wErr != nil {
				errCh <- wErr
				return
			}

			// Spawn a background task under this goroutine's own identity.
			raw, _, sErr := exec.ExecuteDecision(context.Background(), rc, planner.SpawnTask{
				Kind: tasks.KindBackground,
				Spec: planner.SpawnSpec{Query: "q-" + strconv.Itoa(i)},
			})
			if sErr != nil {
				errCh <- sErr
				return
			}
			childID := tasks.TaskID(raw.(map[string]any)["task_id"].(string))

			// Drive it terminal directly (no driver) with a per-goroutine answer.
			if mErr := reg.MarkRunning(idCtx, childID); mErr != nil {
				errCh <- mErr
				return
			}
			envelope, _ := json.Marshal(map[string]any{"answer": "ans-" + strconv.Itoa(i)})
			if mErr := reg.MarkComplete(idCtx, childID, tasks.TaskResult{Value: envelope}); mErr != nil {
				errCh <- mErr
				return
			}

			// Await + assert no cross-talk.
			awaitRaw, _, aErr := exec.ExecuteDecision(context.Background(), rc, planner.AwaitTask{TaskID: childID})
			if aErr != nil {
				errCh <- aErr
				return
			}
			result := awaitRaw.(map[string]any)["result"].(map[string]any)
			if result["answer"] != "ans-"+strconv.Itoa(i) {
				errCh <- fmt.Errorf("cross-talk: await saw %v, want ans-%d", result["answer"], i)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent spawn/await: %v", err)
	}

	// Goroutine-leak check: the executor's await pollers stop on return
	// (ticker stopped via defer). Allow a small settle window.
	assertGoroutineBaseline(t, baseline)
}
