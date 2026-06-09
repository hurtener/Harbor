// internal/runtime/dispatch/dispatch_parallel_test.go — CallParallel
// dispatch parity tests (Phase 107d — D-169; moved from cmd/harbor in
// Phase 110a — D-194, names re-prefixed to TestExecutor_*).

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

func newParallelTestExecutor(t *testing.T, heavyThreshold int) (*toolExecutor, tools.ToolCatalog) {
	t.Helper()
	cat := tools.NewCatalog()
	// Phase 107e: the parallel-path tests do not exercise spawn/await, so
	// a nil TaskRegistry + default depth cap is fine here.
	exec := NewToolExecutor(cat, newTestArtifactStore(t), nil,
		WithHeavyThreshold(heavyThreshold)).(*toolExecutor)
	return exec, cat
}

// TestExecutor_CallParallel_MixedSuccessFailure — AC-13 + AC-2:
// a CallParallel with success / invoke-error / resolve-miss /
// bad-args branches produces one aggregate outcome per branch keyed by
// CallID; the bad-args + missing branches surface as errors while the
// valid branches still dispatch (non-atomic).
func TestExecutor_CallParallel_MixedSuccessFailure(t *testing.T) {
	t.Parallel()
	exec, cat := newParallelTestExecutor(t, 0)
	registerEcho(t, cat, "good")
	// boom — Invoke returns an error.
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "boom"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, errors.New("kaboom")
		},
	}); err != nil {
		t.Fatalf("register boom: %v", err)
	}
	// badargs — Validate rejects.
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "badargs"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: "should-not-run"}, nil
		},
		Validate: func(_ json.RawMessage) error { return errors.New("bad args") },
	}); err != nil {
		t.Fatalf("register badargs: %v", err)
	}

	q := dispatchTestQuad("r-mixed")
	rc := planner.RunContext{Quadruple: q}
	decision := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "good", Args: json.RawMessage(`{"x":1}`), CallID: "c0"},
			{Tool: "boom", Args: json.RawMessage(`{}`), CallID: "c1"},
			{Tool: "missing", Args: json.RawMessage(`{}`), CallID: "c2"},
			{Tool: "badargs", Args: json.RawMessage(`{}`), CallID: "c3"},
		},
	}
	rawAny, llmAny, err := exec.ExecuteDecision(dispatchTestCtx(t, q), rc, decision)
	if err != nil {
		t.Fatalf("ExecuteDecision: unexpected whole-call err: %v", err)
	}
	raw, ok := rawAny.(planner.ParallelObservation)
	if !ok {
		t.Fatalf("raw observation type = %T, want planner.ParallelObservation", rawAny)
	}
	llmObs, ok := llmAny.(planner.ParallelObservation)
	if !ok {
		t.Fatalf("llm observation type = %T, want planner.ParallelObservation", llmAny)
	}
	if len(raw.Branches) != 4 || len(llmObs.Branches) != 4 {
		t.Fatalf("branch counts raw=%d llm=%d, want 4/4 (one per branch)", len(raw.Branches), len(llmObs.Branches))
	}
	// branch-index order + CallID correlation.
	wantIDs := []string{"c0", "c1", "c2", "c3"}
	for i, b := range raw.Branches {
		if b.Index != i {
			t.Errorf("raw.Branches[%d].Index = %d, want %d", i, b.Index, i)
		}
		if b.CallID != wantIDs[i] {
			t.Errorf("raw.Branches[%d].CallID = %q, want %q", i, b.CallID, wantIDs[i])
		}
	}
	if raw.Branches[0].Error != "" || raw.Branches[0].Value == nil {
		t.Errorf("branch 0 (good) should succeed: %+v", raw.Branches[0])
	}
	if !strings.Contains(raw.Branches[1].Error, "kaboom") {
		t.Errorf("branch 1 (boom) error = %q, want it to carry the invoke error", raw.Branches[1].Error)
	}
	if raw.Branches[2].Error == "" {
		t.Errorf("branch 2 (missing) should be a resolve-miss error, got value %+v", raw.Branches[2])
	}
	if raw.Branches[3].Error == "" {
		t.Errorf("branch 3 (badargs) should be a validate error, got value %+v", raw.Branches[3])
	}
}

