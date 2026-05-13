package react_test

import (
	"context"
	"encoding/json"
	"errors"
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

// TestNew_AppliesDefaults asserts the zero-options constructor sets
// every documented default.
func TestNew_AppliesDefaults(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			{Content: `{"tool":"_finish","args":{"answer":"ok"}}`},
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
			{Content: `{"tool":"_finish","args":{}}`},
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
			{Content: `{"tool":"_finish","args":{"answer":"42"},"reasoning":"the answer"}`},
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
	if v, _ := fin.Metadata["reasoning"].(string); v != "the answer" {
		t.Errorf("Metadata[reasoning] = %v, want %q", v, "the answer")
	}
	if v, _ := fin.Metadata["via"].(string); v != "react._finish" {
		t.Errorf("Metadata[via] = %v, want react._finish", v)
	}
}

// TestNext_ParallelPassesThroughVerbatim asserts the Phase 47 (D-056)
// upgrade: when the repair loop returns CallParallel (multi-action
// salvage), the planner passes it through unchanged. The runtime
// parallel executor (internal/runtime/parallel) consumes the shape;
// the §13 import-graph contract forbids the planner subtree from
// importing internal/runtime/..., so the executor is OUTSIDE the
// planner package by design.
//
// This test supersedes the prior V1 reduction test
// (`TestNext_ParallelReducesToFirstCallTool`) — the Phase 45 D-051
// stop-gap reduction override was DELETED in Phase 47.
func TestNext_ParallelPassesThroughVerbatim(t *testing.T) {
	t.Parallel()
	// Multi-action response — Phase 44's parser produces a
	// CallParallel from a JSON array.
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			{Content: `[{"tool":"alpha","args":{"x":1}},{"tool":"beta","args":{"y":2}},{"tool":"gamma","args":{"z":3}}]`},
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-par")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", nil))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	par, ok := dec.(planner.CallParallel)
	if !ok {
		t.Fatalf("decision = %T, want planner.CallParallel (Phase 47 / D-056 pass-through)", dec)
	}
	if got, want := len(par.Branches), 3; got != want {
		t.Fatalf("len(Branches) = %d, want %d", got, want)
	}
	wantNames := []string{"alpha", "beta", "gamma"}
	for i, b := range par.Branches {
		if b.Tool != wantNames[i] {
			t.Errorf("Branches[%d].Tool = %q, want %q", i, b.Tool, wantNames[i])
		}
	}
	if par.Join == nil || par.Join.Kind != planner.JoinAll {
		t.Errorf("Join = %+v, want JoinAll (Phase 44 multi-action salvage default)", par.Join)
	}
}

// TestNext_ParallelWithFinishFirstStillFinishes asserts the special
// case: if the first parallel branch is `_finish`, the planner
// converts it to a Finish Decision (translating reserved names in the
// first branch is the symmetric rule with the single-action path).
// Phase 47 (D-056) preserves this special case even after the
// CallParallel pass-through landed.
func TestNext_ParallelWithFinishFirstStillFinishes(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			{Content: `[{"tool":"_finish","args":{"answer":"early"}},{"tool":"discarded","args":{}}]`},
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
		t.Fatalf("decision = %T, want planner.Finish (parallel first branch _finish should still finish)", dec)
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
			{Content: `{"tool":"_finish","args":{}}`}, // never reached
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

// TestNext_RepairExhaustionPropagatesFinish asserts the planner
// propagates the Phase 44 loop's graceful-failure terminal verbatim
// (the loop emits planner.repair_exhausted; the planner does NOT
// re-emit).
func TestNext_RepairExhaustionPropagatesFinish(t *testing.T) {
	t.Parallel()
	// Stub returns malformed JSON forever — repair loop's
	// MaxConsecutiveArgFailures (default 2) trips.
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			{Content: `garbage no json`},
		},
	}
	rec := &recordingEmit{}
	p := react.New(client)
	q := fixedQuadruple(t, "r-repair-exhaust")
	dec, err := p.Next(ctxWith(t, q), rcWith(q, "g", rec.emit))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	fin, ok := dec.(planner.Finish)
	if !ok {
		t.Fatalf("decision = %T, want planner.Finish (repair-exhausted graceful failure)", dec)
	}
	if fin.Reason != planner.FinishNoPath {
		t.Errorf("Reason = %q, want %q", fin.Reason, planner.FinishNoPath)
	}
	if got, _ := fin.Metadata["followup"].(bool); !got {
		t.Errorf("Metadata[followup] not true — Phase 44 contract surface")
	}

	// Event observation: the loop emits planner.repair_exhausted,
	// not planner.max_steps_exceeded. The planner-side MaxSteps
	// breaker MUST NOT fire here (the trajectory is nil / empty).
	emitted := rec.snapshot()
	var sawRepair, sawMax bool
	for _, ev := range emitted {
		switch ev.Type {
		case planner.EventTypePlannerRepairExhausted:
			sawRepair = true
		case planner.EventTypePlannerMaxStepsExceeded:
			sawMax = true
		}
	}
	if !sawRepair {
		t.Errorf("planner.repair_exhausted not emitted by Phase 44 loop")
	}
	if sawMax {
		t.Errorf("planner.max_steps_exceeded should NOT fire when MaxSteps is not hit")
	}
}

