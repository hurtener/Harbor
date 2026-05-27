package react_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

// scriptedClient is a programmable llm.LLMClient for the react tests.
// Each Complete call returns the next scripted response; once the
// script is exhausted the last response repeats forever (so a
// runaway-loop bug surfaces as Reason=NoPath rather than a panic).
//
// Concurrent-safe: a mutex serialises the cursor. The D-025 test uses
// a different per-goroutine stub to isolate per-call streams; this
// shared client is the single-run / unit-test fixture.
type scriptedClient struct {
	mu        sync.Mutex
	responses []llm.CompleteResponse
	errs      []error
	cursor    int
	calls     atomic.Int64
	seenIDs   []identity.Quadruple
}

func (s *scriptedClient) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := identity.QuadrupleFrom(ctx)
	s.seenIDs = append(s.seenIDs, id)
	if s.cursor >= len(s.responses) {
		idx := len(s.responses) - 1
		var resp llm.CompleteResponse
		var err error
		if idx >= 0 {
			resp = s.responses[idx]
		}
		if idx >= 0 && idx < len(s.errs) {
			err = s.errs[idx]
		}
		return resp, err
	}
	resp := s.responses[s.cursor]
	var err error
	if s.cursor < len(s.errs) {
		err = s.errs[s.cursor]
	}
	s.cursor++
	return resp, err
}

func (s *scriptedClient) Close(_ context.Context) error { return nil }

func (s *scriptedClient) callCount() int64 { return s.calls.Load() }

// fixedQuadruple returns a populated identity quadruple for tests.
func fixedQuadruple(t *testing.T, runID string) identity.Quadruple {
	t.Helper()
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		RunID:    runID,
	}
}

// ctxWith installs the identity quadruple in ctx (matches the
// production wiring where the runtime engine calls identity.WithRun
// before invoking Planner.Next).
func ctxWith(t *testing.T, q identity.Quadruple) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// recordingEmit collects events into a slice (mutex-guarded).
type recordingEmit struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recordingEmit) emit(ev events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recordingEmit) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Event, len(r.events))
	copy(out, r.events)
	return out
}

// rcWith builds a planner.RunContext with the given identity, goal,
// and Emit closure. Trajectory and other fields default to nil/zero.
func rcWith(q identity.Quadruple, goal string, emit func(events.Event)) planner.RunContext {
	return planner.RunContext{
		Quadruple: q,
		Goal:      goal,
		Emit:      emit,
	}
}

// nativeToolCallResp shapes a native-tool-calling mock LLM response
// carrying a single ToolCall (Phase 107c — D-167). Helper to keep
// per-test fixtures compact after the cutover.
func nativeToolCallResp(id, name, argsJSON string) llm.CompleteResponse {
	return llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   id,
			Name: name,
			Args: json.RawMessage(argsJSON),
		}},
	}
}

// finishToolCallResp shapes a native-tool-calling mock LLM response
// carrying the reserved `_finish` tool call. The projector translates
// this to planner.Finish{Goal} with the answer in Payload.
func finishToolCallResp(id, answer string) llm.CompleteResponse {
	return nativeToolCallResp(id, "_finish", fmt.Sprintf(`{"answer":%q}`, answer))
}

// multiToolCallResp shapes a native-tool-calling mock LLM response
// carrying N ToolCalls (AC-19 serialization fallback path: the
// projector emits the first as CallTool and queues the rest on
// rc.PendingToolCalls).
func multiToolCallResp(calls ...llm.ToolCallStructured) llm.CompleteResponse {
	return llm.CompleteResponse{ToolCalls: append([]llm.ToolCallStructured(nil), calls...)}
}

