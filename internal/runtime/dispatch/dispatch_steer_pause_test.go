// internal/runtime/dispatch/dispatch_steer_pause_test.go — _steer_task /
// _pause_task / _resume_task dispatch tests: descendant-scoping via the
// reused isOwnDescendant guard, routing onto the descendant's EXISTING
// per-sub-run steering inbox (the same inbox the operator's steering
// targets) + the unified pause/resume primitive's ControlPause /
// ControlResume entry points, idempotent-on-terminal outcomes, the
// fail-loud pause-payload serialization contract, human-supremacy +
// cross-sibling isolation under one session, and D-025 concurrent reuse.
//
// Real drivers everywhere on the seam (CLAUDE.md §17.3): a real inprocess
// TaskRegistry over an inmem StateStore + a real steering.Registry (the
// production inbox registry). The dispatch package IS the
// planner→steering wiring boundary (AGENTS.md §17.2 in-package carve-out),
// so the human-supremacy + cross-run isolation test lives here against the
// production drivers.

package dispatch

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// newSteerExecutor builds an executor wired with a steering Registry so
// the steer/pause/resume verbs can resolve a descendant's inbox.
func newSteerExecutor(t *testing.T, reg tasks.TaskRegistry, sreg *steering.Registry, maxDepth int) steering.ToolExecutor {
	t.Helper()
	return NewToolExecutor(tools.NewCatalog(), newTestArtifactStore(t), reg,
		WithHeavyThreshold(32*1024),
		WithMaxSpawnDepth(maxDepth),
		WithSteeringRegistry(sreg))
}

// openDescendantInbox opens the per-sub-run steering inbox for a
// descendant task under the shared test session (RunID == the task id at
// the dev layer) and returns it.
func openDescendantInbox(t *testing.T, sreg *steering.Registry, child tasks.TaskID) *steering.Inbox {
	t.Helper()
	inbox, err := sreg.Open(identity.Quadruple{Identity: dispatchTestID, RunID: string(child)})
	if err != nil {
		t.Fatalf("open descendant inbox for %q: %v", child, err)
	}
	t.Cleanup(func() {
		_ = sreg.Retire(identity.Quadruple{Identity: dispatchTestID, RunID: string(child)})
	})
	return inbox
}

// TestExecutor_SteerTask_NilSteeringRegistry_Unsupported — with no
// steering Registry wired, SteerTask fails loud (never panic / silent
// no-op).
func TestExecutor_SteerTask_NilSteeringRegistry_Unsupported(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4) // no steering registry
	_, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.SteerTask{TaskID: "x", Directive: "go"})
	if !errors.Is(err, steering.ErrDecisionShapeUnsupported) {
		t.Fatalf("nil-steering SteerTask err = %v, want ErrDecisionShapeUnsupported", err)
	}
}

// TestExecutor_PauseResume_NilSteeringRegistry_Unsupported — same for
// PauseTask / ResumeTask.
func TestExecutor_PauseResume_NilSteeringRegistry_Unsupported(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4)
	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.PauseTask{TaskID: "x"}); !errors.Is(err, steering.ErrDecisionShapeUnsupported) {
		t.Fatalf("nil-steering PauseTask err = %v, want ErrDecisionShapeUnsupported", err)
	}
	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.ResumeTask{TaskID: "x"}); !errors.Is(err, steering.ErrDecisionShapeUnsupported) {
		t.Fatalf("nil-steering ResumeTask err = %v, want ErrDecisionShapeUnsupported", err)
	}
}

// TestExecutor_SteerTask_OwnDescendant_EnqueuesDirective — AC-8: steering
// an own descendant enqueues the directive onto that descendant's EXISTING
// steering inbox as an INJECT_CONTEXT control and returns {steered:true}.
func TestExecutor_SteerTask_OwnDescendant_EnqueuesDirective(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	sreg := steering.NewRegistry()
	exec := newSteerExecutor(t, reg, sreg, 4)

	child := spawnChildUnder(t, exec, "runA", "child")
	inbox := openDescendantInbox(t, sreg, child)

	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.SteerTask{TaskID: child, Directive: "focus on auth"})
	if err != nil {
		t.Fatalf("SteerTask: %v", err)
	}
	m := raw.(map[string]any)
	if m["steered"] != true {
		t.Fatalf("steered = %v, want true", m["steered"])
	}
	if m["task_id"] != string(child) {
		t.Fatalf("task_id = %v, want %q", m["task_id"], child)
	}

	events, dErr := inbox.Drain()
	if dErr != nil {
		t.Fatalf("Drain: %v", dErr)
	}
	if len(events) != 1 {
		t.Fatalf("inbox events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Type != steering.ControlInjectContext {
		t.Errorf("control type = %q, want INJECT_CONTEXT", ev.Type)
	}
	if ev.Payload["directive"] != "focus on auth" {
		t.Errorf("payload directive = %v, want 'focus on auth'", ev.Payload["directive"])
	}
	if ev.Identity.RunID != string(child) {
		t.Errorf("control targeted RunID %q, want the descendant %q", ev.Identity.RunID, child)
	}
}

