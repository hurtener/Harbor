package steering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

// recordingHookExecutor is a narrow steering.ToolExecutor stub that records
// every ExecuteDecision call (the planner's tool dispatches AND the
// run-completion hook dispatch), decoding a RunCompletionPayload when the
// args parse as one. It optionally returns an error for a named tool (the
// hook-failure leg). Thread-safe: the no-bleed concurrency test shares one
// instance across N runs.
type recordingHookExecutor struct {
	mu      sync.Mutex
	calls   []recordedHookCall
	failFor map[string]error
}

type recordedHookCall struct {
	tool     string
	identity identity.Quadruple
	ctxErr   error
	payload  RunCompletionPayload
	parsed   bool
}

func (e *recordingHookExecutor) ExecuteDecision(ctx context.Context, _ planner.RunContext, decision planner.Decision) (any, any, error) {
	ct, ok := decision.(planner.CallTool)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %T", ErrDecisionShapeUnsupported, decision)
	}
	q, _ := identity.QuadrupleFrom(ctx)
	rec := recordedHookCall{tool: ct.Tool, identity: q, ctxErr: ctx.Err()}
	if err := json.Unmarshal(ct.Args, &rec.payload); err == nil {
		rec.parsed = true
	}
	e.mu.Lock()
	e.calls = append(e.calls, rec)
	e.mu.Unlock()
	if e.failFor != nil {
		if err := e.failFor[ct.Tool]; err != nil {
			return nil, nil, err
		}
	}
	return "ok", "ok", nil
}

// hookCalls returns the recorded dispatches that targeted the fixture hook
// sink (hookTool), excluding any planner-tool dispatches.
func (e *recordingHookExecutor) hookCalls() []recordedHookCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []recordedHookCall
	for _, c := range e.calls {
		if c.tool == hookTool {
			out = append(out, c)
		}
	}
	return out
}

const hookTool = "run_transcript_sink"

// ---------------------------------------------------------------------------
// Outcome mapping.
// ---------------------------------------------------------------------------

