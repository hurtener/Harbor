package steering

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tasks"
)

// RunLoop is Harbor's per-run planner-step loop — the runtime component
// that drives a planner.Planner to a terminal planner.Finish decision,
// draining the per-run steering Inbox between steps and routing pause
// decisions through the unified pauseresume.Coordinator.
//
// # Why this is the steering wiring
//
// Phase 52 shipped the steering primitive (the inbox, the nine-type
// taxonomy, ValidatePayload, CheckScope, the Registry). Phase 50 shipped
// the pause/resume primitive (the Coordinator). Neither did anything by
// itself — there was no run loop to drain the inbox or to route a
// RequestPause decision through the Coordinator. RunLoop IS that loop.
// It is the §13 first consumer of BOTH primitives, landing in the same
// wave (Wave 9, Stage 3) per CLAUDE.md §13 + D-067 §4 + D-070 §5.
//
// # The loop (brief 02 §4)
//
// Per run, RunLoop owns a tight loop:
//
//	Open the run's Inbox on the Registry
//	for step := 0; step < MaxSteps; step++ {
//	    drain the Inbox            -- once, at the step boundary
//	    apply each control event   -- CANCEL / PAUSE / REDIRECT / ... side effects
//	    project onto RunContext.Control  -- the planner sees ONLY this
//	    if a RESUME/APPROVE advanced a pause: clear the pause, continue
//	    if a REJECT advanced a pause: terminate Finish{ConstraintsConflict}
//	    decision := Planner.Next(ctx, runContext)
//	    switch decision {
//	        RequestPause -> Coordinator.Request; block; re-enter on RESUME/APPROVE
//	        Finish       -> Retire the Inbox; return
//	        other        -> (decision execution is a later-phase concern;
//	                         Phase 53 records the step and re-enters)
//	    }
//	}
//	Retire the Inbox  -- always, even on error
//
// The drain happens exactly ONCE per step boundary — never mid-tool-call
// (brief 02 §6). The planner observes the result via RunContext.Control;
// it never touches the Inbox (brief 02 §5 sharp-edge #2).
//
// # Concurrent reuse (D-025)
//
// RunLoop is a compiled artifact: every field is set once at construction
// (the Registry, the Coordinator, the applier's dependencies, the
// control-history ring, the clock — all immutable after NewRunLoop
// returns). There is NO per-run state on the struct: Run reads its
// run-specific data from ctx + the RunSpec argument, and the per-step
// accumulator (stepControl) lives on the run's own goroutine stack. One
// RunLoop is safe to share across N concurrent goroutines;
// concurrent_test.go pins N≥100 under -race.
type RunLoop struct {
	registry *Registry
	coord    pauseresume.Coordinator
	applier  *applier
	history  *controlHistory
	bus      events.EventBus // optional; nil ⇒ no lifecycle events emitted
	clock    Clock
}

// runLoopConfig is the option-applied construction config for a RunLoop.
type runLoopConfig struct {
	taskRegistry      tasks.TaskRegistry
	bus               events.EventBus
	hardCancelHook    func(ctx context.Context, runID string) error
	clock             Clock
	maxControlHistory int
}

// RunLoopOption configures a RunLoop at construction. Options are applied
// in order; later options override earlier ones for the same field.
type RunLoopOption func(*runLoopConfig)

// WithTaskRegistry hands the RunLoop a tasks.TaskRegistry. Required for
// the PRIORITIZE control event — a PRIORITIZE with no TaskRegistry fails
// loud (it cannot reach a task). Optional otherwise: the other eight
// control events do not touch the task registry.
func WithTaskRegistry(tr tasks.TaskRegistry) RunLoopOption {
	return func(c *runLoopConfig) {
		if tr != nil {
			c.taskRegistry = tr
		}
	}
}

// WithRunLoopBus hands the RunLoop an events.EventBus. When set, the
// RunLoop emits control.received (a control event was drained) and
// control.applied (its side effect was applied or failed). When NOT set,
// no lifecycle events are emitted — event emission is observability, not
// correctness.
func WithRunLoopBus(b events.EventBus) RunLoopOption {
	return func(c *runLoopConfig) {
		if b != nil {
			c.bus = b
		}
	}
}

