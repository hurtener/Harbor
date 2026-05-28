package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestReactPlanner_NativeToolCall_ParallelCalls — Phase 107d AC-16 (the
// placeholder the 107c test plan named). Lives in test/integration/
// (not internal/planner/react/) because it wires a REAL
// internal/runtime/parallel.Executor across the planner→runtime seam,
// which the §13 import-graph lint forbids inside the planner subtree
// (the lint gates test files too). Per CLAUDE.md §17.2 a test that spans
// >2 subsystems (planner + runtime/parallel + tools) belongs here.
//
// A scripted LLM emits N>1 ToolCalls in one response; the React planner
// (default: parallel ON) projects a CallParallel; the real executor
// dispatches every branch with identity propagating to each branch
// ctx; one branch fails (the ≥1-failure-mode requirement); the
// resulting aggregate observation feeds the next turn's prompt, which
// carries N RoleTool messages, one per branch. Runs under -race.
func TestReactPlanner_NativeToolCall_ParallelCalls(t *testing.T) {
	t.Parallel()

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t-par", UserID: "u-par", SessionID: "s-par"},
		RunID:    "r-par-int",
	}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Real dispatch catalog (satisfies parallel.Resolver). Each tool
	// asserts the run's identity propagated into the branch ctx.
	realCat := tools.NewCatalog()
	mkIdentityEcho := func(name string) func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		return func(bctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			bq, ok := identity.QuadrupleFrom(bctx)
			if !ok {
				return tools.ToolResult{}, errors.New("branch ctx missing identity")
			}
			return tools.ToolResult{Value: map[string]any{"tool": name, "run": bq.RunID}}, nil
		}
	}
	for _, n := range []string{"alpha", "beta"} {
		if regErr := realCat.Register(tools.ToolDescriptor{
			Tool:   tools.Tool{Name: n},
			Invoke: mkIdentityEcho(n),
		}); regErr != nil {
			t.Fatalf("register %q: %v", n, regErr)
		}
	}
	if regErr := realCat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "boom"},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, errors.New("branch-boom")
		},
	}); regErr != nil {
		t.Fatalf("register boom: %v", regErr)
	}
	exec := parallel.New(realCat)

	plannerCat := &reactParallelCatalogView{
		tools: []tools.Tool{
			{Name: "alpha", Description: "a", Loading: tools.LoadingAlways},
			{Name: "beta", Description: "b", Loading: tools.LoadingAlways},
			{Name: "boom", Description: "c", Loading: tools.LoadingAlways},
		},
	}

	client := &reactParallelScriptedLLM{
		responses: []llm.CompleteResponse{
			{ToolCalls: []llm.ToolCallStructured{
				{ID: "call_a", Name: "alpha", Args: json.RawMessage(`{"x":1}`)},
				{ID: "call_b", Name: "beta", Args: json.RawMessage(`{"x":2}`)},
				{ID: "call_boom", Name: "boom", Args: json.RawMessage(`{}`)},
			}},
			{Content: "all done"},
		},
	}

	p := react.New(client) // default: parallel ON
	traj := &planner.Trajectory{}

	// --- Turn 1: planner emits CallParallel ---
	rc1 := planner.RunContext{Quadruple: q, Goal: "fan out", Trajectory: traj, Catalog: plannerCat}
	dec1, err := p.Next(ctx, rc1)
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	par, ok := dec1.(planner.CallParallel)
	if !ok {
		t.Fatalf("Next #1 = %T, want CallParallel", dec1)
	}
	if len(par.Branches) != 3 {
		t.Fatalf("CallParallel branches = %d, want 3", len(par.Branches))
	}

	// --- Dispatch through the REAL executor (non-atomic, native path) ---
	results, err := exec.Execute(ctx, par, parallel.WithNonAtomicSetup())
	if err != nil {
		t.Fatalf("executor.Execute: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3 (one per branch)", len(results))
	}
	agg := planner.ParallelObservation{}
	var sawBoomErr bool
	for _, r := range results {
		callID := par.Branches[r.Index].CallID
		if r.Err != nil {
			if r.Tool == "boom" {
				sawBoomErr = true
			}
			agg.Branches = append(agg.Branches, planner.ParallelBranchObservation{
				CallID: callID, Tool: r.Tool, Index: r.Index, Error: r.Err.Error(),
			})
			continue
		}
		m, _ := r.Result.Value.(map[string]any)
		if m["run"] != q.RunID {
			t.Errorf("branch %q saw run %v, want %q (identity propagation)", r.Tool, m["run"], q.RunID)
		}
		agg.Branches = append(agg.Branches, planner.ParallelBranchObservation{
			CallID: callID, Tool: r.Tool, Index: r.Index, Value: r.Result.Value,
		})
	}
	if !sawBoomErr {
		t.Errorf("expected the boom branch to surface an error (≥1 failure-mode)")
	}

	traj.Steps = append(traj.Steps, planner.Step{
		Action:         dec1,
		Observation:    agg,
		LLMObservation: agg,
	})

	// --- Turn 2: the next prompt must carry N RoleTool messages ---
	rc2 := planner.RunContext{Quadruple: q, Goal: "fan out", Trajectory: traj, Catalog: plannerCat}
	dec2, err := p.Next(ctx, rc2)
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	if _, ok := dec2.(planner.Finish); !ok {
		t.Fatalf("Next #2 = %T, want Finish", dec2)
	}

	reqs := client.snapshotRequests()
	if len(reqs) < 2 {
		t.Fatalf("captured %d requests, want ≥2", len(reqs))
	}
	turn2 := reqs[1]
	var pending []string
	toolAnswers := 0
	for i, m := range turn2.Messages {
		if m.Role == llm.RoleAssistant && len(m.ToolCalls) > 0 {
			if len(pending) != 0 {
				t.Fatalf("messages[%d]: assistant ToolCalls turn with %d unanswered IDs %v", i, len(pending), pending)
			}
			for _, tc := range m.ToolCalls {
				pending = append(pending, tc.ID)
			}
		}
		if m.Role == llm.RoleTool {
			toolAnswers++
			if m.ToolCallID == nil {
				t.Fatalf("messages[%d]: RoleTool missing ToolCallID", i)
			}
			found := false
			for j, pid := range pending {
				if pid == *m.ToolCallID {
					pending = append(pending[:j], pending[j+1:]...)
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("messages[%d]: RoleTool ToolCallID=%q matched no pending (pending=%v)", i, *m.ToolCallID, pending)
			}
		}
	}
	if toolAnswers != 3 {
		t.Errorf("turn 2 carried %d RoleTool messages, want 3 (one per branch)", toolAnswers)
	}
	if len(pending) != 0 {
		t.Errorf("turn 2 left orphan assistant tool_calls: %v", pending)
	}
}