func TestOutcomeFor_Table(t *testing.T) {
	cases := []struct {
		name string
		fin  planner.Finish
		err  error
		want string
	}{
		{"goal", planner.Finish{Reason: planner.FinishGoal}, nil, "goal"},
		{"no_path", planner.Finish{Reason: planner.FinishNoPath}, nil, "no_path"},
		{"cancelled_finish", planner.Finish{Reason: planner.FinishCancelled}, nil, "cancelled"},
		{"deadline", planner.Finish{Reason: planner.FinishDeadlineExceeded}, nil, "deadline_exceeded"},
		{"constraints", planner.Finish{Reason: planner.FinishConstraintsConflict}, nil, "constraints_conflict"},
		{"err_generic", planner.Finish{}, errors.New("boom"), "error"},
		{"err_cancelled", planner.Finish{}, fmt.Errorf("wrap: %w", context.Canceled), "cancelled"},
		{"unknown_reason", planner.Finish{Reason: planner.FinishReason("weird")}, nil, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := outcomeFor(c.fin, c.err); got != c.want {
				t.Errorf("outcomeFor(%v, %v) = %q, want %q", c.fin.Reason, c.err, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Transcript assembly ordering.
// ---------------------------------------------------------------------------

func TestAssembleTranscript_InterleavedAcrossSteps(t *testing.T) {
	traj := &planner.Trajectory{
		Steps: []planner.Step{
			{Action: planner.CallTool{Tool: "search"}, AssistantPreamble: "let me search"},
			{Action: planner.CallTool{Tool: "fetch"}, Observation: map[string]any{"error": "boom"}},
		},
	}
	// Steering: a user message before step 0, a redirect before step 1, a
	// trailing user message after the last step.
	entries := []steeringEntry{
		{kind: ControlUserMessage, content: "hello mid-run", step: 0},
		{kind: ControlRedirect, content: "new goal", step: 1},
		{kind: ControlUserMessage, content: "final nudge", step: 2},
	}
	got := assembleTranscript("original goal", traj, entries, "the answer", true)

	want := []struct{ role, kind, content string }{
		{transcriptRoleUser, transcriptKindGoal, "original goal"},
		{transcriptRoleUser, transcriptKindUserMessage, "hello mid-run"},
		{transcriptRoleAssistant, transcriptKindAssistant, "let me search"},
		{transcriptRoleAssistant, transcriptKindTool, "search: ok"},
		{transcriptRoleUser, transcriptKindRedirect, "new goal"},
		{transcriptRoleAssistant, transcriptKindTool, "fetch: err"},
		{transcriptRoleUser, transcriptKindUserMessage, "final nudge"},
		{transcriptRoleAssistant, transcriptKindFinalAnswer, "the answer"},
	}
	if len(got) != len(want) {
		t.Fatalf("transcript length = %d, want %d\n%+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Role != w.role || got[i].Kind != w.kind || got[i].Content != w.content {
			t.Errorf("entry[%d] = {%q,%q,%q}, want {%q,%q,%q}",
				i, got[i].Role, got[i].Kind, got[i].Content, w.role, w.kind, w.content)
		}
	}
}

func TestAssembleTranscript_EmptyTrajectory(t *testing.T) {
	got := assembleTranscript("just a goal", nil, nil, "", false)
	if len(got) != 1 || got[0].Kind != transcriptKindGoal || got[0].Content != "just a goal" {
		t.Fatalf("empty-trajectory transcript = %+v, want a single goal entry", got)
	}
}

func TestAssembleTranscript_CallParallel_OneLinePerBranch(t *testing.T) {
	traj := &planner.Trajectory{Steps: []planner.Step{
		{Action: planner.CallParallel{Branches: []planner.CallTool{{Tool: "a"}, {Tool: "b"}}}},
	}}
	got := assembleTranscript("g", traj, nil, "", false)
	// goal + two tool lines.
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	if got[1].Content != "a: ok" || got[2].Content != "b: ok" {
		t.Errorf("tool lines = %q, %q, want 'a: ok', 'b: ok'", got[1].Content, got[2].Content)
	}
}

// ---------------------------------------------------------------------------
// Golden payload JSON.
// ---------------------------------------------------------------------------

func TestBuildRunCompletionPayload_Golden(t *testing.T) {
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"},
		RunID:    "run-a",
	}
	started := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	completed := time.Date(2026, 7, 2, 10, 0, 2, 500000000, time.UTC)
	traj := &planner.Trajectory{Steps: []planner.Step{
		{Action: planner.CallTool{Tool: "search"}, AssistantPreamble: "searching"},
	}}
	entries := []steeringEntry{{kind: ControlUserMessage, content: "wait, also check X", step: 1}}
	fin := planner.Finish{Reason: planner.FinishGoal, Payload: "done"}

	p := buildRunCompletionPayload(q, "agent-42", "goal", started, completed, "initial goal", traj, entries, fin)
	got, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	const want = `{
  "format_version": 1,
  "tenant_id": "tenant-a",
  "user_id": "user-a",
  "session_id": "session-a",
  "run_id": "run-a",
  "agent_id": "agent-42",
  "outcome": "goal",
  "started_at": "2026-07-02T10:00:00Z",
  "completed_at": "2026-07-02T10:00:02.5Z",
  "duration_ms": 2500,
  "step_count": 1,
  "tool_invocations": 1,
  "conversation": [
    {
      "role": "user",
      "kind": "goal",
      "content": "initial goal",
      "step": 0
    },
    {
      "role": "assistant",
      "kind": "assistant",
      "content": "searching",
      "step": 0
    },
    {
      "role": "assistant",
      "kind": "tool",
      "content": "search: ok",
      "step": 0
    },
    {
      "role": "user",
      "kind": "user_message",
      "content": "wait, also check X",
      "step": 1
    },
    {
      "role": "assistant",
      "kind": "final_answer",
      "content": "done",
      "step": 1
    }
  ]
}`
	if string(got) != want {
		t.Errorf("golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// ---------------------------------------------------------------------------
// End-to-end fire over the RunLoop terminal boundary.
// ---------------------------------------------------------------------------

func hookRunSpec(q identity.Quadruple, p planner.Planner, exec ToolExecutor, hook *CompletionHookSpec) RunSpec {
	return RunSpec{
		Planner:        p,
		Base:           planner.RunContext{Quadruple: q, Goal: "reach the goal", Trajectory: &planner.Trajectory{Query: "reach the goal"}},
		MaxSteps:       16,
		ToolExecutor:   exec,
		CompletionHook: hook,
	}
}

func TestRun_CompletionHook_FiresOnGoal_DispatchesTranscript(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	exec := &recordingHookExecutor{}
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal, Payload: "answer"}}

	fin, err := rl.Run(context.Background(), hookRunSpec(runA, p, exec, &CompletionHookSpec{Tool: hookTool, AgentID: "agent-a"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Fatalf("Finish.Reason = %q, want goal", fin.Reason)
	}
	calls := exec.hookCalls()
	if len(calls) != 1 {
		t.Fatalf("hook dispatched %d times, want exactly 1", len(calls))
	}
	c := calls[0]
	if !c.parsed {
		t.Fatal("hook args did not decode as a RunCompletionPayload")
	}
	if c.identity != runA {
		t.Errorf("hook ctx identity = %+v, want %+v", c.identity, runA)
	}
	if c.payload.Outcome != "goal" || c.payload.AgentID != "agent-a" {
		t.Errorf("payload outcome/agent = %q/%q, want goal/agent-a", c.payload.Outcome, c.payload.AgentID)
	}
	if c.payload.FormatVersion != RunCompletionPayloadFormatVersion {
		t.Errorf("payload format_version = %d, want %d", c.payload.FormatVersion, RunCompletionPayloadFormatVersion)
	}
	// run.hook_dispatched emitted, run.hook_failed not.
	if n := bus.countType(EventTypeRunHookDispatched); n != 1 {
		t.Errorf("run.hook_dispatched count = %d, want 1", n)
	}
	if n := bus.countType(EventTypeRunHookFailed); n != 0 {
		t.Errorf("run.hook_failed count = %d, want 0", n)
	}
}

func TestRun_CompletionHook_NilHook_ByteIdentical(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	exec := &recordingHookExecutor{}
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal}}
	if _, err := rl.Run(context.Background(), hookRunSpec(runA, p, exec, nil)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls := exec.hookCalls(); len(calls) != 0 {
		t.Fatalf("nil hook still dispatched %d times, want 0", len(calls))
	}
}

func TestRun_CompletionHook_D274CountersUntouched(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	exec := &recordingHookExecutor{}
	// One real CallTool step, then Finish.
	p := &scriptedPlanner{
		script:     []scriptStep{{dec: planner.CallTool{Tool: "real_tool"}}},
		defaultDec: planner.Finish{Reason: planner.FinishGoal},
	}
	var dispatchedCount int
	spec := hookRunSpec(runA, p, exec, &CompletionHookSpec{Tool: hookTool})
	spec.OnToolDispatched = func(_ context.Context, n int) error { dispatchedCount += n; return nil }

	if _, err := rl.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// OnToolDispatched counts the planner's real tool only — NOT the hook.
	if dispatchedCount != 1 {
		t.Errorf("OnToolDispatched total = %d, want 1 (the hook must not advance the counter)", dispatchedCount)
	}
	// The trajectory carries exactly the one planner step — the hook appended none.
	if got := len(spec.Base.Trajectory.Steps); got != 1 {
		t.Errorf("trajectory steps = %d, want 1 (hook must not append a step)", got)
	}
	// The payload's tool count reflects the trajectory (1), not the hook dispatch.
	calls := exec.hookCalls()
	if len(calls) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(calls))
	}
	if calls[0].payload.ToolInvocations != 1 {
		t.Errorf("payload ToolInvocations = %d, want 1", calls[0].payload.ToolInvocations)
	}
}

func TestRun_CompletionHook_ErrorLeavesOutcomeUnchanged(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	exec := &recordingHookExecutor{failFor: map[string]error{hookTool: errors.New("sink is down")}}
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal, Payload: "answer"}}

	fin, err := rl.Run(context.Background(), hookRunSpec(runA, p, exec, &CompletionHookSpec{Tool: hookTool}))
	if err != nil {
		t.Fatalf("Run err = %v, want nil (a hook failure must not fail the run)", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Finish.Reason = %q, want goal (hook failure must not alter the outcome)", fin.Reason)
	}
	if n := bus.countType(EventTypeRunHookFailed); n != 1 {
		t.Errorf("run.hook_failed count = %d, want 1", n)
	}
	if n := bus.countType(EventTypeRunHookDispatched); n != 0 {
		t.Errorf("run.hook_dispatched count = %d, want 0", n)
	}
}

func TestRun_CompletionHook_NilExecutor_FailsLoudEvent(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal}}
	spec := hookRunSpec(runA, p, nil, &CompletionHookSpec{Tool: hookTool})

	fin, err := rl.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run err = %v, want nil", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Finish.Reason = %q, want goal", fin.Reason)
	}
	if n := bus.countType(EventTypeRunHookFailed); n != 1 {
		t.Errorf("run.hook_failed count = %d, want 1 (nil executor with a configured hook fails loud)", n)
	}
}