// TestNew_AppliesDefaults asserts the zero-options constructor sets
// every documented default.
func TestNew_AppliesDefaults(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			finishToolCallResp("call_1", "ok"),
		},
	}
	p := react.New(client)
	if p == nil {
		t.Fatal("New returned nil")
	}
	if p.WakeMode() != planner.WakePush {
		t.Errorf("WakeMode = %q, want %q", p.WakeMode(), planner.WakePush)
	}
	// Invoke once to confirm the default behaviour produces a Finish
	// on a clean LLM response.
	q := fixedQuadruple(t, "r-defaults")
	rc := rcWith(q, "complete me", (&recordingEmit{}).emit)
	dec, err := p.Next(ctxWith(t, q), rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
}

// TestNew_PanicsOnNilClient asserts the constructor fails closed.
func TestNew_PanicsOnNilClient(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(nil) did not panic")
		}
	}()
	_ = react.New(nil)
}

// TestNext_RejectsMissingIdentity asserts the planner refuses a
// RunContext without a full identity quadruple. Wrapped sentinel for
// errors.Is.
func TestNext_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{}
	p := react.New(client)
	rc := planner.RunContext{
		// No Quadruple set — partial identity.
		Goal: "anything",
	}
	_, err := p.Next(t.Context(), rc)
	if err == nil {
		t.Fatal("Next returned nil error for missing identity")
	}
	if !errors.Is(err, llm.ErrIdentityMissing) {
		t.Errorf("err = %v, want errors.Is llm.ErrIdentityMissing", err)
	}
	if client.callCount() != 0 {
		t.Errorf("client.calls = %d, want 0 (planner must reject BEFORE LLM call)", client.callCount())
	}
}

// TestNext_HonoursCtxCancel asserts a pre-cancelled ctx returns
// ctx.Err() before any LLM call.
func TestNext_HonoursCtxCancel(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			finishToolCallResp("call_1", ""),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-cancel")
	ctx, cancel := context.WithCancel(ctxWith(t, q))
	cancel()
	_, err := p.Next(ctx, rcWith(q, "g", nil))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if client.callCount() != 0 {
		t.Errorf("client.calls = %d, want 0 (cancelled ctx must not burn LLM)", client.callCount())
	}
}

// TestNext_ObservesSteeringCancellation asserts the planner returns
// Finish{Cancelled} when rc.Control.Cancelled is true.
func TestNext_ObservesSteeringCancellation(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{}
	p := react.New(client)
	q := fixedQuadruple(t, "r-steering")
	rc := rcWith(q, "g", nil)
	rc.Control = planner.ControlSignals{Cancelled: true}
	dec, err := p.Next(ctxWith(t, q), rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishCancelled {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishCancelled)
	}
	if client.callCount() != 0 {
		t.Errorf("client.calls = %d, want 0 (CANCEL steering must short-circuit)", client.callCount())
	}
}

// TestNext_FinishToolNameMappedToFinishDecision asserts the
// `_finish` reserved tool name is intercepted at decision-mapping
// time and translated to Finish{FinishGoal} — NEVER returned as a
// CallTool.
func TestNext_FinishToolNameMappedToFinishDecision(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			finishToolCallResp("call_1", "42"),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-finish")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "find the answer", nil))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
	if fin.Payload != "42" {
		t.Errorf("Payload = %v, want %q", fin.Payload, "42")
	}
	// Phase 83e (D-147): the action schema is narrowed to {tool, args};
	// translateFinishCall no longer stamps Metadata["reasoning"]. The
	// `reasoning` field in the fixture above is stripped at parse time.
	if _, present := fin.Metadata["reasoning"]; present {
		t.Errorf("Metadata[reasoning] should be absent post-D-147, got %v", fin.Metadata["reasoning"])
	}
	// Phase 107c (D-167): the projector path stamps `via` to
	// `react.projectResponse._finish` (translateNativeFinish).
	if v, _ := fin.Metadata["via"].(string); v != "react.projectResponse._finish" {
		t.Errorf("Metadata[via] = %v, want react.projectResponse._finish", v)
	}
}

