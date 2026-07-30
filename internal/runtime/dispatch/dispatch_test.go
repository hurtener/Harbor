// internal/runtime/dispatch/dispatch_test.go — behaviour-parity units
// for the promoted ToolExecutor's CallTool path + the D-025
// concurrent-reuse / cancellation-isolation gates (Phase 110a — D-194).
//
// Real drivers on every seam the tests wire (CLAUDE.md §17.3): the
// inmem artifacts driver backs the D-026 promotion path; the failing
// PutText wrapper in the degradation test wraps the REAL store (it is
// the forced failure mode, not a re-implementation).

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
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// dispatchTestID is the documented dummy identity these tests use.
var dispatchTestID = identity.Identity{
	TenantID:  "tenant-dispatch-test",
	UserID:    "user-dispatch-test",
	SessionID: "session-dispatch-test",
}

func dispatchTestQuad(runID string) identity.Quadruple {
	return identity.Quadruple{Identity: dispatchTestID, RunID: runID}
}

func dispatchTestCtx(t *testing.T, q identity.Quadruple) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

func newTestArtifactStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// registerEcho registers a tool that echoes its args under
// Value["echo"]=name.
func registerEcho(t *testing.T, cat tools.ToolCatalog, name string) {
	t.Helper()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: name},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"echo": name, "args": string(args)}}, nil
		},
	}); err != nil {
		t.Fatalf("register %q: %v", name, err)
	}
}

// failingPutTextStore wraps the REAL inmem store and forces PutText to
// fail — the D-026 artifact-store-failure degradation path's forced
// failure mode.
type failingPutTextStore struct {
	artifacts.ArtifactStore
}

func (failingPutTextStore) PutText(context.Context, artifacts.ArtifactScope, string, artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	return artifacts.ArtifactRef{}, errors.New("forced PutText failure")
}

// TestExecutor_CallTool_DisabledInProjection_HookTargetStillDispatches pins
// the documented full-catalog hook semantics (RFC §6.4 / D-281 amendment):
// the run-completion hook resolves its target through this executor, which
// resolves against the FULL catalog — NOT a planner-projected view. So a tool
// that a planner ExclusionView would DISABLE (hidden from List AND Resolve)
// still dispatches when named as a hook target. This is the property that must
// not silently flip (e.g. by wiring the hook to a filtered executor); the
// run-loop pre-dispatch membership check that would gate remembered names is a
// named follow-up, deliberately not enforced here.
func TestExecutor_CallTool_DisabledInProjection_HookTargetStillDispatches(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	registerEcho(t, cat, "run_transcript_sink")
	store := newTestArtifactStore(t)
	exec := NewToolExecutor(cat, store, nil)

	q := dispatchTestQuad("r-hook")

	// A planner projection that DISABLES the hook target: it is absent from the
	// planner-facing view's List AND Resolve.
	view := tools.NewExclusionView(
		tools.NewPlannerView(cat, tools.CatalogFilter{
			TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID,
		}),
		nil, []string{"run_transcript_sink"},
	)
	if _, ok := view.Resolve("run_transcript_sink"); ok {
		t.Fatal("precondition: the tool must be disabled (absent from the projected Resolve)")
	}

	// The executor edge (the hook's dispatch path) resolves against the full
	// catalog, so the disabled tool still dispatches.
	raw, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q},
		planner.CallTool{Tool: "run_transcript_sink", Args: json.RawMessage(`{"format_version":1}`)})
	if err != nil {
		t.Fatalf("a disabled tool named as a hook target must still dispatch through the full-catalog executor: %v", err)
	}
	m, ok := raw.(map[string]any)
	if !ok || m["echo"] != "run_transcript_sink" {
		t.Fatalf("raw observation = %#v, want the hook tool's echo map", raw)
	}
}