// WithHardCancelHook wires the cancellation propagator a hard CANCEL
// fires. The hook is typically engine.Cancel(runID) — it propagates a
// cancellation context into an in-flight decision execution (brief 02
// §6). The RunLoop holds ONLY a func(ctx, runID) error, never a hard
// import of internal/runtime/engine — this keeps the step-loop family
// decoupled from the graph engine. A nil hook is tolerated: a hard
// CANCEL still sets Control.Cancelled (so the run terminates at the next
// boundary), the hook only accelerates an in-flight tool's teardown.
func WithHardCancelHook(fn func(ctx context.Context, runID string) error) RunLoopOption {
	return func(c *runLoopConfig) {
		if fn != nil {
			c.hardCancelHook = fn
		}
	}
}

// WithRunLoopClock overrides the RunLoop's time source — the Clock the
// applied-control history stamps AppliedAt from. Tests inject a
// controllable clock so no test sleeps for synchronisation (CLAUDE.md
// §11). The default is the real-time system clock.
func WithRunLoopClock(c Clock) RunLoopOption {
	return func(cfg *runLoopConfig) {
		if c != nil {
			cfg.clock = c
		}
	}
}

// WithMaxControlHistory overrides the per-session applied-control history
// cap. A non-positive value falls back to MaxControlHistory.
func WithMaxControlHistory(n int) RunLoopOption {
	return func(c *runLoopConfig) { c.maxControlHistory = n }
}

// NewRunLoop builds a RunLoop. The Registry (Phase 52 — owns the per-run
// inboxes the loop drains) and the Coordinator (Phase 50 — the ONE
// pause/resume primitive PAUSE / RESUME / APPROVE / REJECT converge on)
// are mandatory; a nil either fails loud with ErrRunLoopMisconfigured.
// Everything else is optional (see the WithXxx options).
//
// The returned RunLoop is immutable after construction (D-025) and safe
// for concurrent use by N goroutines.
func NewRunLoop(reg *Registry, coord pauseresume.Coordinator, opts ...RunLoopOption) (*RunLoop, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: Registry is nil", ErrRunLoopMisconfigured)
	}
	if coord == nil {
		return nil, fmt.Errorf("%w: Coordinator is nil", ErrRunLoopMisconfigured)
	}
	cfg := runLoopConfig{
		clock:             systemClock{},
		maxControlHistory: MaxControlHistory,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &RunLoop{
		registry: reg,
		coord:    coord,
		applier: &applier{
			coord:          coord,
			taskRegistry:   cfg.taskRegistry,
			hardCancelHook: cfg.hardCancelHook,
		},
		history: newControlHistory(cfg.maxControlHistory),
		bus:     cfg.bus,
		clock:   cfg.clock,
	}, nil
}

// DefaultMaxSteps is the planner-step cap RunLoop.Run applies when
// RunSpec.MaxSteps is ≤ 0. A run that has not reached a terminal Finish
// after this many steps terminates loud with ErrMaxStepsExceeded — an
// unbounded planner loop is a misconfiguration, never a silent spin.
const DefaultMaxSteps = 64

// RunSpec is the per-run input to RunLoop.Run. ALL run-specific state
// lives here + ctx — never on the RunLoop struct (D-025).
type RunSpec struct {
	// Planner is the swappable reasoning policy the loop drives. Nil
	// fails loud with ErrNoPlanner.
	Planner planner.Planner
	// Base is the run's RunContext template. RunLoop refreshes the
	// per-step fields (Control, Goal) on a copy each step; the planner
	// receives a fresh RunContext per Next call (Phase 42 contract).
	// Base.Quadruple is the run's identity — its triple is validated
	// identity-mandatory before the loop starts.
	Base planner.RunContext
	// TaskID is the run's task. Optional — when set, a PRIORITIZE
	// control event targets it; when empty, a PRIORITIZE fails loud.
	TaskID tasks.TaskID
	// MaxSteps caps the planner-step count. ≤ 0 ⇒ DefaultMaxSteps.
	MaxSteps int
}