// TestNext_MultiToolCallSerializesViaPending asserts the Phase 107c
// (AC-19) serialization fallback: when the LLM emits N>1 native
// ToolCalls in one response, the planner emits the FIRST as
// planner.CallTool and queues the rest on rc.PendingToolCalls for
// subsequent steps to drain. The runtime's CallParallel dispatcher is
// post-V1.3; serialization is the V1.3 contract until it lands.
//
// This test supersedes the prior CallParallel pass-through (Phase 47
// / D-056) — the prompt-engineered JSON array path retired with
// Phase 107c.
func TestNext_MultiToolCallSerializesViaPending(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			multiToolCallResp(
				llm.ToolCallStructured{ID: "call_a", Name: "alpha", Args: json.RawMessage(`{"x":1}`)},
				llm.ToolCallStructured{ID: "call_b", Name: "beta", Args: json.RawMessage(`{"y":2}`)},
				llm.ToolCallStructured{ID: "call_c", Name: "gamma", Args: json.RawMessage(`{"z":3}`)},
			),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-par")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	call, ok := dec.(planner.CallTool)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallTool (AC-19 serialization fallback)", dec)
	}
	if call.Tool != "alpha" || call.CallID != "call_a" {
		t.Errorf("first decision = %+v, want CallTool{alpha, call_a}", call)
	}
	// The rc.PendingToolCalls mutation persists within the planner's
	// per-step rc copy but is not observable from this test's outer
	// scope (the rc passed to Next() is a value copy). The runtime
	// surface for cross-step persistence is a future enhancement; this
	// test exercises the projection contract.
}

// TestNext_ParallelWithFinishFirstStillFinishes asserts the special
// case: if the first native ToolCall is `_finish`, the planner
// converts it to a Finish Decision and drops the trailing calls.
// Phase 107c (D-167) preserves this special case under the projector.
func TestNext_ParallelWithFinishFirstStillFinishes(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			multiToolCallResp(
				llm.ToolCallStructured{ID: "call_f", Name: "_finish", Args: json.RawMessage(`{"answer":"early"}`)},
				llm.ToolCallStructured{ID: "call_d", Name: "discarded", Args: json.RawMessage(`{}`)},
			),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-par-finish")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish (first ToolCall _finish should still finish)", dec)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
	if fin.Payload != "early" {
		t.Errorf("Payload = %v, want %q", fin.Payload, "early")
	}
}

// TestNext_MaxStepsCircuitBreakerEmitsAndFinishes asserts the
// planner-side breaker:
//
//   - Fires when len(rc.Trajectory.Steps) >= MaxSteps.
//   - Emits planner.max_steps_exceeded BEFORE returning.
//   - Returns Finish{NoPath, Metadata["max_steps_exceeded"]=true}.
//   - Does NOT burn an LLM call.
func TestNext_MaxStepsCircuitBreakerEmitsAndFinishes(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			finishToolCallResp("call_1", ""), // never reached
		},
	}
	rec := &recordingEmit{}
	p := react.New(client, react.WithMaxSteps(2))
	q := fixedQuadruple(t, "r-maxsteps")

	// Build a trajectory with two prior CallTool steps so the
	// breaker fires.
	traj := &planner.Trajectory{
		Steps: []planner.Step{
			{Action: planner.CallTool{Tool: "alpha", Args: json.RawMessage(`{}`)}},
			{Action: planner.CallTool{Tool: "beta", Args: json.RawMessage(`{}`)}},
		},
	}
	rc := rcWith(q, "g", rec.emit)
	rc.Trajectory = traj

	dec, err := p.Next(ctxWith(t, q), rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if got, _ := fin.Metadata["max_steps_exceeded"].(bool); !got {
		t.Errorf("Metadata[max_steps_exceeded] not true: %+v", fin.Metadata)
	}
	if v, _ := fin.Metadata["last_tool"].(string); v != "beta" {
		t.Errorf("Metadata[last_tool] = %v, want %q", v, "beta")
	}
	if client.callCount() != 0 {
		t.Errorf("client.calls = %d, want 0 (breaker must fire BEFORE LLM call)", client.callCount())
	}

	// Event observation: planner.max_steps_exceeded with the correct
	// identity + payload.
	emitted := rec.snapshot()
	if len(emitted) != 1 {
		t.Fatalf("emitted %d events, want 1: %+v", len(emitted), emitted)
	}
	ev := emitted[0]
	if ev.Type != planner.EventTypePlannerMaxStepsExceeded {
		t.Errorf("ev.Type = %q, want %q", ev.Type, planner.EventTypePlannerMaxStepsExceeded)
	}
	if ev.Identity != q {
		t.Errorf("ev.Identity = %+v, want %+v", ev.Identity, q)
	}
	payload, ok := ev.Payload.(planner.MaxStepsExceededPayload)
	if !ok {
		t.Fatalf("ev.Payload = %T, want MaxStepsExceededPayload", ev.Payload)
	}
	if payload.MaxSteps != 2 {
		t.Errorf("payload.MaxSteps = %d, want 2", payload.MaxSteps)
	}
	if payload.StepsObserved != 2 {
		t.Errorf("payload.StepsObserved = %d, want 2", payload.StepsObserved)
	}
	if payload.LastTool != "beta" {
		t.Errorf("payload.LastTool = %q, want %q", payload.LastTool, "beta")
	}
}