// TestReact_ThreeStepScenario is the load-bearing acceptance
// criterion from Phase 45's master-plan detail block: "3-step
// reasoning task succeeds against a mock LLM."
//
// The scripted mock LLM emits:
//
//	call 1: {"tool":"search","args":{"q":"foo"},"reasoning":"step1"}
//	call 2: {"tool":"summarize","args":{"text":"bar"},"reasoning":"step2"}
//	call 3: {"tool":"_finish","args":{"answer":"done"},"reasoning":"step3"}
//
// The test issues three successive Next calls. After each non-
// terminal Next call the test appends a synthetic Trajectory.Step to
// the RunContext so the next prompt sees the prior step (this is what
// the runtime engine will do once Phase 47 wires the loop).
//
// Asserts the three Decisions and the LLM call count.
func TestReact_ThreeStepScenario(t *testing.T) {
	t.Parallel()
	client := &scriptedClient{
		responses: []llm.CompleteResponse{
			{Content: `{"tool":"search","args":{"q":"foo"},"reasoning":"step1"}`},
			{Content: `{"tool":"summarize","args":{"text":"bar"},"reasoning":"step2"}`},
			{Content: `{"tool":"_finish","args":{"answer":"done"},"reasoning":"step3"}`},
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
			{Content: `{"tool":"_finish","args":{"answer":"x"}}`},
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
			{Content: `{"tool":"_finish","args":{"answer":"ok"}}`},
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
			{Content: `{"tool":"alpha","args":{}}`},
			{Content: `{"tool":"_finish","args":{}}`},
		},
	}
	p := react.New(client)
	q := fixedQuadruple(t, "r-counter")
	for i := 0; i < 2; i++ {
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
			{Content: `{"tool":"_spawn_task","args":{"kind":"background","spec":{"description":"summarise document X","query":"summarise X","priority":5,"retain_turn":false,"fail_fast":true},"group_id":"g-42"},"reasoning":"want a side-channel summary"}`},
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
			{Content: `{"tool":"_spawn_task","args":{"spec":{"description":"bg","query":"q"}}}`},
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
			// args is a string, not an object — JSON-valid at the parser
			// (parser accepts any args shape) but Unmarshal into the
			// envelope struct fails.
			{Content: `{"tool":"_spawn_task","args":"this is not an object"}`},
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
			{Content: `{"tool":"_spawn_task","args":{"kind":"poltergeist","spec":{"description":"d","query":"q"}}}`},
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
			{Content: `{"tool":"_await_task","args":{"task_id":"t-99"},"reasoning":"block on the spawn"}`},
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
			{Content: `{"tool":"_await_task","args":{"task_id":""}}`},
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
			{Content: `{"tool":"_await_task","args":[1,2,3]}`},
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

// TestDefaultSystemPrompt_DocumentsAllThreeReservedNames asserts the
// system prompt documents `_finish`, `_spawn_task`, `_await_task` so
// the LLM can emit them without prompt-engineering at the call site.
// The string-grep is intentionally brittle — drift in the prompt
// surfaces here at test time.
func TestDefaultSystemPrompt_DocumentsAllThreeReservedNames(t *testing.T) {
	t.Parallel()
	if !strings.Contains(react.DefaultSystemPrompt, "_finish") {
		t.Errorf("DefaultSystemPrompt missing _finish (D-056 reserved names)")
	}
	if !strings.Contains(react.DefaultSystemPrompt, "_spawn_task") {
		t.Errorf("DefaultSystemPrompt missing _spawn_task (D-056 reserved names)")
	}
	if !strings.Contains(react.DefaultSystemPrompt, "_await_task") {
		t.Errorf("DefaultSystemPrompt missing _await_task (D-056 reserved names)")
	}
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