// TestExecutor_CallTool_LightResult_PassesThrough — a result whose JSON
// encoding is under the heavy threshold reaches the planner verbatim:
// llmObservation == raw, no artifact stored.
func TestExecutor_CallTool_LightResult_PassesThrough(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	registerEcho(t, cat, "light")
	store := newTestArtifactStore(t)
	exec := NewToolExecutor(cat, store, nil)

	q := dispatchTestQuad("r-light")
	raw, llmObs, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q},
		planner.CallTool{Tool: "light", Args: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}
	m, ok := raw.(map[string]any)
	if !ok || m["echo"] != "light" {
		t.Fatalf("raw observation = %#v, want the tool's echo map", raw)
	}
	lm, ok := llmObs.(map[string]any)
	if !ok || lm["echo"] != "light" {
		t.Errorf("light llmObservation = %#v, want pass-through of the raw value", llmObs)
	}
	refs, lErr := store.List(context.Background(), artifacts.ArtifactScope{TenantID: dispatchTestID.TenantID})
	if lErr != nil {
		t.Fatalf("store.List: %v", lErr)
	}
	if len(refs) != 0 {
		t.Errorf("light result stored %d artifacts, want 0", len(refs))
	}
}

// TestExecutor_CallTool_HeavyResult_PromotedToArtifact — AC: a result
// whose encoding exceeds the threshold is promoted: the llmObservation
// is the HeavyTruncationSummary map carrying artifact_ref, and the
// artifact lands in the store under the run's identity scope with the
// canonical `source: tool` + `created_at` provenance stamps.
func TestExecutor_CallTool_HeavyResult_PromotedToArtifact(t *testing.T) {
	t.Parallel()
	const threshold = 256
	cat := tools.NewCatalog()
	big := strings.Repeat("y", threshold*4)
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "heavy"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"title": "T", "blob": big}}, nil
		},
	}); err != nil {
		t.Fatalf("register heavy: %v", err)
	}
	store := newTestArtifactStore(t)
	exec := NewToolExecutor(cat, store, nil, WithHeavyThreshold(threshold))

	q := dispatchTestQuad("r-heavy-calltool")
	raw, llmObs, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q},
		planner.CallTool{Tool: "heavy", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}
	// Raw keeps the full untruncated value.
	if rm, ok := raw.(map[string]any); !ok || rm["blob"] != big {
		t.Errorf("raw observation should carry the untruncated value")
	}
	// llmObservation is the truncation summary with the artifact ref.
	lm, ok := llmObs.(map[string]any)
	if !ok {
		t.Fatalf("llmObservation type = %T, want truncation-summary map", llmObs)
	}
	if lm["truncated"] != true || lm["tool"] != "heavy" {
		t.Errorf("summary missing truncated/tool markers: %#v", lm)
	}
	refID, _ := lm["artifact_ref"].(string)
	if refID == "" {
		t.Fatalf("summary missing artifact_ref: %#v", lm)
	}
	// The artifact is scoped to the run's identity triple and stamped
	// with the canonical provenance keys.
	scope := artifacts.ArtifactScope{
		TenantID:  dispatchTestID.TenantID,
		UserID:    dispatchTestID.UserID,
		SessionID: dispatchTestID.SessionID,
	}
	ref, found, gErr := store.GetRef(context.Background(), scope, refID)
	if gErr != nil || !found {
		t.Fatalf("GetRef(%q) = found=%v err=%v, want stored under the run's scope", refID, found, gErr)
	}
	if ref.Source["source"] != "tool" || ref.Source["tool"] != "heavy" {
		t.Errorf("provenance = %#v, want source=tool + tool=heavy", ref.Source)
	}
	// The producer value is a wire contract: D-194 explicitly preserves
	// "dev-tool-executor" so pre-110a consumers keying on it keep working.
	if ref.Source["producer"] != "dev-tool-executor" {
		t.Errorf("provenance producer = %#v, want dev-tool-executor (D-194 preserved key)", ref.Source["producer"])
	}
	if _, hasCreated := ref.Source["created_at"]; !hasCreated {
		t.Errorf("provenance missing created_at stamp: %#v", ref.Source)
	}
}