// TestExecutor_CallParallel_HeavyBranchesProjected — AC-3: a
// CallParallel with ≥2 heavy-output branches projects each branch to an
// artifact-stub summary in the llmObservation independently; the raw
// aggregate keeps the untruncated values. The llm aggregate stays well
// under the heavy threshold.
func TestExecutor_CallParallel_HeavyBranchesProjected(t *testing.T) {
	t.Parallel()
	const threshold = 256
	exec, cat := newParallelTestExecutor(t, threshold)
	// heavy tool — returns a value whose JSON encoding exceeds threshold.
	bigField := strings.Repeat("y", threshold*4)
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "heavy"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"title": "T", "blob": bigField}}, nil
		},
	}); err != nil {
		t.Fatalf("register heavy: %v", err)
	}

	q := dispatchTestQuad("r-heavy")
	rc := planner.RunContext{Quadruple: q}
	decision := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "heavy", Args: json.RawMessage(`{}`), CallID: "h0"},
			{Tool: "heavy", Args: json.RawMessage(`{}`), CallID: "h1"},
		},
	}
	rawAny, llmAny, err := exec.ExecuteDecision(dispatchTestCtx(t, q), rc, decision)
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}
	raw := rawAny.(planner.ParallelObservation)
	llmObs := llmAny.(planner.ParallelObservation)

	// Raw aggregate carries the untruncated values (the big blob).
	rawEncoded, _ := json.Marshal(raw)
	if len(rawEncoded) < threshold {
		t.Errorf("raw aggregate is suspiciously small (%d bytes) — untruncated values expected", len(rawEncoded))
	}

	// LLM aggregate: each branch projected to a truncation summary that
	// carries the artifact_ref + truncated marker, and the whole
	// aggregate stays small.
	for i, b := range llmObs.Branches {
		m, ok := b.Value.(map[string]any)
		if !ok {
			t.Fatalf("llm branch[%d] value type = %T, want truncation-summary map", i, b.Value)
		}
		if m["truncated"] != true {
			t.Errorf("llm branch[%d] not marked truncated: %#v", i, m)
		}
		if _, hasRef := m["artifact_ref"]; !hasRef {
			t.Errorf("llm branch[%d] missing artifact_ref: %#v", i, m)
		}
	}
	llmEncoded, _ := json.Marshal(llmObs)
	if len(llmEncoded) >= len(rawEncoded) {
		t.Errorf("llm aggregate (%d) should be smaller than raw (%d) after projection", len(llmEncoded), len(rawEncoded))
	}
}

// TestExecutor_CallParallel_ConcurrentReuse — AC-17 / D-025:
// N≥100 concurrent ExecuteDecision(CallParallel) calls against ONE
// shared executor (and its one shared parallel.Executor), each
// with its own identity quadruple + branch set, under -race. Asserts no
// cross-talk (each run's branches carry its own run id), no data race,
// and that the goroutine count returns to baseline after all runs
// settle.
func TestExecutor_CallParallel_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	exec, cat := newParallelTestExecutor(t, 0)
	// A tool that echoes the run id it observed via ctx identity, so we
	// can assert no cross-run bleed.
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "whoami"},
		Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			q, ok := identity.QuadrupleFrom(ctx)
			if !ok {
				return tools.ToolResult{}, errors.New("missing identity in branch ctx")
			}
			return tools.ToolResult{Value: map[string]any{"run": q.RunID}}, nil
		},
	}); err != nil {
		t.Fatalf("register whoami: %v", err)
	}
	registerEcho(t, cat, "side")

	baseline := runtime.NumGoroutine()

	const n = 128
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runID := fmt.Sprintf("r-reuse-%d", idx)
			q := dispatchTestQuad(runID)
			rc := planner.RunContext{Quadruple: q}
			decision := planner.CallParallel{
				Branches: []planner.CallTool{
					{Tool: "whoami", Args: json.RawMessage(`{}`), CallID: fmt.Sprintf("%d-a", idx)},
					{Tool: "side", Args: json.RawMessage(`{}`), CallID: fmt.Sprintf("%d-b", idx)},
				},
			}
			rawAny, _, err := exec.ExecuteDecision(dispatchTestCtx(t, q), rc, decision)
			if err != nil {
				errCh <- fmt.Errorf("run %d: %w", idx, err)
				return
			}
			raw := rawAny.(planner.ParallelObservation)
			if len(raw.Branches) != 2 {
				errCh <- fmt.Errorf("run %d: branches = %d, want 2", idx, len(raw.Branches))
				return
			}
			// whoami branch must report THIS run's id — no cross-talk.
			m, ok := raw.Branches[0].Value.(map[string]any)
			if !ok || m["run"] != runID {
				errCh <- fmt.Errorf("run %d: cross-talk — whoami saw %v, want %q", idx, raw.Branches[0].Value, runID)
				return
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