func TestRun_CompletionHook_CancelledRun_FiresWithCancelledOutcome(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	exec := &recordingHookExecutor{}
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run ctx is already dead before the loop's first boundary check

	fin, err := rl.Run(ctx, hookRunSpec(runA, p, exec, &CompletionHookSpec{Tool: hookTool}))
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want a context.Canceled-wrapped error", err)
	}
	_ = fin
	calls := exec.hookCalls()
	if len(calls) != 1 {
		t.Fatalf("hook dispatched %d times, want 1 (must fire under the detached ctx even for a cancelled run)", len(calls))
	}
	c := calls[0]
	if c.payload.Outcome != "cancelled" {
		t.Errorf("payload outcome = %q, want cancelled", c.payload.Outcome)
	}
	// The detached hook ctx must NOT be cancelled (WithoutCancel + timeout).
	if c.ctxErr != nil {
		t.Errorf("hook ctx.Err() = %v, want nil (the dispatch runs under a detached, non-cancelled ctx)", c.ctxErr)
	}
	// Identity still flows through WithoutCancel.
	if c.identity != runA {
		t.Errorf("hook ctx identity = %+v, want %+v (WithoutCancel must preserve identity)", c.identity, runA)
	}
	if n := bus.countType(EventTypeRunHookDispatched); n != 1 {
		t.Errorf("run.hook_dispatched count = %d, want 1", n)
	}
}

