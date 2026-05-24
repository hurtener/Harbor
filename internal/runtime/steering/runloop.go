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
	"github.com/hurtener/Harbor/internal/tools/approval"
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
	gates             map[string]*approval.ApprovalGate
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

// WithApprovalGates hands the RunLoop the catalog-applied approval gates
// keyed by tool name (Phase 64a `applyToolCatalogWiring` produces this
// map via the `Deps.AppliedGates` out-channel — D-090, D-097). When a
// drained CONTROL_APPROVE / CONTROL_REJECT event references a `token`
// the bridge tries each gate's `ResolveApproval` in turn; the gate that
// owns the token resumes its `pending` waiter so the wrapped tool's
// `Invoke` unblocks. When no gate owns the token (a plain RESUME or an
// OAuth-pause APPROVE), the apply path falls back to the direct
// `Coordinator.Resume`. A nil / empty map disables the bridge — the
// loop behaves exactly as before D-097 (direct Resume only). See
// `applier.advancePause` for the routing.
//
// Coupling note (acceptable; D-097): `internal/runtime/steering`
// imports `internal/tools/approval` for the gate type. Both packages
// are runtime mechanism — the boundary is acceptable because the
// bridge IS the runtime-side wiring the gate needs to receive
// wire-side decisions.
func WithApprovalGates(gates map[string]*approval.ApprovalGate) RunLoopOption {
	return func(c *runLoopConfig) {
		// Nil/empty is tolerated: the bridge is inert when no gates
		// are wired. A boot that registers zero approval-gated tools
		// stays correct.
		c.gates = gates
	}
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
			gates:          cfg.gates,
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

// ToolExecutor is the runtime-side dispatch surface the RunLoop calls
// when the planner returns a non-Finish, non-RequestPause decision
// (CallTool, CallParallel, SpawnTask, AwaitTask). The executor:
//
//   - Looks up the tool descriptor by name.
//   - Invokes it under the run's identity-scoped ctx.
//   - Returns a planner-readable observation (the runtime appends it
//     onto trajectory.Step.Observation for the planner's next step).
//
// Phase 83i (D-152) introduces this seam so the dev binary can wire
// a real `tools.ToolCatalog`-backed executor; before 83i the runloop's
// `default:` case dropped every CallTool on the floor (Phase 53's
// punted scope), which made multi-step ReAct structurally broken
// against real LLMs because the planner saw the same trajectory on
// every step.
//
// An executor that does not support a given decision shape returns
// ErrDecisionShapeUnsupported with a message naming the unsupported
// shape — the runloop surfaces this as the step's observation so the
// planner can choose a different path (repair, finish, alternative tool).
type ToolExecutor interface {
	// ExecuteDecision dispatches `decision` and returns BOTH the raw
	// observation (preserved for inspect-runs / audit) AND the
	// projection the next prompt sees (`llmObservation`, the D-026
	// heavy-content-discipline projection: a small summary +
	// ArtifactRef when the raw result is over the heavy threshold,
	// or just == raw when the result is small enough to inline).
	//
	// The runloop appends a trajectory.Step{Action: decision,
	// Observation: raw, LLMObservation: projection} so the planner's
	// renderer (Phase 46 / D-055) sees only the projection.
	//
	// `rc` is the per-step RunContext (identity, ToolContext, etc.).
	// ctx is the per-step ctx; the executor MUST honour cancellation.
	ExecuteDecision(ctx context.Context, rc planner.RunContext, decision planner.Decision) (observation, llmObservation any, err error)
}

// ErrDecisionShapeUnsupported — returned by ToolExecutor implementations
// for decision shapes the executor does not yet dispatch (e.g. the
// dev binary's V1.1 executor handles CallTool only; CallParallel /
// SpawnTask / AwaitTask need their own dispatcher layers). The runloop
// records the error as the step's observation so the planner sees
// "this didn't run" and can re-plan.
var ErrDecisionShapeUnsupported = errors.New("steering: ToolExecutor does not support this decision shape")

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

	// ToolExecutor dispatches the planner's non-Finish, non-RequestPause
	// decisions (CallTool, CallParallel, SpawnTask, AwaitTask). Phase
	// 83i (D-152): when nil, the runloop's default case logs and
	// appends an empty-observation step (the Phase 53 behaviour) so
	// existing pause/steering tests still drive deterministic finishes.
	// In production the dev binary wires a real executor backed by the
	// tool catalog so the planner's CallTool decisions actually run.
	ToolExecutor ToolExecutor

	// OnToolDispatched is the optional per-run hook the runloop
	// invokes after the ToolExecutor returns WITHOUT ERROR (Phase 83m
	// item 7). The dev binary wires it to
	// `taskReg.IncrementToolCount(ctx, taskID)` so the Console Tasks
	// page reflects the per-task tool-dispatch count. A nil hook is
	// the legacy / test path (no counter wired); a hook that errors
	// fails the run loud — silent degradation of an observability
	// counter is forbidden per §13 (the counter is an integrity
	// surface, not a best-effort log line).
	//
	// The runloop calls the hook for every successful executor
	// dispatch — CallTool today, CallParallel + SpawnTask + AwaitTask
	// once those executor shapes land. A dispatch that the executor
	// reports as failed (the executor's own error path) does NOT
	// invoke the hook; the planner's repair / re-plan flow records
	// the failure on the trajectory and the counter stays put.
	OnToolDispatched func(ctx context.Context) error
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
	// The per-session control-history ring is intentionally NOT forgotten
	// here: run-end is the wrong signal (a session hosts multiple runs).
	// Wiring controlHistory.forget to a real session-end signal is
	// tracked in issue #79; each ring is capped so the per-session entry
	// is bounded, only the session-keyed map grows. Accepted V1 limit — D-071.
	defer func() {
		_ = rl.registry.Retire(q) //nolint:errcheck // best-effort cleanup on run-end; a Retire error must not mask the run result
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

	for step := range maxSteps {
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

		// Phase 83m item 8: per-step closure that captures the planner's
		// reasoning trace via the RunContext.OnReasoning side-channel.
		// The runloop reads stepReasoning after Planner.Next returns and
		// copies it into the appended trajectory.Step. The closure is
		// scoped to THIS step (one captured variable per iteration); a
		// new closure is installed each step so a stale read from a
		// prior step never reaches the next append. The capture lives
		// on this goroutine's stack — D-025 holds (no planner-side
		// mutable state).
		var stepReasoning string
		rc.OnReasoning = func(s string) { stepReasoning = s }

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
			// CallTool / CallParallel / SpawnTask / AwaitTask. Phase 83i
			// (D-152): dispatch via spec.ToolExecutor when present, then
			// append a trajectory.Step the planner sees on its next step.
			// Without the trajectory append the planner repeats the same
			// prompt forever (the failure mode the audit pinned in
			// production). Without the dispatch the operator gets a
			// "planner-only" loop that never actually does work. Both
			// are V1.1 blockers.
			//
			// When spec.ToolExecutor is nil (the Phase 53 dev / legacy
			// test path), the step still gets appended with a nil
			// Observation so the planner sees its decision did NOT
			// silently disappear (audit lesson: silent execution gaps
			// are §13-forbidden silent degradation).
			var observation, llmObservation any
			if spec.ToolExecutor != nil {
				obs, llmObs, execErr := spec.ToolExecutor.ExecuteDecision(runCtx, rc, decision)
				if execErr != nil {
					// Fail-loud per CLAUDE.md §5 / §13: the executor's
					// own error path (catalog lookup failed, tool Invoke
					// returned an error, decision shape unsupported) is
					// surfaced as the step's observation so the planner
					// can re-plan. The runloop does NOT abort the run
					// on a single tool error — that's the planner's
					// call (it may repair, try another tool, or finish).
					errPayload := map[string]any{"error": execErr.Error()}
					observation = errPayload
					llmObservation = errPayload
				} else {
					observation = obs
					llmObservation = llmObs
					// Phase 83m item 7: notify the per-run dispatch hook
					// on a successful executor return. The dev binary
					// wires this to `taskReg.IncrementToolCount` so the
					// Console Tasks page's tool_count reflects the
					// running per-task dispatch count. Hook errors are
					// surfaced loud — silent degradation of an
					// observability counter is §13-forbidden.
					if spec.OnToolDispatched != nil {
						if hookErr := spec.OnToolDispatched(runCtx); hookErr != nil {
							return planner.Finish{}, fmt.Errorf("steering: tool-dispatched hook: %w", hookErr)
						}
					}
				}
			}
			// Append the step to the run's Trajectory so the planner
			// sees the prior action + observation on its next step.
			// `rc` is a value-copy of `spec.Base`, but `Trajectory` is a
			// pointer — mutations are visible to the next step's rc.
			//
			// Phase 83m item 8: copy the captured reasoning trace
			// (delivered by the planner via the rc.OnReasoning
			// side-channel) onto Step.ReasoningTrace. Without this
			// copy, `ReasoningReplay=text` mode (Phase 83e — D-148)
			// is structurally ineffective in production because the
			// prompt builder reads from Step.ReasoningTrace and finds
			// an empty string on every prior step.
			if spec.Base.Trajectory != nil {
				spec.Base.Trajectory.Steps = append(spec.Base.Trajectory.Steps, planner.Step{
					Action:         decision,
					Observation:    observation,
					LLMObservation: llmObservation,
					ReasoningTrace: stepReasoning,
				})
			}
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
	_ = rl.bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort control-lifecycle emit; observability only
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