// TestExecutor_SteerTask_TerminalDescendant_Idempotent — AC-8: steering a
// descendant whose inbox has been retired (its run ended) returns
// {steered:false}, not an error.
func TestExecutor_SteerTask_TerminalDescendant_Idempotent(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	sreg := steering.NewRegistry()
	exec := newSteerExecutor(t, reg, sreg, 4)

	child := spawnChildUnder(t, exec, "runA", "child")
	// Deliberately do NOT open an inbox — the descendant's run has ended,
	// its inbox is retired / never live.
	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.SteerTask{TaskID: child, Directive: "too late"})
	if err != nil {
		t.Fatalf("SteerTask(terminal) err = %v, want nil (idempotent-on-terminal)", err)
	}
	if raw.(map[string]any)["steered"] != false {
		t.Errorf("steered = %v, want false on retired inbox", raw.(map[string]any)["steered"])
	}
}

// TestExecutor_PauseResume_OwnDescendant_RoundTrips — AC-9: pausing then
// resuming an own descendant enqueues ControlPause then ControlResume onto
// that descendant's inbox (the unified pause/resume primitive's entry
// points), carrying the reason / resume directive; each returns done:true.
func TestExecutor_PauseResume_OwnDescendant_RoundTrips(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	sreg := steering.NewRegistry()
	exec := newSteerExecutor(t, reg, sreg, 4)

	child := spawnChildUnder(t, exec, "runA", "child")
	inbox := openDescendantInbox(t, sreg, child)

	praw, _, pErr := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.PauseTask{TaskID: child, Reason: "hold"})
	if pErr != nil {
		t.Fatalf("PauseTask: %v", pErr)
	}
	if praw.(map[string]any)["paused"] != true {
		t.Fatalf("paused = %v, want true", praw.(map[string]any)["paused"])
	}

	rraw, _, rErr := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.ResumeTask{TaskID: child, Directive: "carry on"})
	if rErr != nil {
		t.Fatalf("ResumeTask: %v", rErr)
	}
	if rraw.(map[string]any)["resumed"] != true {
		t.Fatalf("resumed = %v, want true", rraw.(map[string]any)["resumed"])
	}

	events, dErr := inbox.Drain()
	if dErr != nil {
		t.Fatalf("Drain: %v", dErr)
	}
	if len(events) != 2 {
		t.Fatalf("inbox events = %d, want 2 (pause then resume)", len(events))
	}
	if events[0].Type != steering.ControlPause {
		t.Errorf("event 0 type = %q, want PAUSE", events[0].Type)
	}
	if events[0].Payload["reason"] != "hold" {
		t.Errorf("pause reason = %v, want 'hold'", events[0].Payload["reason"])
	}
	if events[1].Type != steering.ControlResume {
		t.Errorf("event 1 type = %q, want RESUME", events[1].Type)
	}
	if events[1].Payload["directive"] != "carry on" {
		t.Errorf("resume directive = %v, want 'carry on'", events[1].Payload["directive"])
	}
}