// TestExecutor_CallTool_HeavyResult_StoreFailure_TruncatesLoudly — the
// D-026 degradation path: a PutText failure degrades to the truncated
// preview summary (no artifact_ref) instead of failing the run.
func TestExecutor_CallTool_HeavyResult_StoreFailure_TruncatesLoudly(t *testing.T) {
	t.Parallel()
	const threshold = 256
	cat := tools.NewCatalog()
	big := strings.Repeat("z", threshold*4)
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "heavy"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"blob": big}}, nil
		},
	}); err != nil {
		t.Fatalf("register heavy: %v", err)
	}
	exec := NewToolExecutor(cat, failingPutTextStore{newTestArtifactStore(t)}, nil,
		WithHeavyThreshold(threshold))

	q := dispatchTestQuad("r-heavy-storefail")
	_, llmObs, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q},
		planner.CallTool{Tool: "heavy", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("ExecuteDecision should degrade, not fail: %v", err)
	}
	lm, ok := llmObs.(map[string]any)
	if !ok || lm["truncated"] != true {
		t.Fatalf("llmObservation = %#v, want truncation summary", llmObs)
	}
	if _, hasRef := lm["artifact_ref"]; hasRef {
		t.Errorf("store-failure summary must NOT carry artifact_ref: %#v", lm)
	}
	if prev, _ := lm["preview"].(string); prev == "" {
		t.Errorf("store-failure summary missing preview fallback: %#v", lm)
	}
}

// TestExecutor_CallTool_NilStore_TruncatesLoudly — the degraded-stack
// shape: no artifact store wired → truncated preview, no artifact_ref,
// no panic.
func TestExecutor_CallTool_NilStore_TruncatesLoudly(t *testing.T) {
	t.Parallel()
	const threshold = 256
	cat := tools.NewCatalog()
	big := strings.Repeat("w", threshold*4)
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "heavy"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"blob": big}}, nil
		},
	}); err != nil {
		t.Fatalf("register heavy: %v", err)
	}
	exec := NewToolExecutor(cat, nil, nil, WithHeavyThreshold(threshold))

	q := dispatchTestQuad("r-heavy-nilstore")
	_, llmObs, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q},
		planner.CallTool{Tool: "heavy", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("ExecuteDecision should degrade, not fail: %v", err)
	}
	lm, ok := llmObs.(map[string]any)
	if !ok || lm["truncated"] != true {
		t.Fatalf("llmObservation = %#v, want truncation summary", llmObs)
	}
	if _, hasRef := lm["artifact_ref"]; hasRef {
		t.Errorf("nil-store summary must NOT carry artifact_ref: %#v", lm)
	}
}

// TestExecutor_CallTool_ErrorShapes — empty tool name, unresolved tool
// (wrapped tools.ErrToolNotFound), nil Invoke, and invoke failure all
// fail loud.
func TestExecutor_CallTool_ErrorShapes(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "boom"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, errors.New("kaboom")
		},
	}); err != nil {
		t.Fatalf("register boom: %v", err)
	}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), nil)
	q := dispatchTestQuad("r-errors")
	ctx := dispatchTestCtx(t, q)
	rc := planner.RunContext{Quadruple: q}

	if _, _, err := exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: ""}); err == nil {
		t.Error("empty tool name accepted, want error")
	}
	_, _, err := exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "missing"})
	if !errors.Is(err, tools.ErrToolNotFound) {
		t.Errorf("unresolved tool err = %v, want ErrToolNotFound", err)
	}
	_, _, err = exec.ExecuteDecision(ctx, rc, planner.CallTool{Tool: "boom", Args: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("invoke failure err = %v, want it to carry the tool's error", err)
	}
}

// TestExecutor_UnsupportedDecisionShape — an unknown Decision concrete
// fails loud with steering.ErrDecisionShapeUnsupported.
func TestExecutor_UnsupportedDecisionShape(t *testing.T) {
	t.Parallel()
	exec := NewToolExecutor(tools.NewCatalog(), newTestArtifactStore(t), nil)
	q := dispatchTestQuad("r-unsupported")
	_, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), planner.RunContext{Quadruple: q}, planner.Finish{})
	if !errors.Is(err, steering.ErrDecisionShapeUnsupported) {
		t.Fatalf("err = %v, want ErrDecisionShapeUnsupported", err)
	}
}