// TestNext_MaxStepsBreakerWithoutEmitClosure asserts the planner
// still returns the Finish Decision when rc.Emit is nil (the
// observability surface is absent but the contract still holds — no
// panic, no silent silent-degradation).
func TestNext_MaxStepsBreakerWithoutEmitClosure(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{}
	p := react.New(client, react.WithMaxSteps(1))
	q := fixedQuadruple(t, "r-no-emit")
	rc := rcWith(q, "g", nil) // no Emit
	rc.Trajectory = &planner.Trajectory{
		Steps: []planner.Step{
			{Action: planner.CallTool{Tool: "alpha"}},
		},
	}
	dec, err := p.Next(ctxWith(t, q), rc)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if fin, ok := dec.(planner.Finish); !ok || fin.Reason != planner.FinishNoPath {
		t.Fatalf("decision = %+v, want Finish{NoPath}", dec)
	}
}

// TestNext_EmptyResponseMapsToFinishNoPath asserts the Phase 107c
// (D-167) projector contract for an LLM response with neither
// ToolCalls nor Content: the planner emits Finish{NoPath} with
// Metadata[followup]=true so the runtime / UX can ask the user for
// retry / clarification.
//
// This test supersedes the prior Phase 44 repair-exhaustion path
// (`TestNext_RepairExhaustionPropagatesFinish`) — under native
// tool-calling the salvage/repair ladder is bypassed; the projector
// reads `resp.ToolCalls` directly. The repair loop is retained for
// the `declarative_action` escape-hatch (step 10), with its own test
// coverage there.
func TestNext_EmptyResponseMapsToFinishNoPath(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			{}, // both Content and ToolCalls empty
		},
	}
	rec := &recordingEmit{}
	p := react.New(client)
	q := fixedQuadruple(t, "r-empty-resp")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", rec.emit))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish (projector empty-empty path)", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if got, _ := fin.Metadata["followup"].(bool); !got {
		t.Errorf("Metadata[followup] not true — projector NoPath contract surface")
	}

	// Native path does NOT emit planner.repair_exhausted (the repair
	// loop is bypassed). It DOES emit planner.decision (the
	// observability surface for the resolved Decision). max_steps must
	// also be absent.
	emitted := rec.snapshot()
	var sawRepair, sawMax, sawDecision bool
	for _, ev := range emitted {
		switch ev.Type {
		case planner.EventTypePlannerRepairExhausted:
			sawRepair = true
		case planner.EventTypePlannerMaxStepsExceeded:
			sawMax = true
		case planner.EventTypePlannerDecision:
			sawDecision = true
		}
	}
	if sawRepair {
		t.Errorf("planner.repair_exhausted should NOT fire on the native path (repair loop is bypassed)")
	}
	if sawMax {
		t.Errorf("planner.max_steps_exceeded should NOT fire when MaxSteps is not hit")
	}
	if !sawDecision {
		t.Errorf("planner.decision should fire for the resolved Finish{NoPath}")
	}
}

