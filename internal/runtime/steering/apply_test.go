package steering

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tasks"
)

// stubTaskRegistry embeds tasks.TaskRegistry (so it satisfies the
// interface) but only implements Prioritize — the one method the
// steering apply path reaches. Every other method panics if called,
// which is the loud failure a test wants if the apply path drifts.
type stubTaskRegistry struct {
	tasks.TaskRegistry
	prioritizeE error
	lastTaskID  tasks.TaskID
	prioCalls   int
	lastPrio    int
	mu          sync.Mutex
}

func (s *stubTaskRegistry) Prioritize(_ context.Context, id tasks.TaskID, priority int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prioCalls++
	s.lastTaskID = id
	s.lastPrio = priority
	if s.prioritizeE != nil {
		return false, s.prioritizeE
	}
	return true, nil
}

func (s *stubTaskRegistry) snapshot() (calls int, id tasks.TaskID, prio int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prioCalls, s.lastTaskID, s.lastPrio
}

// newTestApplier builds an applier with stub dependencies.
func newTestApplier(coord pauseresume.Coordinator, tr tasks.TaskRegistry, hook func(context.Context, string) error) *applier {
	return &applier{coord: coord, taskRegistry: tr, hardCancelHook: hook}
}

// ---------------------------------------------------------------------------
// Accumulating events — INJECT_CONTEXT / USER_MESSAGE / REDIRECT / CANCEL /
// PAUSE. None of these reach a runtime dependency; they only mutate sc.
// ---------------------------------------------------------------------------

func TestApplyEvent_InjectContext_Appends(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlInjectContext, Identity: runA, Payload: map[string]any{"note": "hello"}}
	if err := a.applyEvent(context.Background(), sc, ev, ""); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if len(sc.signals.InjectedContext) != 1 {
		t.Fatalf("InjectedContext len = %d, want 1", len(sc.signals.InjectedContext))
	}
	if sc.signals.InjectedContext[0]["note"] != "hello" {
		t.Errorf("InjectedContext[0] = %v, want {note: hello}", sc.signals.InjectedContext[0])
	}
}

func TestApplyEvent_UserMessage_Appends(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlUserMessage, Identity: runA, Payload: map[string]any{"message": "do the thing"}}
	if err := a.applyEvent(context.Background(), sc, ev, ""); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if len(sc.signals.UserMessages) != 1 || sc.signals.UserMessages[0] != "do the thing" {
		t.Errorf("UserMessages = %v, want [do the thing]", sc.signals.UserMessages)
	}
}

func TestApplyEvent_Redirect_SetsGoal(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlRedirect, Identity: runA, Payload: map[string]any{"goal": "new goal"}}
	if err := a.applyEvent(context.Background(), sc, ev, ""); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if sc.goal != "new goal" {
		t.Errorf("sc.goal = %q, want %q", sc.goal, "new goal")
	}
	if sc.signals.RedirectGoal != "new goal" {
		t.Errorf("sc.signals.RedirectGoal = %q, want %q", sc.signals.RedirectGoal, "new goal")
	}
}

func TestApplyEvent_CancelSoft_SetsCancelled(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlCancel, Identity: runA, Payload: map[string]any{"hard": false}}
	if err := a.applyEvent(context.Background(), sc, ev, ""); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if !sc.signals.Cancelled {
		t.Error("soft CANCEL did not set Control.Cancelled")
	}
	if sc.hardCancel {
		t.Error("soft CANCEL set hardCancel — only payload.hard==true should")
	}
}

func TestApplyEvent_CancelHard_SetsHardCancel(t *testing.T) {
	var hookCalls int
	hook := func(_ context.Context, runID string) error { hookCalls++; return nil }
	a := newTestApplier(&stubCoordinator{}, nil, hook)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlCancel, Identity: runA, Payload: map[string]any{"hard": true}}
	if err := a.applyEvent(context.Background(), sc, ev, ""); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if !sc.signals.Cancelled {
		t.Error("hard CANCEL did not set Control.Cancelled")
	}
	if !sc.hardCancel {
		t.Error("hard CANCEL did not set sc.hardCancel")
	}
	// applyEvent records the flag; the RunLoop fires the hook. Verify
	// the hook path directly here.
	if err := a.hardCancel(context.Background(), "run-a"); err != nil {
		t.Fatalf("hardCancel: %v", err)
	}
	if hookCalls != 1 {
		t.Errorf("hard-cancel hook calls = %d, want 1", hookCalls)
	}
}

func TestApplyEvent_HardCancel_NilHookTolerated(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil) // nil hook
	if err := a.hardCancel(context.Background(), "run-a"); err != nil {
		t.Errorf("hardCancel with nil hook should be a no-op, got %v", err)
	}
}