// TestExecutor_TaskOutcomeObservation_ConsumesAnswerEnvelope — the
// AwaitTask observation round-trips the exported planner.AnswerEnvelope
// (the named cmd↔cmd wire contract this phase exports): a Result.Value
// marshalled FROM the typed envelope parses back into the observation's
// `result` map with the envelope's snake_case keys.
func TestExecutor_TaskOutcomeObservation_ConsumesAnswerEnvelope(t *testing.T) {
	t.Parallel()
	envelope, err := json.Marshal(planner.AnswerEnvelope{
		Answer:        "the sub answer",
		FinishReason:  string(planner.FinishGoal),
		ToolCallsSeen: 2,
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	obs := taskOutcomeObservation(&tasks.Task{
		ID:     "task-envelope",
		Status: tasks.StatusComplete,
		Result: &tasks.TaskResult{Value: envelope},
	})
	m, ok := obs.(map[string]any)
	if !ok {
		t.Fatalf("observation type = %T, want map", obs)
	}
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want parsed map", m["result"])
	}
	if result["answer"] != "the sub answer" || result["finish_reason"] != string(planner.FinishGoal) {
		t.Errorf("result = %#v, want the envelope's snake_case keys round-tripped", result)
	}
}

// TestExecutor_ConcurrentReuse_CallTool — D-025 / §11: N≥100 concurrent
// ExecuteDecision(CallTool) invocations against ONE shared executor
// under -race. Asserts: no data race (the gate), no context bleed (each
// run's tool reads ITS OWN identity + run id from ctx), goroutine
// baseline restored after all runs return.
func TestExecutor_ConcurrentReuse_CallTool(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "whoami"},
		Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			q, ok := identity.QuadrupleFrom(ctx)
			if !ok {
				return tools.ToolResult{}, errors.New("missing identity in tool ctx")
			}
			return tools.ToolResult{Value: map[string]any{
				"run":    q.RunID,
				"tenant": q.TenantID,
			}}, nil
		},
	}); err != nil {
		t.Fatalf("register whoami: %v", err)
	}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), nil)

	baseline := runtime.NumGoroutine()
	const n = 120
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
			runID := "run-" + strconv.Itoa(idx)
			ctx, wErr := identity.WithRun(context.Background(), id, runID)
			if wErr != nil {
				errCh <- wErr
				return
			}
			q := identity.Quadruple{Identity: id, RunID: runID}
			raw, _, err := exec.ExecuteDecision(ctx, planner.RunContext{Quadruple: q},
				planner.CallTool{Tool: "whoami", Args: json.RawMessage(`{}`)})
			if err != nil {
				errCh <- fmt.Errorf("run %d: %w", idx, err)
				return
			}
			m, ok := raw.(map[string]any)
			if !ok || m["run"] != runID || m["tenant"] != id.TenantID {
				errCh <- fmt.Errorf("run %d: cross-talk — tool saw %#v", idx, raw)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	assertGoroutineBaseline(t, baseline)
}