// TestReact_ThreeStepScenario is the load-bearing acceptance
// criterion from Phase 45's master-plan detail block: "3-step
// reasoning task succeeds against a mock LLM." Phase 107c (D-167)
// rewrites the mock LLM to emit native ToolCalls.
//
// The scripted mock LLM emits:
//
//	call 1: ToolCalls: [{Name: "search", Args: {"q":"foo"}}]
//	call 2: ToolCalls: [{Name: "summarize", Args: {"text":"bar"}}]
//	call 3: ToolCalls: [{Name: "_finish", Args: {"answer":"done"}}]
//
// The test issues three successive Next calls. After each non-
// terminal Next call the test appends a synthetic Trajectory.Step to
// the RunContext so the next prompt sees the prior step (matching the
// runtime engine's behaviour).
//
// Asserts the three Decisions and the LLM call count.
func TestReact_ThreeStepScenario(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			nativeToolCallResp("call_1", "search", `{"q":"foo"}`),
			nativeToolCallResp("call_2", "summarize", `{"text":"bar"}`),
			finishToolCallResp("call_3", "done"),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-three-step")
	ctx := ctxWith(t, q)

	// Shared trajectory — the test appends a synthetic step between
	// Next calls to simulate the runtime executor's behaviour.
	traj := &planner.Trajectory{Steps: nil}

	// --- Step 1 ---
	rc1 := rcWith(q, "find and summarise foo", nil)
	rc1.Trajectory = traj
	dec1, err := p.Next(ctx, rc1)
	if err != nil {
		t.Fatalf("Next #1: %v", err)
	}
	call1, ok := dec1.(planner.CallTool)
	if !ok {
		t.Fatalf("decision #1 = %T, want planner.CallTool", dec1)
	}
	if call1.Tool != "search" {
		t.Errorf("Tool #1 = %q, want %q", call1.Tool, "search")
	}
	// Append synthetic observation.
	traj.Steps = append(traj.Steps, planner.Step{
		Action:         call1,
		Observation:    map[string]any{"hits": 3},
		LLMObservation: "found 3 hits",
		StartedAt:      time.Now(),
	})

	// --- Step 2 ---
	rc2 := rcWith(q, "find and summarise foo", nil)
	rc2.Trajectory = traj
	dec2, err := p.Next(ctx, rc2)
	if err != nil {
		t.Fatalf("Next #2: %v", err)
	}
	call2, ok := dec2.(planner.CallTool)
	if !ok {
		t.Fatalf("decision #2 = %T, want planner.CallTool", dec2)
	}
	if call2.Tool != "summarize" {
		t.Errorf("Tool #2 = %q, want %q", call2.Tool, "summarize")
	}
	traj.Steps = append(traj.Steps, planner.Step{
		Action:         call2,
		LLMObservation: "summary: bar is foo's friend",
		StartedAt:      time.Now(),
	})

	// --- Step 3 ---
	rc3 := rcWith(q, "find and summarise foo", nil)
	rc3.Trajectory = traj
	dec3, err := p.Next(ctx, rc3)
	if err != nil {
		t.Fatalf("Next #3: %v", err)
	}
	fin3, ok := dec3.(planner.Finish)
	if !ok {
		t.Fatalf("decision #3 = %T, want planner.Finish", dec3)
	}
	if fin3.Reason != planner.FinishGoal {
		t.Errorf("Reason #3 = %q, want %q", fin3.Reason, planner.FinishGoal)
	}
	if fin3.Payload != "done" {
		t.Errorf("Payload #3 = %v, want %q", fin3.Payload, "done")
	}

	if client.callCount() != 3 {
		t.Errorf("LLM call count = %d, want 3", client.callCount())
	}
}

