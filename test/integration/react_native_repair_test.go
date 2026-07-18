package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// TestE2E_ReactNativeStructuralRepair proves the F1 fix end-to-end across
// the planner→runtime seam: when a REAL react.ReActPlanner rejects a
// native tool-call response as structurally invalid (the projector wraps
// planner.ErrInvalidDecision), the steering.RunLoop feeds that rejection
// back as a step observation instead of aborting the run, and the planner
// re-plans on the next step and finishes. This is the exact live-glm-5.2
// failure class ("_spawn_task args malformed JSON: unexpected end of JSON
// input", a retain-turn spawn in a batch, a _finish co-occurring with
// other calls) that previously killed the whole run.
//
// Lives in test/integration/ because it wires the react planner across the
// internal/runtime/steering RunLoop seam (the §13 import-graph lint
// forbids internal/runtime imports inside the planner subtree, including
// its tests). Runs under -race.
func TestE2E_ReactNativeStructuralRepair(t *testing.T) {
	t.Parallel()

	// Each case: a structurally-invalid first response, then a clean
	// terminal _finish. The run must SURVIVE (Finish{Goal}) and the LLM
	// must be re-prompted exactly once with the violation visible.
	cases := []struct {
		name      string
		bad       llm.CompleteResponse
		violation string // substring the fed-back observation must carry
		runSuffix string
	}{
		{
			name: "malformed_spawn_args_json",
			bad: oneToolCall("call_bad", react.SpawnTaskToolName,
				// Truncated args — "unexpected end of JSON input", the exact
				// live-glm-5.2 shape.
				`{"kind":"background","spec":{"description":"summarise`),
			violation: "malformed JSON",
			runSuffix: "malformed",
		},
		{
			name: "retain_turn_spawn_in_batch",
			bad: manyToolCalls(
				llm.ToolCallStructured{ID: "call_s", Name: react.SpawnTaskToolName,
					Args: json.RawMessage(`{"kind":"background","spec":{"description":"d","query":"q","retain_turn":true}}`)},
				llm.ToolCallStructured{ID: "call_t", Name: "alpha", Args: json.RawMessage(`{}`)},
			),
			violation: "RetainTurn=true",
			runSuffix: "retainturn",
		},
		{
			name: "finish_co_occurrence",
			bad: manyToolCalls(
				llm.ToolCallStructured{ID: "call_f", Name: react.FinishToolName, Args: json.RawMessage(`{"answer":"early"}`)},
				llm.ToolCallStructured{ID: "call_t", Name: "alpha", Args: json.RawMessage(`{}`)},
			),
			violation: "standalone",
			runSuffix: "cooccur",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := newPhase53Deps(t)
			defer deps.cleanup()

			q := reactRepairRun(tc.runSuffix)
			ctx := ctxFor(t, q)

			client := &recordingScriptedLLM{responses: []llm.CompleteResponse{
				tc.bad,
				oneToolCall("call_ok", react.FinishToolName, `{"answer":"done"}`),
			}}
			p := react.New(client)
			tr := &planner.Trajectory{}

			fin, err := deps.runLoop.Run(ctx, steering.RunSpec{
				Planner:  p,
				Base:     planner.RunContext{Quadruple: q, Goal: "reach the goal", Trajectory: tr},
				MaxSteps: 16,
			})
			if err != nil {
				t.Fatalf("Run: %v — the structural rejection must be repaired, not fatal", err)
			}
			if fin.Reason != planner.FinishGoal {
				t.Fatalf("Finish.Reason = %q, want goal (the re-plan step's finish)", fin.Reason)
			}
			// The LLM was re-prompted exactly once: the bad response and the
			// recovered response.
			if n := client.calls(); n != 2 {
				t.Fatalf("LLM Complete calls = %d, want 2 (bad + recovered)", n)
			}
			// The rejection was fed back into the trajectory so the re-plan
			// turn's prompt surfaces the exact violation.
			if len(tr.Steps) != 1 {
				t.Fatalf("trajectory steps = %d, want 1 (the rejection observation)", len(tr.Steps))
			}
			if !strings.Contains(tr.Steps[0].Error, tc.violation) {
				t.Errorf("rejection observation = %q, want it to name %q", tr.Steps[0].Error, tc.violation)
			}
			// The re-prompt (2nd Complete) carried the violation as a rendered
			// observation message so the model could correct itself.
			second := client.requestAt(1)
			if !messagesContain(second, "Observation (error)") {
				t.Errorf("re-prompt request did not surface the fed-back rejection observation to the model")
			}
		})
	}
}