func TestApplyEvent_Pause_SetsPauseRequested(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlPause, Identity: runA}
	if err := a.applyEvent(context.Background(), sc, ev, ""); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if !sc.signals.PauseRequested || !sc.pauseRequested {
		t.Error("PAUSE did not set PauseRequested")
	}
	// PAUSE must NOT itself call Coordinator.Request — the planner's
	// RequestPause decision does.
	if c := a.coord.(*stubCoordinator); func() int { r, _ := c.snapshot(); return r }() != 0 {
		t.Error("PAUSE called Coordinator.Request directly — it must not; the planner's RequestPause does")
	}
}

// ---------------------------------------------------------------------------
// Acting events — RESUME / APPROVE / REJECT reach the Coordinator;
// PRIORITIZE reaches the TaskRegistry.
// ---------------------------------------------------------------------------

func TestApplyEvent_Resume_CallsCoordinatorResume(t *testing.T) {
	coord := &stubCoordinator{}
	a := newTestApplier(coord, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlResume, Identity: runA}
	if err := a.applyEvent(context.Background(), sc, ev, pauseresume.Token("tok-1")); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if _, res := coord.snapshot(); res != 1 {
		t.Errorf("Coordinator.Resume calls = %d, want 1", res)
	}
	if sc.resumeKind != ControlResume {
		t.Errorf("sc.resumeKind = %q, want RESUME", sc.resumeKind)
	}
	// D-096: the typed pauseresume.Decision is derived from the
	// ControlType and threaded into Coordinator.Resume.
	coord.mu.Lock()
	got := coord.lastResumeDecision
	coord.mu.Unlock()
	if got != pauseresume.DecisionResume {
		t.Errorf("Coordinator.Resume Decision = %q, want %q", got, pauseresume.DecisionResume)
	}
}

func TestApplyEvent_Approve_CallsCoordinatorResume(t *testing.T) {
	coord := &stubCoordinator{}
	a := newTestApplier(coord, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlApprove, Identity: runA}
	if err := a.applyEvent(context.Background(), sc, ev, pauseresume.Token("tok-1")); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if _, res := coord.snapshot(); res != 1 {
		t.Errorf("Coordinator.Resume calls = %d, want 1", res)
	}
	if sc.resumeKind != ControlApprove {
		t.Errorf("sc.resumeKind = %q, want APPROVE", sc.resumeKind)
	}
	// D-096: APPROVE control → DecisionApprove on the wire.
	coord.mu.Lock()
	got := coord.lastResumeDecision
	coord.mu.Unlock()
	if got != pauseresume.DecisionApprove {
		t.Errorf("Coordinator.Resume Decision = %q, want %q", got, pauseresume.DecisionApprove)
	}
}

func TestApplyEvent_Reject_StampsRejectedInPayload(t *testing.T) {
	coord := &stubCoordinator{}
	a := newTestApplier(coord, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlReject, Identity: runA, Payload: map[string]any{"why": "bad plan"}}
	if err := a.applyEvent(context.Background(), sc, ev, pauseresume.Token("tok-1")); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	coord.mu.Lock()
	pay := coord.lastResumePay
	got := coord.lastResumeDecision
	coord.mu.Unlock()
	if rejected, _ := pay["rejected"].(bool); !rejected {
		t.Error("REJECT did not stamp rejected:true into the resume payload")
	}
	if pay["why"] != "bad plan" {
		t.Error("REJECT dropped the caller's payload fields")
	}
	// D-096: REJECT control → DecisionReject on the wire; the typed
	// Decision is the load-bearing channel, the rejected:true payload
	// stamp is for backward-compatible map observers only.
	if got != pauseresume.DecisionReject {
		t.Errorf("Coordinator.Resume Decision = %q, want %q", got, pauseresume.DecisionReject)
	}
}

func TestApplyEvent_Resume_NoOutstandingPause_FailsLoud(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlResume, Identity: runA}
	err := a.applyEvent(context.Background(), sc, ev, "") // empty token
	if !errors.Is(err, ErrNoOutstandingPause) {
		t.Fatalf("RESUME with no outstanding pause err = %v, want ErrNoOutstandingPause", err)
	}
}

func TestApplyEvent_Resume_CoordinatorError_Propagates(t *testing.T) {
	coordErr := pauseresume.ErrAlreadyResumed
	coord := &stubCoordinator{resumeErr: coordErr}
	a := newTestApplier(coord, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlResume, Identity: runA}
	err := a.applyEvent(context.Background(), sc, ev, pauseresume.Token("tok-1"))
	if !errors.Is(err, coordErr) {
		t.Fatalf("RESUME with a Coordinator error: err = %v, want it to wrap %v", err, coordErr)
	}
}