// TestReact_ConfigOverrides asserts each functional option applies.
func TestReact_ConfigOverrides(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			finishToolCallResp("call_1", "x"),
		},
	}
	customSystem := "custom system prompt"
	customBuilder := &capturingBuilder{}

	p := react.New(client,
		react.WithMaxSteps(99),
		react.WithRepairAttempts(7),
		react.WithMaxConsecutiveArgFailures(5),
		react.WithArgFillEnabled(false),
		react.WithPromptBuilder(customBuilder),
		react.WithSystemPrompt(customSystem),
	)
	q := fixedQuadruple(t, "r-override")
	if _, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil)); err != nil {
		t.Fatalf("Next: %v", err)
	}
	// The custom builder should have been called once.
	if customBuilder.calls.Load() != 1 {
		t.Errorf("custom builder calls = %d, want 1", customBuilder.calls.Load())
	}
	// The system prompt should have been forwarded verbatim.
	if got := customBuilder.lastSystem.Load(); got == nil || *got != customSystem {
		t.Errorf("system prompt forwarded = %v, want %q", got, customSystem)
	}
}

// TestReact_NilPromptBuilderOptionIsNoop asserts a nil builder
// passed via WithPromptBuilder leaves the default in place.
func TestReact_NilPromptBuilderOptionIsNoop(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			finishToolCallResp("call_1", "ok"),
		},
	}
	p := react.New(client, react.WithPromptBuilder(nil))
	q := fixedQuadruple(t, "r-nil-builder")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if _, ok := dec.(planner.Finish); !ok {
		t.Fatalf("decision = %T, want planner.Finish", dec)
	}
}

// TestReact_LLMErrorPropagatesVerbatim asserts an LLM-level error
// from the client bubbles out of Next as a non-Decision return. The
// planner does NOT try to swallow upstream errors (§13 fail-loudly;
// the planner contract is `(Decision, error)`).
func TestReact_LLMErrorPropagatesVerbatim(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("upstream LLM transient failure")
	client := &scriptedClient{
		responses: []llm.CompleteResponse{{}},
		errs:      []error{wantErr},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-llm-err")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err == nil {
		t.Fatalf("Next returned nil err, want %v", wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want errors.Is %v", err, wantErr)
	}
	if dec != nil {
		t.Errorf("dec = %v, want nil on error path", dec)
	}
}

// TestStepsTaken_TracksSuccessfulNextCalls asserts the diagnostic
// counter increments on each successful Next.
func TestStepsTaken_TracksSuccessfulNextCalls(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			nativeToolCallResp("call_1", "alpha", `{}`),
			finishToolCallResp("call_2", ""),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-counter")
	for i := range 2 {
		if _, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil)); err != nil {
			t.Fatalf("Next #%d: %v", i+1, err)
		}
	}
	if got := p.StepsTaken(); got != 2 {
		t.Errorf("StepsTaken = %d, want 2", got)
	}
}