// Run drives the planner to a terminal planner.Finish decision. It Opens
// the run's Inbox on the Registry, drives the drain-apply-project-Next
// loop, and Retires the Inbox on exit — ALWAYS, even on error (a leaked
// inbox would orphan a run's steering surface).
//
// Run fails closed:
//
//   - ErrNoPlanner — spec.Planner is nil.
//   - ErrIdentityRequired — spec.Base.Quadruple is an incomplete
//     quadruple (the per-run isolation gate, CLAUDE.md §6).
//   - ErrInboxExists — an Inbox is already open for this run quadruple.
//   - ErrMaxStepsExceeded — the planner did not reach a terminal Finish
//     within spec.MaxSteps steps.
//   - any wrapped planner / Coordinator / TaskRegistry error from a
//     step's Next call or a control event's side effect.
//
// On success Run returns the terminal planner.Finish the planner emitted.
func (rl *RunLoop) Run(ctx context.Context, spec RunSpec) (planner.Finish, error) {
	if spec.Planner == nil {
		return planner.Finish{}, ErrNoPlanner
	}
	q := spec.Base.Quadruple
	if err := validateQuadruple(q); err != nil {
		return planner.Finish{}, err
	}

	inbox, err := rl.registry.Open(q)
	if err != nil {
		return planner.Finish{}, fmt.Errorf("steering: opening run inbox: %w", err)
	}
	// Retire ALWAYS — a leaked inbox orphans the run's steering surface.
	defer func() {
		_ = rl.registry.Retire(q)
		// Forget the session's control history only when the inbox
		// retire succeeded for the LAST run in the session is a
		// later-phase concern; Phase 53 keeps the per-session ring until
		// the process ends (it is capped, so it is bounded regardless).
	}()

	maxSteps := spec.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}

	// runCtx carries the run's identity quadruple so Coordinator.Resume
	// (called from applyEvent) and Coordinator.Request (called below)
	// see the run's triple + run on their identity.From(ctx) pathway.
	runCtx := ctxWithIdentity(ctx, q)

	// outstandingToken is the run's current pause Token, "" when the run
	// is not paused. It is per-run loop state — it lives on this
	// goroutine's stack, never on the RunLoop struct (D-025).
	var outstandingToken pauseresume.Token

	for step := 0; step < maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return planner.Finish{}, fmt.Errorf("steering: run cancelled at step boundary: %w", err)
		}

		// While a pause is outstanding the planner must NOT be
		// re-entered (it would just re-emit RequestPause). Block —
		// without busy-spinning — until a steering control event
		// arrives (a RESUME / APPROVE / REJECT, or any other control).
		// The next drain applies it. WaitForEvent honours ctx so a
		// cancelled run unblocks loud.
		if outstandingToken != "" {
			if werr := inbox.WaitForEvent(ctx); werr != nil {
				return planner.Finish{}, fmt.Errorf("steering: waiting for resume control on a paused run: %w", werr)
			}
		}

		// --- DRAIN: once per step boundary, never mid-tool-call. ---
		drained, derr := inbox.Drain()
		if derr != nil {
			return planner.Finish{}, fmt.Errorf("steering: draining run inbox: %w", derr)
		}

		// --- APPLY: each drained control event's side effect. ---
		sc := &stepControl{}
		for _, ev := range drained {
			rl.emitLifecycle(runCtx, q, ev.Type, EventTypeControlReceived, "")
			applyErr := rl.applier.applyEvent(runCtx, sc, ev, outstandingToken)
			rl.history.record(q.SessionID, AppliedControl{
				Type:      ev.Type,
				RunID:     q.RunID,
				AppliedAt: rl.clock.Now(),
				Err:       applyErr,
			})
			rl.emitLifecycle(runCtx, q, ev.Type, EventTypeControlApplied, classifyApplyErr(applyErr))
			if applyErr != nil {
				// A failed side effect is surfaced loud — never swallowed
				// (CLAUDE.md §5). The history + control.applied event
				// already recorded it; Run returns the wrapped error.
				return planner.Finish{}, applyErr
			}
		}

		// A hard CANCEL fires the cancellation propagator. Done after the
		// drain loop so a single step's drained events are all applied
		// before the in-flight execution is torn down.
		if sc.hardCancel {
			if err := rl.applier.hardCancel(runCtx, q.RunID); err != nil {
				return planner.Finish{}, err
			}
		}

		// A PRIORITIZE reaches the TaskRegistry once the run's TaskID is
		// in scope (the RunSpec carries it).
		if sc.prioritizeSet {
			if err := rl.applier.prioritize(runCtx, spec.TaskID, sc.prioritizeVal); err != nil {
				return planner.Finish{}, err
			}
		}

		// A REJECT that advanced a pause terminates the run: a rejected
		// HITL gate is a constraint conflict the planner cannot resolve
		// (D-071). The Coordinator.Resume already happened in applyEvent.
		if sc.resumeRequested && sc.resumeKind == ControlReject {
			outstandingToken = ""
			return planner.Finish{
				Reason: planner.FinishConstraintsConflict,
				Metadata: map[string]any{
					"run_id":          q.RunID,
					"rejected_by":     "steering",
					"steering_reason": "control_reject",
				},
			}, nil
		}
		// A RESUME / APPROVE that advanced a pause clears the outstanding
		// Token; the loop falls through to Planner.Next and the planner
		// re-enters.
		if sc.resumeRequested && (sc.resumeKind == ControlResume || sc.resumeKind == ControlApprove) {
			outstandingToken = ""
		}

		// If a pause is STILL outstanding after the drain (the drained
		// events were INJECT_CONTEXT / REDIRECT / USER_MESSAGE / etc. —
		// no RESUME / APPROVE / REJECT), the planner must NOT be
		// re-entered: it would just re-emit RequestPause. Loop back to
		// WaitForEvent. The non-resume controls were still applied —
		// their side effects (an injected context, a redirected goal)
		// accumulate onto the run's base RunContext and the next step
		// after the eventual resume sees them.
		//
		// EXCEPT a CANCEL: a CANCEL that arrives while a run is paused
		// terminates the run — there is no point waiting for a resume
		// that will never come. The pause record is left for the
		// Coordinator's own GC / restart logic; the run loop exits with
		// Finish{Cancelled}.
		if outstandingToken != "" {
			if sc.signals.Cancelled {
				return planner.Finish{
					Reason: planner.FinishCancelled,
					Metadata: map[string]any{
						"run_id":          q.RunID,
						"steering_reason": "cancel_while_paused",
					},
				}, nil
			}
			mergeAccumulatedSignals(&spec.Base, sc)
			continue
		}

		// --- PROJECT: the planner sees ONLY RunContext.Control. ---
		rc := spec.Base // value copy — the planner gets a fresh RunContext per step
		// Fold any carry-over signals (accumulated while a pause was
		// outstanding — see mergeAccumulatedSignals) into this step's
		// freshly-drained signals so nothing is lost across a pause.
		rc.Control = mergeSignals(spec.Base.Control, sc.signals)
		// The carry-over has now been handed to the planner — clear it
		// from the base so the NEXT step does not re-deliver it.
		spec.Base.Control = planner.ControlSignals{}
		if sc.goal != "" {
			rc.Goal = sc.goal
			// Persist the redirected goal into the run's base so a later
			// step (after a non-REDIRECT drain) still sees it.
			spec.Base.Goal = sc.goal
		}

		// --- NEXT: the planner contributes exactly this. ---
		decision, nerr := spec.Planner.Next(runCtx, rc)
		if nerr != nil {
			return planner.Finish{}, fmt.Errorf("steering: planner step %d: %w", step, nerr)
		}
		if decision == nil {
			// (nil, nil) is the silent-degradation shape §13 forbids.
			return planner.Finish{}, fmt.Errorf("steering: planner step %d returned a nil Decision (silent degradation forbidden — CLAUDE.md §13)", step)
		}

		// --- EXECUTE the decision. ---
		switch d := decision.(type) {
		case planner.Finish:
			return d, nil

		case planner.RequestPause:
			// Route the planner's RequestPause through the ONE
			// Coordinator (CLAUDE.md §7 rule 4). This is the §13
			// end-to-end consumer path: RequestPause -> Coordinator.Request
			// -> Token (+ durable checkpoint when a store is configured)
			// -> the loop blocks at this boundary -> an APPROVE / RESUME
			// control event arrives via the Phase 52 inbox -> the next
			// step's drain applies it -> Coordinator.Resume -> the
			// planner re-enters.
			tok, perr := rl.requestPause(runCtx, q, d)
			if perr != nil {
				return planner.Finish{}, perr
			}
			outstandingToken = tok
			// The loop continues: the next iteration drains the inbox
			// and, when a RESUME / APPROVE has arrived, applyEvent calls
			// Coordinator.Resume and outstandingToken is cleared. Until
			// then the loop simply re-drains — the planner is NOT
			// re-entered while a pause is outstanding (the planner would
			// just re-emit RequestPause).
			//
			// Re-emit guard: if the planner re-emits RequestPause while a
			// pause is already outstanding, do not Request a second
			// Token — the existing pause stands.

		default:
			// CallTool / CallParallel / SpawnTask / AwaitTask — the
			// runtime decision-execution layer (a later phase) dispatches
			// these. Phase 53's scope is the steering + pause wiring; it
			// records the step boundary and re-enters the planner. A
			// production RunLoop wired with a decision executor would
			// dispatch here; Phase 53's tests use planners whose step
			// sets reach a Finish (or a RequestPause) so the loop
			// terminates deterministically.
		}
	}

	return planner.Finish{}, fmt.Errorf("%w: %d steps", ErrMaxStepsExceeded, maxSteps)
}