// TestE2E_ReactNativeStructuralRepair_BudgetBounded asserts the bound: a
// model that emits a malformed _spawn_task on EVERY response terminates
// the run loudly after the consecutive-failure budget rather than looping.
func TestE2E_ReactNativeStructuralRepair_BudgetBounded(t *testing.T) {
	t.Parallel()
	deps := newPhase53Deps(t)
	defer deps.cleanup()

	q := reactRepairRun("bounded")
	ctx := ctxFor(t, q)

	badSpawn := oneToolCall("call_bad", react.SpawnTaskToolName,
		`{"kind":"background","spec":{"description":"summarise`)
	client := &recordingScriptedLLM{responses: []llm.CompleteResponse{
		badSpawn, badSpawn, badSpawn, badSpawn, badSpawn,
	}}
	p := react.New(client)

	_, err := deps.runLoop.Run(ctx, steering.RunSpec{
		Planner:  p,
		Base:     planner.RunContext{Quadruple: q, Goal: "reach the goal", Trajectory: &planner.Trajectory{}},
		MaxSteps: 16,
	})
	if err == nil {
		t.Fatal("Run: a persistently-malformed model must terminate loud, got nil")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Fatalf("Run err = %v, want errors.Is planner.ErrInvalidDecision", err)
	}
	if !strings.Contains(err.Error(), "repair budget exhausted") {
		t.Errorf("Run err = %q, want it to name the budget exhaustion", err.Error())
	}
	// Default budget is 2 (>= boundary): terminates on the 2nd consecutive
	// rejection — never a runaway to MaxSteps.
	if n := client.calls(); n != 2 {
		t.Fatalf("LLM Complete calls = %d, want 2 (bounded, not MaxSteps)", n)
	}
}

// TestE2E_ReactNativeStructuralRepair_CleanNoRepair asserts a clean native
// response is NOT touched: the run finishes on the first turn with exactly
// one LLM call — no spurious repair, no regression.
func TestE2E_ReactNativeStructuralRepair_CleanNoRepair(t *testing.T) {
	t.Parallel()
	deps := newPhase53Deps(t)
	defer deps.cleanup()

	q := reactRepairRun("clean")
	ctx := ctxFor(t, q)

	client := &recordingScriptedLLM{responses: []llm.CompleteResponse{
		oneToolCall("call_ok", react.FinishToolName, `{"answer":"straight to the point"}`),
	}}
	p := react.New(client)
	tr := &planner.Trajectory{}

	fin, err := deps.runLoop.Run(ctx, steering.RunSpec{
		Planner:  p,
		Base:     planner.RunContext{Quadruple: q, Goal: "reach the goal", Trajectory: tr},
		MaxSteps: 16,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %q, want goal", fin.Reason)
	}
	if n := client.calls(); n != 1 {
		t.Fatalf("LLM Complete calls = %d, want 1 (no spurious repair)", n)
	}
	if len(tr.Steps) != 0 {
		t.Errorf("trajectory steps = %d, want 0 (a clean finish appends no rejection)", len(tr.Steps))
	}
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

func reactRepairRun(suffix string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "tenant-nr",
			UserID:    "user-nr",
			SessionID: "session-nr-" + suffix,
		},
		RunID: "run-nr-" + suffix,
	}
}

func oneToolCall(id, name, argsJSON string) llm.CompleteResponse {
	return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{
		ID: id, Name: name, Args: json.RawMessage(argsJSON),
	}}}
}

func manyToolCalls(calls ...llm.ToolCallStructured) llm.CompleteResponse {
	return llm.CompleteResponse{ToolCalls: append([]llm.ToolCallStructured(nil), calls...)}
}

// recordingScriptedLLM returns scripted responses per Complete call and
// records every request so a test can assert the fed-back observation
// reached the model on the re-prompt.
type recordingScriptedLLM struct {
	mu        sync.Mutex
	responses []llm.CompleteResponse
	cursor    int
	requests  []llm.CompleteRequest
	callCount int
}

func (c *recordingScriptedLLM) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callCount++
	c.requests = append(c.requests, req)
	var resp llm.CompleteResponse
	if c.cursor < len(c.responses) {
		resp = c.responses[c.cursor]
		c.cursor++
	} else if len(c.responses) > 0 {
		resp = c.responses[len(c.responses)-1]
	}
	return resp, nil
}

func (c *recordingScriptedLLM) Close(context.Context) error { return nil }

func (c *recordingScriptedLLM) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callCount
}

func (c *recordingScriptedLLM) requestAt(i int) llm.CompleteRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < 0 || i >= len(c.requests) {
		return llm.CompleteRequest{}
	}
	return c.requests[i]
}

// messagesContain reports whether any message's text content in the
// request carries the substring.
func messagesContain(req llm.CompleteRequest, sub string) bool {
	for _, m := range req.Messages {
		if m.Content.Text != nil && strings.Contains(*m.Content.Text, sub) {
			return true
		}
	}
	return false
}