// TestNext_SpawnTaskEmissionMappedToSpawnTaskDecision asserts the
// Phase 47 (D-056) spawn-task emission path: when the LLM emits the
// reserved tool name `_spawn_task`, the planner translates the
// envelope into a typed planner.SpawnTask Decision with Kind + Spec
// fields populated. Background is the documented default kind; the
// retain-turn / fail-fast / priority fields round-trip.
func TestNext_SpawnTaskEmissionMappedToSpawnTaskDecision(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			nativeToolCallResp("call_1", "_spawn_task",
				`{"kind":"background","spec":{"description":"summarise document X","query":"summarise X","priority":5,"retain_turn":false,"fail_fast":true},"group_id":"g-42"}`),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-spawn")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	spawn, ok := dec.(planner.SpawnTask)
	if !ok {
		t.Fatalf("decision = %T, want planner.SpawnTask (Phase 47 / D-056)", dec)
	}
	if spawn.Kind != "background" {
		t.Errorf("Kind = %q, want %q", spawn.Kind, "background")
	}
	if spawn.Spec.Description != "summarise document X" {
		t.Errorf("Spec.Description = %q, want %q", spawn.Spec.Description, "summarise document X")
	}
	if spawn.Spec.Priority != 5 {
		t.Errorf("Spec.Priority = %d, want 5", spawn.Spec.Priority)
	}
	if spawn.Spec.RetainTurn {
		t.Errorf("Spec.RetainTurn = true, want false (push-wake default per D-032)")
	}
	if !spawn.Spec.FailFast {
		t.Errorf("Spec.FailFast = false, want true")
	}
	if string(spawn.GroupID) != "g-42" {
		t.Errorf("GroupID = %q, want %q", spawn.GroupID, "g-42")
	}
}

// TestNext_SpawnTaskDefaultsKindToBackground asserts the documented
// default: when the LLM omits `kind`, the planner stamps
// `tasks.KindBackground`.
func TestNext_SpawnTaskDefaultsKindToBackground(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			nativeToolCallResp("call_1", "_spawn_task", `{"spec":{"description":"bg","query":"q"}}`),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-spawn-default")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	spawn, ok := dec.(planner.SpawnTask)
	if !ok {
		t.Fatalf("decision = %T, want planner.SpawnTask", dec)
	}
	if spawn.Kind != "background" {
		t.Errorf("Kind = %q, want %q (default)", spawn.Kind, "background")
	}
}

// TestNext_SpawnTaskMalformedArgsFailsLoudly asserts fail-loudly
// translation: malformed JSON in `args` returns wrapped
// planner.ErrInvalidDecision rather than silently emitting a literal
// `_spawn_task` CallTool (which the dispatcher would reject anyway —
// the planner surfaces the error at translation time per §13).
func TestNext_SpawnTaskMalformedArgsFailsLoudly(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			// args is a string, not an object — JSON-valid but
			// Unmarshal into the spawn envelope struct fails.
			nativeToolCallResp("call_1", "_spawn_task", `"this is not an object"`),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-spawn-mal")
	_, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err == nil {
		t.Fatal("Next returned nil err, want wrapped ErrInvalidDecision")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Errorf("err = %v, want errors.Is planner.ErrInvalidDecision", err)
	}
}

// TestNext_SpawnTaskInvalidKindFailsLoudly asserts an unknown
// `kind` value (anything other than foreground/background) is
// rejected at translation time.
func TestNext_SpawnTaskInvalidKindFailsLoudly(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			nativeToolCallResp("call_1", "_spawn_task",
				`{"kind":"poltergeist","spec":{"description":"d","query":"q"}}`),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-spawn-kind")
	_, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err == nil {
		t.Fatal("Next returned nil err, want wrapped ErrInvalidDecision")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Errorf("err = %v, want errors.Is planner.ErrInvalidDecision", err)
	}
}

// TestNext_AwaitTaskEmissionMappedToAwaitTaskDecision asserts the
// Phase 47 (D-056) await-task emission: when the LLM emits
// `_await_task` with a `task_id`, the planner returns a typed
// planner.AwaitTask Decision.
func TestNext_AwaitTaskEmissionMappedToAwaitTaskDecision(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			nativeToolCallResp("call_1", "_await_task", `{"task_id":"t-99"}`),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-await")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	aw, ok := dec.(planner.AwaitTask)
	if !ok {
		t.Fatalf("decision = %T, want planner.AwaitTask (Phase 47 / D-056)", dec)
	}
	if string(aw.TaskID) != "t-99" {
		t.Errorf("TaskID = %q, want %q", aw.TaskID, "t-99")
	}
}