// requestPause routes a planner's RequestPause decision through the
// unified Coordinator. It maps the planner-side PauseReason onto the
// pauseresume.Reason (the typedef bridge keeps them byte-identical) and
// hands the trajectory through so a checkpoint-store-backed Coordinator
// can persist it. A pauseresume error (ErrUnserializable from a
// non-serialisable payload / trajectory, ErrInvalidReason) propagates
// verbatim — no silent degradation.
func (rl *RunLoop) requestPause(ctx context.Context, q identity.Quadruple, d planner.RequestPause) (pauseresume.Token, error) {
	req := pauseresume.PauseRequest{
		Identity:   q.Identity,
		Reason:     d.Reason, // Reason is `= planner.PauseReason` — same type
		Payload:    d.Payload,
		Trajectory: nil, // the run's Trajectory wiring is a later-phase concern; Phase 53 routes the decision, not the trajectory snapshot
	}
	pause, err := rl.coord.Request(ctx, req)
	if err != nil {
		return "", fmt.Errorf("steering: routing RequestPause through Coordinator.Request: %w", err)
	}
	return pause.Token, nil
}

// emitLifecycle publishes a control.received / control.applied event
// when a bus is configured. A publish failure is swallowed deliberately:
// lifecycle event emission is observability, not correctness — a failed
// control.received emit must not unwind an otherwise-correct drain. (This
// is NOT silent degradation of a correctness path: the control event was
// drained and applied; only the best-effort notification was lost. The
// applied-control history — the durable audit trail — still recorded it.)
func (rl *RunLoop) emitLifecycle(ctx context.Context, q identity.Quadruple, t ControlType, evType events.EventType, errStr string) {
	if rl.bus == nil {
		return
	}
	outcome := outcomeReceived
	if evType == EventTypeControlApplied {
		if errStr == "" {
			outcome = outcomeApplied
		} else {
			outcome = outcomeFailed
		}
	}
	_ = rl.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: q,
		Payload: ControlLifecyclePayload{
			Type:    string(t),
			Outcome: outcome,
			Err:     errStr,
		},
	})
}