// reactParallelCatalogView is a minimal planner.ToolCatalogView.
type reactParallelCatalogView struct {
	tools []tools.Tool
}

func (c *reactParallelCatalogView) Resolve(name string) (tools.Tool, bool) {
	for _, t := range c.tools {
		if t.Name == name {
			return t, true
		}
	}
	return tools.Tool{}, false
}

func (c *reactParallelCatalogView) List() []tools.Tool {
	out := make([]tools.Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// reactParallelScriptedLLM is a scripted llm.LLMClient recording each
// request so the test can assert the turn-2 RoleTool round-trip.
type reactParallelScriptedLLM struct {
	mu        sync.Mutex
	responses []llm.CompleteResponse
	cursor    int
	requests  []llm.CompleteRequest
}

func (c *reactParallelScriptedLLM) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, req)
	if c.cursor >= len(c.responses) {
		if len(c.responses) == 0 {
			return llm.CompleteResponse{}, nil
		}
		return c.responses[len(c.responses)-1], nil
	}
	out := c.responses[c.cursor]
	c.cursor++
	return out, nil
}

func (c *reactParallelScriptedLLM) Close(_ context.Context) error { return nil }

func (c *reactParallelScriptedLLM) snapshotRequests() []llm.CompleteRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]llm.CompleteRequest, len(c.requests))
	copy(out, c.requests)
	return out
}