// TestExecutor_PauseTask_DoesNotPauseParentRun — the pause/resume-vs-parent
// risk guard: pausing a descendant enqueues ONLY onto the descendant's
// inbox, never the issuing run's own inbox — the parent turn continues.
func TestExecutor_PauseTask_DoesNotPauseParentRun(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	sreg := steering.NewRegistry()
	exec := newSteerExecutor(t, reg, sreg, 4)

	// Open the PARENT run's own inbox so we can assert nothing lands on it.
	parentInbox, err := sreg.Open(identity.Quadruple{Identity: dispatchTestID, RunID: "runA"})
	if err != nil {
		t.Fatalf("open parent inbox: %v", err)
	}
	t.Cleanup(func() { _ = sreg.Retire(identity.Quadruple{Identity: dispatchTestID, RunID: "runA"}) })

	child := spawnChildUnder(t, exec, "runA", "child")
	childInbox := openDescendantInbox(t, sreg, child)

	if _, _, pErr := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.PauseTask{TaskID: child, Reason: "hold"}); pErr != nil {
		t.Fatalf("PauseTask: %v", pErr)
	}
	if parentInbox.Len() != 0 {
		t.Fatalf("parent run inbox got %d events — pausing a descendant must never pause the parent", parentInbox.Len())
	}
	if childInbox.Len() != 1 {
		t.Fatalf("descendant inbox got %d events, want 1", childInbox.Len())
	}
}

// TestExecutor_SteerPauseResume_EmptyTaskID_FailsLoud — an empty target
// fails loud rather than reaching the inbox.
func TestExecutor_SteerPauseResume_EmptyTaskID_FailsLoud(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	sreg := steering.NewRegistry()
	exec := newSteerExecutor(t, reg, sreg, 4)

	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.SteerTask{Directive: "d"}); err == nil {
		t.Error("SteerTask with empty TaskID must fail loud")
	}
	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.PauseTask{}); err == nil {
		t.Error("PauseTask with empty TaskID must fail loud")
	}
	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.ResumeTask{}); err == nil {
		t.Error("ResumeTask with empty TaskID must fail loud")
	}
}

// TestExecutor_SteerTask_Self_NotDescendant — a run targeting its own task
// id is rejected (self is not a descendant), mirroring _cancel_task.
func TestExecutor_SteerTask_Self_NotDescendant(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	sreg := steering.NewRegistry()
	exec := newSteerExecutor(t, reg, sreg, 4)

	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.SteerTask{TaskID: "runA", Directive: "d"}); !errors.Is(err, ErrTaskNotOwnDescendant) {
		t.Fatalf("self-steer err = %v, want ErrTaskNotOwnDescendant", err)
	}
}

// TestExecutor_PauseResume_AgentPayloadSerializationGuard — §5/§11: the
// fail-loud serialization guard on the AGENT-supplied pause/resume control
// payload (the {source, issuer_run, reason|directive} map this dispatch
// edge constructs). A non-JSON-encodable leaf surfaces
// trajectory.ErrUnserializable naming the offending field path — never a
// silent drop. validatePausePayload IS the gate pauseTask / resumeTask run
// before enqueuing onto the descendant's inbox.
//
// Scope (the honest boundary — NOT the descendant's run state): this guard
// covers only the payload THIS edge builds. The descendant's own run-state
// serialization contract (trajectory.ErrUnserializable on the checkpointed
// trajectory) is enforced DOWNSTREAM, unchanged, by the unified primitive
// inside the descendant's RunLoop (pauseresume.Coordinator.Request) — that
// path is covered by the pauseresume package's own contract tests, not
// reachable through this parent verb. Because every field this edge emits
// is an agent-supplied string/literal, the real projector→dispatch path
// cannot itself produce an unencodable leaf; this test drives one directly
// to prove the guard is wired and fails loud, not to claim the real path
// can trip it.
func TestExecutor_PauseResume_AgentPayloadSerializationGuard(t *testing.T) {
	t.Parallel()
	bad := map[string]any{
		"reason":   "hold",
		"callback": func() {}, // non-encodable leaf (cannot arise from the real path)
	}
	err := validatePausePayload(bad)
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("validatePausePayload err = %v, want trajectory.ErrUnserializable (the fail-loud agent-payload guard)", err)
	}
	if unserr.Field == "" {
		t.Error("ErrUnserializable.Field is empty — the contract requires the offending field path be named")
	}
	// The realistic agent payloads (all strings) pass — the guard is not a
	// blanket reject and never trips through the real dispatch path.
	if gErr := validatePausePayload(map[string]any{"reason": "hold", "source": "agent", "issuer_run": "runA"}); gErr != nil {
		t.Fatalf("validatePausePayload(realistic agent payload) = %v, want nil", gErr)
	}
	if gErr := validatePausePayload(map[string]any{"directive": "carry on", "source": "agent", "issuer_run": "runA"}); gErr != nil {
		t.Fatalf("validatePausePayload(realistic resume payload) = %v, want nil", gErr)
	}
}