// TestExecutor_ParallelCancel_NoCrossTalk — D-025 cancellation
// isolation: cancelling run A's ctx mid-CallParallel aborts A (its
// blocked branch observes ctx-done) while run B's concurrent
// CallParallel on the SAME shared executor completes unaffected.
func TestExecutor_ParallelCancel_NoCrossTalk(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	registerEcho(t, cat, "fast")
	// Buffered so the tool body's non-blocking send latches even if the main
	// goroutine has not yet parked on the receive — an unbuffered channel drops
	// the edge-triggered signal through the `default:` branch under scheduler
	// contention, stalling the test to its timeout (issue #480).
	blockEntered := make(chan struct{}, 1)
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "block"},
		Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			select {
			case blockEntered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return tools.ToolResult{}, ctx.Err()
		},
	}); err != nil {
		t.Fatalf("register block: %v", err)
	}
	exec := NewToolExecutor(cat, newTestArtifactStore(t), nil)

	// Run A: a parallel call whose branch blocks until ctx cancel.
	qa := dispatchTestQuad("r-cancel-a")
	ctxA, cancelA := context.WithCancel(dispatchTestCtx(t, qa))
	defer cancelA()
	aDone := make(chan struct{})
	var aRaw any
	var aErr error
	go func() {
		defer close(aDone)
		aRaw, _, aErr = exec.ExecuteDecision(ctxA, planner.RunContext{Quadruple: qa},
			planner.CallParallel{Branches: []planner.CallTool{
				{Tool: "block", Args: json.RawMessage(`{}`), CallID: "a0"},
			}})
	}()

	// Wait (bounded) until A's branch is parked inside the tool body.
	select {
	case <-blockEntered:
	case <-time.After(30 * time.Second):
		t.Fatal("run A's blocking branch never started")
	}

	// Run B on the SAME executor while A is parked — must complete.
	qb := dispatchTestQuad("r-cancel-b")
	bRaw, _, bErr := exec.ExecuteDecision(dispatchTestCtx(t, qb), planner.RunContext{Quadruple: qb},
		planner.CallParallel{Branches: []planner.CallTool{
			{Tool: "fast", Args: json.RawMessage(`{}`), CallID: "b0"},
		}})
	if bErr != nil {
		t.Fatalf("run B failed while run A parked: %v", bErr)
	}
	if obs, ok := bRaw.(planner.ParallelObservation); !ok || len(obs.Branches) != 1 || obs.Branches[0].Error != "" {
		t.Errorf("run B observation = %#v, want one successful branch", bRaw)
	}

	// Cancel A; it must return promptly with the cancellation surfaced
	// on its branch (non-atomic mode) — and ONLY A is affected.
	cancelA()
	select {
	case <-aDone:
	case <-time.After(30 * time.Second):
		t.Fatal("run A did not return after its ctx was cancelled")
	}
	if aErr != nil {
		// Whole-call abort is acceptable cancellation surfacing too.
		if !errors.Is(aErr, context.Canceled) && !strings.Contains(aErr.Error(), context.Canceled.Error()) {
			t.Errorf("run A err = %v, want cancellation-shaped", aErr)
		}
	} else {
		obs, ok := aRaw.(planner.ParallelObservation)
		if !ok || len(obs.Branches) != 1 || !strings.Contains(obs.Branches[0].Error, context.Canceled.Error()) {
			t.Errorf("run A observation = %#v, want the branch to carry the cancellation", aRaw)
		}
	}
}

// assertGoroutineBaseline polls (bounded) until the goroutine count
// returns to within tolerance of baseline.
func assertGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	// Generous settle window: spawn/await tests start background tasks whose
	// goroutines reap asynchronously, and under heavy `-race` CI load the
	// scheduler reaps them slowly — a tight deadline flakes. A real leak
	// never settles, so a long bound stays a faithful leak check.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
}

// ---------------------------------------------------------------------
// Phase 213 (D-358) — the raised LLM-context heavy-output threshold.
//
// Appended, not interleaved: this file is touched concurrently by a
// sibling phase and every addition below is additive.
// ---------------------------------------------------------------------

// TestExecutor_DefaultThreshold_InlinedBand_NoArtifactWritten is the
// phase's CENTRAL behavioural change, asserted at projectForLLM rather
// than at the config layer: on the DEFAULT threshold a tool result in
// the 32–128 KiB band reaches the planner observation as the RAW value
// with ZERO writes to the artifact store. Before the raise every one of
// these sizes was promoted and cost the planner a stub-then-fetch turn.
//
// Mutation witness: reverting config.DefaultHeavyOutputThresholdBytes to
// 32 KiB fails the 64 KiB and 127 KiB cases here.
func TestExecutor_DefaultThreshold_InlinedBand_NoArtifactWritten(t *testing.T) {
	t.Parallel()
	for _, size := range []int{31 * 1024, 64 * 1024, 127 * 1024} {
		t.Run(strconv.Itoa(size)+"B", func(t *testing.T) {
			t.Parallel()
			marker := strings.Repeat("i", size)
			cat := tools.NewCatalog()
			if err := cat.Register(tools.ToolDescriptor{
				Tool: tools.Tool{Name: "banded"},
				Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
					return tools.ToolResult{Value: map[string]any{"blob": marker}}, nil
				},
			}); err != nil {
				t.Fatalf("register banded: %v", err)
			}
			store := newTestArtifactStore(t)
			// No WithHeavyThreshold: the constructor's fallback resolves
			// the canonical default, which is exactly what an operator
			// who sets nothing gets.
			exec := NewToolExecutor(cat, store, nil)

			q := dispatchTestQuad("r-band-" + strconv.Itoa(size))
			_, llmObs, err := exec.ExecuteDecision(dispatchTestCtx(t, q),
				planner.RunContext{Quadruple: q},
				planner.CallTool{Tool: "banded", Args: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatalf("ExecuteDecision: %v", err)
			}
			lm, ok := llmObs.(map[string]any)
			if !ok {
				t.Fatalf("llmObservation type = %T, want the raw echo map", llmObs)
			}
			if lm["truncated"] == true {
				t.Fatalf("a %d-byte result was promoted; the inlined band tops out at %d",
					size, config.DefaultHeavyOutputThresholdBytes)
			}
			if lm["blob"] != marker {
				t.Error("inlined observation lost the raw value")
			}
			refs, lErr := store.List(context.Background(),
				artifacts.ArtifactScope{TenantID: dispatchTestID.TenantID})
			if lErr != nil {
				t.Fatalf("store.List: %v", lErr)
			}
			if len(refs) != 0 {
				t.Errorf("inlined band wrote %d artifacts, want 0 — the round trip this phase removes", len(refs))
			}
		})
	}
}