// ControlHistory returns a copy of a session's applied-control history,
// oldest-to-newest. Primarily for observability + tests; the Protocol
// edge (Phase 54) projects this as the session's steering audit trail.
func (rl *RunLoop) ControlHistory(sessionID string) []AppliedControl {
	return rl.history.snapshot(sessionID)
}

// compile-time assertion: the RunLoop's pause-routing relies on the
// pauseresume.Reason typedef bridge being byte-identical to
// planner.PauseReason. If that bridge is ever re-typed, this fails to
// compile and the requestPause mapping must be revisited.
var _ pauseresume.Reason = planner.PauseReason("")

// errMaxStepsIs is a tiny helper kept so callers can errors.Is-test the
// max-steps terminal without importing the sentinel name awkwardly.
// (Unexported; used by runloop_test.go.)
func errMaxStepsIs(err error) bool { return errors.Is(err, ErrMaxStepsExceeded) }

// mergeSignals folds carry-over signals (accumulated while a pause was
// outstanding) into this step's freshly-drained signals. The boolean
// signals OR; the slices concatenate (carry-over first, preserving FIFO
// order); RedirectGoal prefers the fresher value when both are set.
func mergeSignals(carry, fresh planner.ControlSignals) planner.ControlSignals {
	out := fresh
	out.Cancelled = carry.Cancelled || fresh.Cancelled
	out.PauseRequested = carry.PauseRequested || fresh.PauseRequested
	if len(carry.InjectedContext) > 0 {
		out.InjectedContext = append(append([]map[string]any{}, carry.InjectedContext...), fresh.InjectedContext...)
	}
	if len(carry.UserMessages) > 0 {
		out.UserMessages = append(append([]string{}, carry.UserMessages...), fresh.UserMessages...)
	}
	if out.RedirectGoal == "" {
		out.RedirectGoal = carry.RedirectGoal
	}
	return out
}

// mergeAccumulatedSignals persists the side effects of non-resume
// control events (INJECT_CONTEXT / REDIRECT / USER_MESSAGE / CANCEL /
// PAUSE) applied while a pause is outstanding onto the run's base
// RunContext, so they survive the WaitForEvent block and reach the
// planner on the step after the eventual RESUME / APPROVE. Without this,
// a context injected during a pause would be silently dropped.
func mergeAccumulatedSignals(base *planner.RunContext, sc *stepControl) {
	base.Control = mergeSignals(base.Control, sc.signals)
	if sc.goal != "" {
		base.Goal = sc.goal
	}
}