// TestExecutor_ResumeTask_NotPausedDescendant_ParentSuccessDecoupled — W3:
// a redundant _resume_task on a LIVE descendant that this run never paused
// returns resumed:true (the parent-side "control enqueued to a live task"
// signal) and enqueues exactly one RESUME control onto that descendant's
// inbox. The parent's resumed:true is DECOUPLED from the descendant's
// downstream handling: the dispatch edge does not inspect the descendant's
// pause state. When the descendant's own RunLoop later drains that RESUME
// with no outstanding pause it surfaces ErrNoOutstandingPause and ends that
// run loud — the SAME inherited outcome an operator's mistaken resume
// produces (covered end-to-end by steering's
// TestApplyEvent_Resume_NoOutstandingPause_FailsLoud). This test pins the
// honest decoupling at the dispatch edge; it deliberately does NOT claim
// the descendant survives a spurious resume (it does not — that is the
// inherited pause/resume-primitive semantics, not a new mechanism).
func TestExecutor_ResumeTask_NotPausedDescendant_ParentSuccessDecoupled(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	sreg := steering.NewRegistry()
	exec := newSteerExecutor(t, reg, sreg, 4)

	child := spawnChildUnder(t, exec, "runA", "child")
	inbox := openDescendantInbox(t, sreg, child)

	// The descendant was never paused. A resume still reports resumed:true
	// (control delivered to a live inbox) — the parent cannot tell whether a
	// valid pause was released.
	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.ResumeTask{TaskID: child, Directive: "carry on"})
	if err != nil {
		t.Fatalf("ResumeTask(not-paused): %v", err)
	}
	if raw.(map[string]any)["resumed"] != true {
		t.Fatalf("resumed = %v, want true (control enqueued to a live descendant, decoupled from its pause state)", raw.(map[string]any)["resumed"])
	}

	// Exactly one RESUME control landed — the control the descendant's own
	// RunLoop would then apply through the unified primitive (where a
	// no-outstanding-pause resume ends that run loud, per the steering
	// package's own contract test).
	events, dErr := inbox.Drain()
	if dErr != nil {
		t.Fatalf("Drain: %v", dErr)
	}
	if len(events) != 1 || events[0].Type != steering.ControlResume {
		t.Fatalf("descendant inbox = %+v, want exactly one RESUME control", events)
	}
}

// TestExecutor_SteerPauseResume_HumanSupremacy_CrossRunIsolation — AC-12:
// two sibling runs in the SAME (tenant, user, session). Run A spawns a
// descendant; run B calls _steer_task / _pause_task / _resume_task naming
// run A's descendant — all three fail loud with ErrTaskNotOwnDescendant,
// and run A's descendant inbox is untouched. The operator's EXISTING
// control surface (a direct admin-scoped Enqueue) CAN reach that same
// descendant (human supremacy). Run A's OWN pause→resume of its own
// descendant round-trips.
func TestExecutor_SteerPauseResume_HumanSupremacy_CrossRunIsolation(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	sreg := steering.NewRegistry()
	exec := newSteerExecutor(t, reg, sreg, 4)

	// Run A spawns a descendant under its own run (same session identity).
	aChild := spawnChildUnder(t, exec, "runA", "run-A-work")
	inbox := openDescendantInbox(t, sreg, aChild)

	// Run B (same session, different run) cannot steer / pause / resume
	// run A's descendant.
	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runB"), planner.SteerTask{TaskID: aChild, Directive: "not mine"}); !errors.Is(err, ErrTaskNotOwnDescendant) {
		t.Fatalf("run B steer of run A's descendant err = %v, want ErrTaskNotOwnDescendant", err)
	}
	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runB"), planner.PauseTask{TaskID: aChild}); !errors.Is(err, ErrTaskNotOwnDescendant) {
		t.Fatalf("run B pause of run A's descendant err = %v, want ErrTaskNotOwnDescendant", err)
	}
	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runB"), planner.ResumeTask{TaskID: aChild}); !errors.Is(err, ErrTaskNotOwnDescendant) {
		t.Fatalf("run B resume of run A's descendant err = %v, want ErrTaskNotOwnDescendant", err)
	}

	// Run A's descendant inbox is untouched by run B's rejected attempts.
	if inbox.Len() != 0 {
		t.Fatalf("run A's descendant inbox got %d events from rejected run B attempts — isolation violated", inbox.Len())
	}

	// Human supremacy: the operator's EXISTING control surface (an
	// admin-scoped ControlEvent enqueued directly onto the inbox — the
	// same surface the Protocol control edge uses) CAN pause run A's
	// descendant, from outside run A entirely.
	opErr := inbox.Enqueue(steering.ControlEvent{
		Type:         steering.ControlPause,
		Identity:     identity.Quadruple{Identity: dispatchTestID, RunID: string(aChild)},
		CallerScope:  steering.ScopeAdmin,
		CallerTenant: dispatchTestID.TenantID,
	})
	if opErr != nil {
		t.Fatalf("operator (admin) pause of the descendant failed: %v — human supremacy violated", opErr)
	}

	// Run A's OWN pause→resume of its own descendant round-trips.
	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.PauseTask{TaskID: aChild, Reason: "mine"}); err != nil {
		t.Fatalf("run A pause of its own descendant: %v", err)
	}
	if _, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA"), planner.ResumeTask{TaskID: aChild, Directive: "go"}); err != nil {
		t.Fatalf("run A resume of its own descendant: %v", err)
	}
	// The operator PAUSE + run A's own PAUSE + RESUME all landed.
	events, dErr := inbox.Drain()
	if dErr != nil {
		t.Fatalf("Drain: %v", dErr)
	}
	if len(events) != 3 {
		t.Fatalf("descendant inbox events = %d, want 3 (operator pause + own pause + own resume)", len(events))
	}
}