// TestExecutor_DefaultThreshold_AboveBand_StillPromoted pins the tail:
// the raise moved the boundary, it did not remove it.
func TestExecutor_DefaultThreshold_AboveBand_StillPromoted(t *testing.T) {
	t.Parallel()
	for _, size := range []int{config.DefaultHeavyOutputThresholdBytes, 256 * 1024} {
		t.Run(strconv.Itoa(size)+"B", func(t *testing.T) {
			t.Parallel()
			cat := tools.NewCatalog()
			blob := strings.Repeat("p", size)
			if err := cat.Register(tools.ToolDescriptor{
				Tool: tools.Tool{Name: "above"},
				Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
					return tools.ToolResult{Value: map[string]any{"blob": blob}}, nil
				},
			}); err != nil {
				t.Fatalf("register above: %v", err)
			}
			store := newTestArtifactStore(t)
			exec := NewToolExecutor(cat, store, nil)

			q := dispatchTestQuad("r-above-" + strconv.Itoa(size))
			_, llmObs, err := exec.ExecuteDecision(dispatchTestCtx(t, q),
				planner.RunContext{Quadruple: q},
				planner.CallTool{Tool: "above", Args: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatalf("ExecuteDecision: %v", err)
			}
			lm, ok := llmObs.(map[string]any)
			if !ok || lm["truncated"] != true {
				t.Fatalf("llmObservation = %T, want the truncation summary at %d bytes", llmObs, size)
			}
			refID, _ := lm["artifact_ref"].(string)
			if refID == "" {
				t.Fatalf("promoted summary missing artifact_ref: %#v", lm)
			}
			scope := artifacts.ArtifactScope{
				TenantID:  dispatchTestID.TenantID,
				UserID:    dispatchTestID.UserID,
				SessionID: dispatchTestID.SessionID,
			}
			if _, found, gErr := store.GetRef(context.Background(), scope, refID); gErr != nil || !found {
				t.Fatalf("GetRef(%q) = found=%v err=%v, want the stub resolvable", refID, found, gErr)
			}
		})
	}
}

// TestExecutor_DefaultThreshold_IsTheLLMContextArm proves the fallback
// resolves the LLM-CONTEXT constant, not the pinned Console bound — the
// assertion that fails if someone "harmonises" the dispatch floor onto
// the Console-facing pin.
func TestExecutor_DefaultThreshold_IsTheLLMContextArm(t *testing.T) {
	t.Parallel()
	if defaultHeavyThreshold != config.DefaultHeavyOutputThresholdBytes {
		t.Fatalf("defaultHeavyThreshold = %d, want the LLM-context arm %d",
			defaultHeavyThreshold, config.DefaultHeavyOutputThresholdBytes)
	}
	if defaultHeavyThreshold == config.DefaultConsoleInlinePayloadBytes {
		t.Fatal("the dispatch promote-to-stub floor must not be the Console inline-payload bound: " +
			"a planner observation IS prompt bytes")
	}
	exec := NewToolExecutor(tools.NewCatalog(), nil, nil, WithHeavyThreshold(0)).(*toolExecutor)
	if exec.heavyThreshold != config.DefaultHeavyOutputThresholdBytes {
		t.Errorf("a non-positive configured threshold resolved to %d, want %d",
			exec.heavyThreshold, config.DefaultHeavyOutputThresholdBytes)
	}
}