// TestNext_AwaitTaskEmptyIDFailsLoudly asserts the fail-loudly path:
// empty task_id returns wrapped ErrInvalidDecision.
func TestNext_AwaitTaskEmptyIDFailsLoudly(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			nativeToolCallResp("call_1", "_await_task", `{"task_id":""}`),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-await-empty")
	_, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err == nil {
		t.Fatal("Next returned nil err, want wrapped ErrInvalidDecision")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Errorf("err = %v, want errors.Is planner.ErrInvalidDecision", err)
	}
}

// TestNext_AwaitTaskMalformedJSONFailsLoudly asserts malformed args
// JSON returns wrapped ErrInvalidDecision.
func TestNext_AwaitTaskMalformedJSONFailsLoudly(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			nativeToolCallResp("call_1", "_await_task", `[1,2,3]`),
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-await-mal")
	_, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err == nil {
		t.Fatal("Next returned nil err, want wrapped ErrInvalidDecision")
	}
	if !errors.Is(err, planner.ErrInvalidDecision) {
		t.Errorf("err = %v, want errors.Is planner.ErrInvalidDecision", err)
	}
}

// promptCapturingClient records the most recent CompleteRequest the
// planner built so a test can assert on the rendered system prompt.
// The scripted `_finish` response keeps the planner's Next call on a
// terminal path.
type promptCapturingClient struct {
	lastReq atomic.Pointer[llm.CompleteRequest]
}

func (c *promptCapturingClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	r := req
	c.lastReq.Store(&r)
	return finishToolCallResp("call_done", "done"), nil
}

func (c *promptCapturingClient) Close(_ context.Context) error { return nil }

// systemPromptText returns the rendered system message of the last
// captured request, or "" when no request was captured.
func (c *promptCapturingClient) systemPromptText() string {
	req := c.lastReq.Load()
	if req == nil || len(req.Messages) == 0 {
		return ""
	}
	if req.Messages[0].Content.Text == nil {
		return ""
	}
	return *req.Messages[0].Content.Text
}

// TestDefaultSystemPrompt_DocumentsAllThreeReservedNames asserts the
// rendered default system prompt still documents `_finish` (it appears
// in `<tool_discovery>`, `<tone>`, and `<reasoning>`) so the LLM
// understands the terminal condition. Phase 107c (D-167) deletes the
// `<action_schema>` section — `_spawn_task` and `_await_task` are no
// longer in the prompt text (those opcodes were specific to the
// prompt-engineered JSON-action format).
func TestDefaultSystemPrompt_DocumentsAllThreeReservedNames(t *testing.T) {
	t.Parallel()
	client := &promptCapturingClient{}
	p := react.New(client)
	ctx, err := identity.WithRun(context.Background(),
		identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, "run-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	if _, err := p.Next(ctx, planner.RunContext{
		Quadruple: fixedQuadruple(t, "run-1"),
		Goal:      "g",
	}); err != nil {
		t.Fatalf("Next: %v", err)
	}
	body := client.systemPromptText()
	if !strings.Contains(body, "_finish") {
		t.Errorf("rendered default prompt missing _finish")
	}
	// Phase 107c (D-167): _spawn_task and _await_task were in the
	// deleted <action_schema> — they are not expected in the prompt.
}

// capturingBuilder is a PromptBuilder used to verify
// WithPromptBuilder / WithSystemPrompt routing.
type capturingBuilder struct {
	calls      atomic.Int64
	lastSystem atomic.Pointer[string]
}

func (c *capturingBuilder) Build(rc planner.RunContext, systemPrompt string) llm.CompleteRequest {
	c.calls.Add(1)
	c.lastSystem.Store(&systemPrompt)
	t := rc.Goal
	return llm.CompleteRequest{
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &t}},
		},
	}
}