// TestExecutor_SteerPauseResume_ConcurrentReuse — AC-13 / D-025: N=100
// concurrent steer/pause/resume cycles against ONE shared executor + ONE
// shared registry + ONE shared steering Registry, each with its own
// identity and descendant, under -race. Asserts no data races, no context
// bleed (each cycle's controls land only on its own descendant's inbox),
// no cancellation cross-talk, and no goroutine leak.
func TestExecutor_SteerPauseResume_ConcurrentReuse(t *testing.T) {
	const n = 100

	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	sreg := steering.NewRegistry()
	exec := newSteerExecutor(t, reg, sreg, 4)

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
			runID := tasks.TaskID("run-" + strconv.Itoa(i))
			rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: id, RunID: string(runID)}}

			// Spawn a child under this goroutine's own run.
			raw, _, sErr := exec.ExecuteDecision(context.Background(), rc, planner.SpawnTask{
				Kind: tasks.KindBackground,
				Spec: planner.SpawnSpec{Query: "q-" + strconv.Itoa(i)},
			})
			if sErr != nil {
				errCh <- sErr
				return
			}
			child := tasks.TaskID(raw.(map[string]any)["task_id"].(string))

			// Open the child's per-sub-run inbox under this goroutine's
			// own identity (RunID == child at the dev layer).
			childQuad := identity.Quadruple{Identity: id, RunID: string(child)}
			inbox, oErr := sreg.Open(childQuad)
			if oErr != nil {
				errCh <- oErr
				return
			}

			// Steer, pause, resume the run's own child.
			if _, _, err := exec.ExecuteDecision(context.Background(), rc, planner.SteerTask{TaskID: child, Directive: "d-" + strconv.Itoa(i)}); err != nil {
				errCh <- err
				return
			}
			if _, _, err := exec.ExecuteDecision(context.Background(), rc, planner.PauseTask{TaskID: child}); err != nil {
				errCh <- err
				return
			}
			if _, _, err := exec.ExecuteDecision(context.Background(), rc, planner.ResumeTask{TaskID: child}); err != nil {
				errCh <- err
				return
			}

			// No context bleed: exactly this cycle's three controls landed
			// on its own child's inbox — never another run's.
			events, dErr := inbox.Drain()
			if dErr != nil {
				errCh <- dErr
				return
			}
			if len(events) != 3 {
				errCh <- errors.New("context bleed: descendant inbox did not carry exactly this cycle's 3 controls")
				return
			}
			_ = sreg.Retire(childQuad)
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent cycle: %v", err)
		}
	}

	if delta := runtime.NumGoroutine() - baseline; delta > 8 {
		t.Errorf("goroutine leak: delta=%d after %d cycles", delta, n)
	}
}