func TestApplyEvent_Prioritize_RecordsValue(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlPrioritize, Identity: runA, Payload: map[string]any{"priority": float64(42)}}
	if err := a.applyEvent(context.Background(), sc, ev, ""); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if !sc.prioritizeSet || sc.prioritizeVal != 42 {
		t.Errorf("prioritizeSet=%v val=%d, want true/42", sc.prioritizeSet, sc.prioritizeVal)
	}
}

func TestApplyEvent_Prioritize_MissingPriority_FailsLoud(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil)
	sc := &stepControl{}
	ev := ControlEvent{Type: ControlPrioritize, Identity: runA, Payload: map[string]any{}}
	err := a.applyEvent(context.Background(), sc, ev, "")
	if !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("PRIORITIZE with no priority err = %v, want ErrPayloadInvalid", err)
	}
}

func TestApplier_Prioritize_CallsTaskRegistry(t *testing.T) {
	tr := &stubTaskRegistry{}
	a := newTestApplier(&stubCoordinator{}, tr, nil)
	if err := a.prioritize(context.Background(), tasks.TaskID("task-1"), 7); err != nil {
		t.Fatalf("prioritize: %v", err)
	}
	calls, id, prio := tr.snapshot()
	if calls != 1 || id != "task-1" || prio != 7 {
		t.Errorf("Prioritize calls=%d id=%q prio=%d, want 1/task-1/7", calls, id, prio)
	}
}

func TestApplier_Prioritize_NilRegistry_FailsLoud(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, nil, nil)
	err := a.prioritize(context.Background(), tasks.TaskID("task-1"), 7)
	if !errors.Is(err, ErrRunLoopMisconfigured) {
		t.Fatalf("prioritize with nil registry err = %v, want ErrRunLoopMisconfigured", err)
	}
}

func TestApplier_Prioritize_EmptyTaskID_FailsLoud(t *testing.T) {
	a := newTestApplier(&stubCoordinator{}, &stubTaskRegistry{}, nil)
	err := a.prioritize(context.Background(), "", 7)
	if !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("prioritize with empty TaskID err = %v, want ErrPayloadInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// Payload helper coverage.
// ---------------------------------------------------------------------------

func TestPayloadHelpers(t *testing.T) {
	m := map[string]any{
		"s":     "str",
		"b":     true,
		"i":     float64(10),
		"iInt":  7,
		"frac":  float64(1.5),
		"wrong": []any{1, 2},
	}
	if v, ok := stringFromPayload(m, "s"); !ok || v != "str" {
		t.Errorf("stringFromPayload(s) = %q,%v", v, ok)
	}
	if _, ok := stringFromPayload(m, "missing"); ok {
		t.Error("stringFromPayload(missing) ok=true, want false")
	}
	if _, ok := stringFromPayload(m, "b"); ok {
		t.Error("stringFromPayload(b) ok=true on a non-string, want false")
	}
	if !boolFromPayload(m, "b") {
		t.Error("boolFromPayload(b) = false, want true")
	}
	if boolFromPayload(m, "missing") {
		t.Error("boolFromPayload(missing) = true, want false")
	}
	if v, ok := intFromPayload(m, "i"); !ok || v != 10 {
		t.Errorf("intFromPayload(i) = %d,%v", v, ok)
	}
	if v, ok := intFromPayload(m, "iInt"); !ok || v != 7 {
		t.Errorf("intFromPayload(iInt) = %d,%v", v, ok)
	}
	if _, ok := intFromPayload(m, "frac"); ok {
		t.Error("intFromPayload(frac) ok=true on a non-integral float, want false")
	}
	if _, ok := intFromPayload(m, "wrong"); ok {
		t.Error("intFromPayload(wrong) ok=true on a non-number, want false")
	}
	if _, ok := intFromPayload(nil, "x"); ok {
		t.Error("intFromPayload(nil) ok=true, want false")
	}
}

func TestClassifyApplyErr(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{ErrNoOutstandingPause, "no_outstanding_pause"},
		{ErrRunLoopMisconfigured, "misconfigured"},
		{ErrPayloadInvalid, "payload_invalid"},
		{pauseresume.ErrAlreadyResumed, "already_resumed"},
		{pauseresume.ErrScopeMismatch, "scope_mismatch"},
		{pauseresume.ErrPauseNotFound, "pause_not_found"},
		{errors.New("something else"), "apply_failed"},
	}
	for _, c := range cases {
		if got := classifyApplyErr(c.err); got != c.want {
			t.Errorf("classifyApplyErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