func TestRun_CompletionHook_ConcurrentNoBleed(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	exec := &recordingHookExecutor{}

	const n = 24
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("tenant-%d", i),
					UserID:    fmt.Sprintf("user-%d", i),
					SessionID: fmt.Sprintf("session-%d", i),
				},
				RunID: fmt.Sprintf("run-%d", i),
			}
			goal := fmt.Sprintf("goal-sentinel-%d", i)
			p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal, Payload: fmt.Sprintf("answer-%d", i)}}
			spec := RunSpec{
				Planner:        p,
				Base:           planner.RunContext{Quadruple: q, Goal: goal, Trajectory: &planner.Trajectory{Query: goal}},
				MaxSteps:       8,
				ToolExecutor:   exec,
				CompletionHook: &CompletionHookSpec{Tool: hookTool},
			}
			if _, err := rl.Run(context.Background(), spec); err != nil {
				t.Errorf("run %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	calls := exec.hookCalls()
	if len(calls) != n {
		t.Fatalf("hook calls = %d, want %d", len(calls), n)
	}
	// Each run's payload must carry its OWN goal + identity — no cross-run bleed.
	for _, c := range calls {
		if !c.parsed {
			t.Fatalf("call for %s did not decode", c.identity.RunID)
		}
		wantGoal := "goal-sentinel-" + trimRunPrefix(c.identity.RunID)
		var gotGoal string
		for _, e := range c.payload.Conversation {
			if e.Kind == transcriptKindGoal {
				gotGoal = e.Content
			}
		}
		if gotGoal != wantGoal {
			t.Errorf("run %s: transcript goal = %q, want %q (cross-run bleed)", c.identity.RunID, gotGoal, wantGoal)
		}
		if c.payload.RunID != c.identity.RunID {
			t.Errorf("payload RunID %q != ctx identity RunID %q", c.payload.RunID, c.identity.RunID)
		}
	}
}

// trimRunPrefix strips the "run-" prefix from a sentinel run id.
func trimRunPrefix(runID string) string {
	const pfx = "run-"
	if len(runID) > len(pfx) && runID[:len(pfx)] == pfx {
		return runID[len(pfx):]
	}
	return runID
}

// countType counts published events of a given type. Extends the fakeBus
// defined in events_test.go (same package).
func (b *fakeBus) countType(t events.EventType) int {
	n := 0
	for _, ev := range b.published {
		if ev.Type == t {
			n++
		}
	}
	return n
}

// panickingExecutor panics inside ExecuteDecision — the third-party-sink
// failure shape the fire's recover must contain.
type panickingExecutor struct{}

func (panickingExecutor) ExecuteDecision(context.Context, planner.RunContext, planner.Decision) (any, any, error) {
	panic("sink exploded")
}

func TestRun_CompletionHook_ExecutorPanic_NeverReplacesRunResult(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal, Payload: "answer"}}

	fin, err := rl.Run(context.Background(), hookRunSpec(runA, p, panickingExecutor{}, &CompletionHookSpec{Tool: hookTool}))
	if err != nil {
		t.Fatalf("Run err = %v, want nil (a panicking hook sink must not fail the run)", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Finish.Reason = %q, want goal (a panicking hook sink must not replace the settled result)", fin.Reason)
	}
	if n := bus.countType(EventTypeRunHookFailed); n != 1 {
		t.Errorf("run.hook_failed count = %d, want 1 (the contained panic surfaces as a classified failure)", n)
	}
	failed, ok := bus.published[len(bus.published)-1].Payload.(RunHookFailedPayload)
	if !ok || failed.ErrorClass != "panic" {
		t.Errorf("run.hook_failed ErrorClass = %+v, want the 'panic' class", bus.published[len(bus.published)-1].Payload)
	}
}
