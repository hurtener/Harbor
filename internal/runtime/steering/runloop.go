package steering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
// An earlier phase shipped the steering primitive (the inbox, the nine-type
// taxonomy, ValidatePayload, CheckScope, the Registry); another shipped
// the pause/resume primitive (the Coordinator). Neither did anything by
// itself — there was no run loop to drain the inbox or to route a
// RequestPause decision through the Coordinator. RunLoop IS that loop.
// It is the §13 first consumer of BOTH primitives, landing in the same
// wave per CLAUDE.md §13.
//
// # The loop
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
//	                         the loop records the step and re-enters)
//	    }
//	}
//	Retire the Inbox  -- always, even on error
//
// The full applyEvent treatment happens exactly ONCE per step boundary.
// While a decision execution is in flight the loop ALSO drains the
// inbox, but consumes ONLY approval-bridge-eligible APPROVE /
// REJECT controls there (the gate bridge — without the mid-step
// drain an approval-gated tool deadlocks the run: RunGuarded parks
// until ResolveApproval, whose only production caller is this loop's
// drain). Every other control drained mid-step is deferred verbatim to
// the next boundary, preserving the step-boundary semantics.
// The planner observes the result via RunContext.Control; it never
// touches the Inbox.
//
// # Concurrent reuse
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
	logger   *slog.Logger
	// pauseRecheckInterval is the parked run's Status re-check cadence
	// (the delivery-independent timeout backstop). Defaults to
	// pauseStatusRecheckInterval; injectable via
	// WithPauseStatusRecheckInterval. Set once at construction.
	pauseRecheckInterval time.Duration
}

// runLoopConfig is the option-applied construction config for a RunLoop.
type runLoopConfig struct {
	taskRegistry         tasks.TaskRegistry
	bus                  events.EventBus
	hardCancelHook       func(ctx context.Context, runID string) error
	clock                Clock
	logger               *slog.Logger
	maxControlHistory    int
	gates                map[string]*approval.ApprovalGate
	pauseRecheckInterval time.Duration
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
// cancellation context into an in-flight decision execution (the
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

// WithRunLoopLogger hands the RunLoop a logger for degradation /
// recovery lines (CLAUDE.md §5 — "Warn: unexpected but recovered",
// e.g. a parked run's bus subscription failing and falling back to
// the Status re-check). Defaults to slog.Default(); the production
// assembly threads the telemetry-backed logger.
func WithRunLoopLogger(l *slog.Logger) RunLoopOption {
	return func(cfg *runLoopConfig) {
		if l != nil {
			cfg.logger = l
		}
	}
}

// WithPauseStatusRecheckInterval overrides the parked run's
// Coordinator.Status re-check cadence — the delivery-independent
// backstop that makes the timeout-terminal guarantee hold even when
// the `pause.resumed` bus wake is dropped (and the ONLY wake channel
// on a bus-less RunLoop). The default
// (pauseStatusRecheckInterval, 30s) is deliberately coarse; tests
// inject a small interval so the backstop branch is exercisable
// without a 30s wall-clock wait. A
// non-positive d keeps the default.
func WithPauseStatusRecheckInterval(d time.Duration) RunLoopOption {
	return func(cfg *runLoopConfig) {
		if d > 0 {
			cfg.pauseRecheckInterval = d
		}
	}
}

// WithMaxControlHistory overrides the per-session applied-control history
// cap. A non-positive value falls back to MaxControlHistory.
func WithMaxControlHistory(n int) RunLoopOption {
	return func(c *runLoopConfig) { c.maxControlHistory = n }
}

// WithApprovalGates hands the RunLoop the catalog-applied approval gates
// keyed by tool name (the assembly's catalog band — `assemble.Assemble`
// → `Stack.Gates` — produces this map via the catalog Builder's
// `Deps.AppliedGates` out-channel). When a
// drained CONTROL_APPROVE / CONTROL_REJECT event references a `token`
// the bridge tries each gate's `ResolveApproval` in turn; the gate that
// owns the token resumes its `pending` waiter so the wrapped tool's
// `Invoke` unblocks. When no gate owns the token (a plain RESUME or an
// OAuth-pause APPROVE), the apply path falls back to the direct
// `Coordinator.Resume`. A nil / empty map disables the bridge — the
// loop behaves exactly as before the gate bridge landed (direct Resume only). See
// `applier.advancePause` for the step-boundary routing and
// `applier.routeApprovalControl` + `RunLoop.dispatchDecision` for the
// mid-step routing that fires while a decision execution is in flight
// (the path a planner-dispatched approval-gated tool resumes
// through).
//
// Coupling note (acceptable): `internal/runtime/steering`
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

// NewRunLoop builds a RunLoop. The Registry (owns the per-run
// inboxes the loop drains) and the Coordinator (the ONE
// pause/resume primitive PAUSE / RESUME / APPROVE / REJECT converge on)
// are mandatory; a nil either fails loud with ErrRunLoopMisconfigured.
// Everything else is optional (see the WithXxx options).
//
// The returned RunLoop is immutable after construction and safe
// for concurrent use by N goroutines.
func NewRunLoop(reg *Registry, coord pauseresume.Coordinator, opts ...RunLoopOption) (*RunLoop, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: Registry is nil", ErrRunLoopMisconfigured)
	}
	if coord == nil {
		return nil, fmt.Errorf("%w: Coordinator is nil", ErrRunLoopMisconfigured)
	}
	cfg := runLoopConfig{
		clock:                systemClock{},
		logger:               slog.Default(),
		maxControlHistory:    MaxControlHistory,
		pauseRecheckInterval: pauseStatusRecheckInterval,
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
		history:              newControlHistory(cfg.maxControlHistory),
		bus:                  cfg.bus,
		clock:                cfg.clock,
		logger:               cfg.logger,
		pauseRecheckInterval: cfg.pauseRecheckInterval,
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
// Harbor introduces this seam so the dev binary can wire
// a real `tools.ToolCatalog`-backed executor; previously the runloop's
// `default:` case dropped every CallTool on the floor (the
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
	// projection the next prompt sees (`llmObservation`, the
	// heavy-content-discipline projection: a small summary +
	// ArtifactRef when the raw result is over the heavy threshold,
	// or just == raw when the result is small enough to inline).
	//
	// The runloop appends a trajectory.Step{Action: decision,
	// Observation: raw, LLMObservation: projection} so the planner's
	// renderer sees only the projection.
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
// lives here + ctx — never on the RunLoop struct.
type RunSpec struct {
	// Planner is the swappable reasoning policy the loop drives. Nil
	// fails loud with ErrNoPlanner.
	Planner planner.Planner
	// Base is the run's RunContext template. RunLoop refreshes the
	// per-step fields (Control, Goal) on a copy each step; the planner
	// receives a fresh RunContext per Next call (contract).
	// Base.Quadruple is the run's identity — its triple is validated
	// identity-mandatory before the loop starts.
	Base planner.RunContext
	// TaskID is the run's task. Optional — when set, a PRIORITIZE
	// control event targets it; when empty, a PRIORITIZE fails loud.
	TaskID tasks.TaskID
	// MaxSteps caps the planner-step count. ≤ 0 ⇒ DefaultMaxSteps.
	MaxSteps int

	// ToolExecutor dispatches the planner's non-Finish, non-RequestPause
	// decisions (CallTool, CallParallel, SpawnTask, AwaitTask).
	// When nil, the runloop's default case logs and
	// appends an empty-observation step (the behaviour) so
	// existing pause/steering tests still drive deterministic finishes.
	// In production the dev binary wires a real executor backed by the
	// tool catalog so the planner's CallTool decisions actually run.
	ToolExecutor ToolExecutor

	// Compression is the optional trajectory-compression runner
	// (the §13 first call site of
	// planner.CompressionRunner.MaybeCompress). When non-nil AND the
	// run's Base.Budget.TokenBudget > 0, the runloop invokes
	// MaybeCompress at each step boundary (after the control drain +
	// projection, before Planner.Next) so an over-budget trajectory is
	// compacted into Trajectory.Summary BEFORE the next prompt build —
	// the React prompt builder's `Summary != nil` branch then renders
	// the five-field summary instead of the per-step history and the
	// prompt shrinks. Nil (or a zero TokenBudget) is byte-identical to
	// the pre-111e behaviour: no estimate, no summariser, no events.
	//
	// One compression per run at V1.1.x: the runner is idempotent on
	// `Trajectory.Summary != nil` (the documented scope fence — RFC
	// §6.5; re-compaction cadence is the recorded follow-up).
	// A MaybeCompress error fails the run LOUDLY (the runner already
	// emitted trajectory.compression_failed) — never a silent
	// fall-through that pretends compression happened.
	Compression *planner.CompressionRunner

	// OnToolDispatched is the optional per-run hook the runloop invokes
	// after the ToolExecutor returns WITHOUT ERROR. The dev binary
	// wires it to loop `count` calls of
	// `taskReg.IncrementToolCount(ctx, taskID)` so the Console Tasks
	// page's tool_count reflects the per-task count of SUCCESSFUL tool
	// dispatches, using the same per-decision counting rule as
	// `planner.DecisionInvocationCount`. Note the deliberate failure-axis
	// difference from `planner.AnswerEnvelope.ToolCallsSeen`: tool_count
	// counts successful dispatches only (this hook is skipped on an
	// executor error), while ToolCallsSeen counts ATTEMPTED invocations
	// recorded on the trajectory — a failed dispatch still appends its
	// step, so the envelope counts it. A nil hook is the legacy / test
	// path (no counter wired); a hook that errors fails the run loud —
	// silent degradation of an observability counter is forbidden per
	// §13 (the counter is an integrity surface, not a best-effort log
	// line).
	//
	// count is the decision's tool-invocation count
	// (planner.DecisionInvocationCount): 1 for a successfully-dispatched
	// CallTool, len(Branches) for a CallParallel. The runloop only calls
	// the hook when count > 0 — a SpawnTask / AwaitTask dispatch never
	// invokes it, because spawning or joining a task is not a tool
	// invocation. A dispatch the executor reports as failed (the
	// executor's own error path) also does NOT invoke the hook; the
	// planner's repair / re-plan flow records the failure on the
	// trajectory and the counter stays put.
	OnToolDispatched func(ctx context.Context, count int) error

	// CompletionHook, when set, is the operator-configured run-completion
	// hook: at Run's terminal boundary the runloop fires it exactly once —
	// for EVERY terminal outcome, never mid-run and never on pause —
	// dispatching the run's RunCompletionPayload transcript to the named
	// catalog tool through ToolExecutor. The outcome rides in the payload.
	// The hook runs AFTER the run's (fin, err) are settled and can NEVER
	// alter them: a dispatch failure emits run.hook_failed + a Warn log and
	// nothing else. A nil CompletionHook is byte-identical to the pre-hook
	// behaviour. See completion.go for the payload contract + the detached
	// cancellation bridge.
	CompletionHook *CompletionHookSpec

	// Naming, when set, is the per-run session auto-naming configuration: at
	// Run's terminal boundary the runloop fires the auto-naming trigger once
	// (a sibling of the completion hook) — records the completed turn and,
	// when a title is due, makes ONE bounded Complete call over a transcript
	// digest and writes the result through the registry's manual-safe auto
	// path. The trigger runs AFTER (fin, err) settle and can NEVER alter them;
	// a failure emits session.naming_failed + a Warn and nothing else. A nil
	// Naming is byte-identical to the naming-off behaviour (no counters, no
	// LLM calls, no events). See naming.go.
	Naming *NamingSpec
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
//
// Named returns (fin, err) are load-bearing: the deferred run-completion
// hook fire reads them AFTER they are settled at any terminal exit, so the
// hook covers every terminal outcome (goal, no-path, constraints-conflict,
// cancelled, error) uniformly without a fire at each return site.
func (rl *RunLoop) Run(ctx context.Context, spec RunSpec) (fin planner.Finish, err error) {
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
	// is bounded, only the session-keyed map grows. Accepted V1 limit.
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

	// Per-run run-completion-hook state — all stack-local (the
	// concurrent-reuse contract: per-run state on the run goroutine, never on
	// the compiled artifact): the ordered steering-entry
	// accumulator, the run's start instant, and the initial goal captured
	// BEFORE the loop mutates spec.Base. The deferred fire below reads the
	// settled (fin, err) named returns at every terminal exit — the single
	// seam covering all terminal outcomes. Registered AFTER runCtx/identity
	// are established so a hook never fires for a pre-run misconfiguration
	// (nil planner, incomplete identity, inbox-open failure) — those are
	// not terminal outcomes of an actual run.
	var steeringEntries []steeringEntry
	initialGoal := spec.Base.Goal
	runStartedAt := rl.clock.Now()
	// TERMINAL-BOUNDARY ORDERING (load-bearing, LIFO): the auto-naming defer
	// is registered FIRST and the completion-hook defer SECOND, so the HOOK
	// fires first at the terminal exit. The hook stamps CompletedAt/DurationMS
	// from the clock when it fires — if naming (a synchronous, up-to-10s LLM
	// call) ran first, a slow naming call would inflate the hook's timestamps
	// and delay transcript egress. Naming has no timing fields of its own, so
	// running second costs it nothing. Pinned by
	// TestRun_TerminalOrdering_HookFiresBeforeNaming.
	//
	// The session auto-naming trigger is a SIBLING of the completion hook at
	// the same terminal boundary: registered after runCtx/identity are
	// established so it never fires for a pre-run misconfiguration, and reads
	// the settled (fin, err) named returns. Fires only when a naming policy is
	// active for the run; a nil Naming is byte-identical to the naming-off
	// path.
	if spec.Naming != nil && spec.Naming.Titler != nil {
		defer func() {
			rl.fireNaming(runCtx, spec, q, steeringEntries, initialGoal, fin)
		}()
	}
	if spec.CompletionHook != nil && spec.CompletionHook.Tool != "" {
		defer func() {
			rl.fireCompletionHook(runCtx, spec, q, fin, err, steeringEntries, initialGoal, runStartedAt)
		}()
	}

	// outstandingToken is the run's current pause Token, "" when the run
	// is not paused. It is per-run loop state — it lives on this
	// goroutine's stack, never on the RunLoop struct.
	var outstandingToken pauseresume.Token

	// carryEvents holds control events drained mid-step (while a
	// decision execution was in flight) that were NOT
	// approval-bridge-eligible. They keep their step-boundary
	// semantics: the next boundary merges them ahead of the fresh
	// drain (FIFO preserved — they arrived first) and applies them
	// exactly once. Per-run loop state on this goroutine's stack,
	// never on the RunLoop struct.
	var carryEvents []ControlEvent

	for step := range maxSteps {
		if err := ctx.Err(); err != nil {
			return planner.Finish{}, fmt.Errorf("steering: run cancelled at step boundary: %w", err)
		}

		// While a pause is outstanding the planner must NOT be
		// re-entered (it would just re-emit RequestPause). Block —
		// without busy-spinning — until a steering control event
		// arrives (a RESUME / APPROVE / REJECT, or any other control)
		// OR the pause is reaped out-of-band by the max-park sweeper
		// (— a `pause.resumed` with the typed
		// `timeout` Decision). The next drain applies a control;
		// a timeout is TERMINAL: the run finishes with
		// Finish{ConstraintsConflict} (a deadline the human missed is
		// a constraint the planner cannot resolve — the REJECT
		// posture), never a silent unpark-and-continue. The wait
		// honours ctx so a cancelled run unblocks loud.
		if outstandingToken != "" {
			timedOut, werr := rl.awaitResumeSignal(ctx, runCtx, inbox, q, outstandingToken)
			if werr != nil {
				return planner.Finish{}, fmt.Errorf("steering: waiting for resume control on a paused run: %w", werr)
			}
			if timedOut {
				return timeoutFinish(q, outstandingToken), nil
			}
		}

		// --- DRAIN: the step boundary. The full applyEvent treatment
		// happens here for every control type. (carve-out: while
		// a decision execution is in flight, dispatchDecision keeps
		// draining and consumes approval-bridge-eligible APPROVE /
		// REJECT controls mid-step; everything else it drained rides
		// in carryEvents and is applied at THIS boundary, exactly
		// once.) ---
		drained, derr := inbox.Drain()
		if derr != nil {
			return planner.Finish{}, fmt.Errorf("steering: draining run inbox: %w", derr)
		}
		if len(carryEvents) > 0 {
			drained = append(carryEvents, drained...)
			carryEvents = nil
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
			// Capture an applied steering USER_MESSAGE / REDIRECT for the
			// run-completion transcript (only when a hook is configured —
			// otherwise this is dead work). The steering text is not durably
			// recorded anywhere else: apply.go consumes it per step and the
			// applied-control history drops payloads. The captured trajectory
			// index is the position the entry PRECEDES (drain happens before
			// the step's Planner.Next appends), so the transcript assembler
			// interleaves it just ahead of that step. Captured exactly once
			// per drained event (drained events apply exactly once, even on
			// the paused-accumulation path). Stack-local — concurrent-reuse clean.
			if applyErr == nil && (steeringCaptureWanted(spec)) {
				if se, ok := captureSteeringEntry(ev, trajStepLen(spec.Base.Trajectory)); ok {
					steeringEntries = append(steeringEntries, se)
				}
			}
			if applyErr != nil {
				// Race carve-out: a legitimate
				// RESUME / APPROVE / REJECT control can lose the race
				// against the max-park sweeper — the sweeper's
				// DecisionTimeout Resume lands first and the control's
				// own Coordinator.Resume surfaces ErrAlreadyResumed.
				// The pause resolved exactly once (the documented
				// loser contract); the run's honest outcome is the
				// timeout-terminal Finish, not an error.
				if errors.Is(applyErr, pauseresume.ErrAlreadyResumed) &&
					outstandingToken != "" && rl.pauseTimedOut(runCtx, outstandingToken) {
					return timeoutFinish(q, outstandingToken), nil
				}
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
		// HITL gate is a constraint conflict the planner cannot resolve.
		// The Coordinator.Resume already happened in applyEvent.
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
		// input artifacts attach to the FIRST
		// planner turn only. The current step's rc copy carries them
		// (set at run-loop wire-up time from `task.InputArtifactIDs`);
		// clear them from the base so subsequent steps see an empty
		// slice. Without this, every step's prompt would re-inline the
		// uploaded image bytes — pure waste plus a context blow-up.
		spec.Base.InputArtifacts = nil
		if sc.goal != "" {
			rc.Goal = sc.goal
			// Persist the redirected goal into the run's base so a later
			// step (after a non-REDIRECT drain) still sees it.
			spec.Base.Goal = sc.goal
		}

		// item 8: per-step closure that captures the planner's
		// reasoning trace via the RunContext.OnReasoning side-channel.
		// The runloop reads stepReasoning after Planner.Next returns and
		// copies it into the appended trajectory.Step. The closure is
		// scoped to THIS step (one captured variable per iteration); a
		// new closure is installed each step so a stale read from a
		// prior step never reaches the next append. The capture lives
		// on this goroutine's stack — the concurrent-reuse contract holds (no planner-side
		// mutable state).
		var stepReasoning string
		rc.OnReasoning = func(s string) { stepReasoning = s }

		// Capture the assistant's preamble prose (the model's
		// natural-language `content` field on the response, emitted
		// alongside any `tool_calls`) so the prompt builder can replay
		// it on the next turn's assistant message and the model
		// retains its narrative thread. Same closure shape as
		// OnReasoning: per-run stack-local, nil-safe.
		var stepAssistantContent string
		rc.OnAssistantContent = func(s string) { stepAssistantContent = s }

		// (AC-19 + AC-19a) — wire the per-run
		// native-tool-calling queue callback. The planner receives rc
		// by VALUE, so any mutations the projector makes to
		// `rc.PendingToolCalls` inside Next die with the planner's
		// stack frame. The callback bridges the boundary the same way
		// `rc.OnReasoning` does: a function pointer set on the
		// runloop's local rc, captured by the planner's closure when
		// Next returns. The runloop reads `stepPending` after Next
		// returns and writes it into `spec.Base` so the next
		// iteration's value-copy carries the queue forward.
		//
		// `rc.DiscoveredTools` does NOT need this bridge — the
		// projector re-derives it from the trajectory pointer each
		// step (mergeDiscovered walks `*Trajectory` which the runloop
		// appends to). Keeping this callback narrow to
		// PendingToolCalls preserves rc-as-read-only for every field
		// that has another persistence channel.
		var stepPending []planner.ToolCallDeferred
		rc.OnPendingToolCalls = func(pending []planner.ToolCallDeferred) {
			stepPending = pending
		}

		// --- COMPRESS: the trajectory-compression
		// gate, after the drain/projection and before Planner.Next so
		// THIS step's prompt build already sees the compacted view.
		// The runner owns the semantics (estimate → threshold →
		// summarise → stamp Summary → emit trajectory.compressed;
		// idempotent on Summary != nil — one compression per run at
		// V1.1.x). The gate below keeps the nil-runner / zero-budget
		// paths byte-identical to the pre-111e loop. An error is
		// fail-loud: the runner emitted trajectory.compression_failed
		// and the run terminates with the wrapped error — never a
		// silent fall-through to raw history (CLAUDE.md §13).
		if spec.Compression != nil && rc.Budget.TokenBudget > 0 {
			if cerr := spec.Compression.MaybeCompress(runCtx, rc, rc.Trajectory); cerr != nil {
				return planner.Finish{}, fmt.Errorf("steering: trajectory compression at step %d: %w", step, cerr)
			}
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

		// Write the post-step pending queue into spec.Base so the
		// next iteration's `rc := spec.Base` value-copy sees it.
		// stepPending is nil when the planner did not invoke the
		// callback (drain-only step, no projector run).
		if stepPending != nil {
			spec.Base.PendingToolCalls = stepPending
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
			// control event arrives via the inbox -> the next
			// step's drain applies it -> Coordinator.Resume -> the
			// planner re-enters.
			tok, perr := rl.requestPause(runCtx, q, d, spec.Base.Trajectory)
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
			// CallTool / CallParallel / SpawnTask / AwaitTask. Note:
			// dispatch via spec.ToolExecutor when present, then
			// append a trajectory.Step the planner sees on its next step.
			// Without the trajectory append the planner repeats the same
			// prompt forever (the failure mode the audit pinned in
			// production). Without the dispatch the operator gets a
			// "planner-only" loop that never actually does work. Both
			// are V1.1 blockers.
			//
			// When spec.ToolExecutor is nil (the dev / legacy
			// test path), the step still gets appended with a nil
			// Observation so the planner sees its decision did NOT
			// silently disappear (audit lesson: silent execution gaps
			// are §13-forbidden silent degradation).
			var observation, llmObservation any
			if spec.ToolExecutor != nil {
				// dispatch on a per-step goroutine and keep
				// draining the inbox while the execution is in flight,
				// routing ONLY approval-bridge-eligible APPROVE /
				// REJECT controls (the gate bridge) mid-step. A
				// synchronous dispatch here deadlocked approval-gated
				// tools: the gate's RunGuarded parked until
				// ResolveApproval, whose only production caller is
				// this loop's own drain. Every other control type
				// keeps its step-boundary semantics — dispatchDecision
				// returns them as `deferred` and the next boundary
				// applies them exactly once. The per-step goroutine is
				// joined before dispatchDecision returns; run-ctx
				// cancellation still aborts an in-flight gated
				// decision (the step ctx is a child of runCtx).
				out, deferred, bridgeErr := rl.dispatchDecision(runCtx, q, inbox, spec.ToolExecutor, rc, decision)
				carryEvents = deferred
				if bridgeErr != nil {
					// A mid-step gate-bridge failure is the same
					// fail-loud shape as a step-boundary apply
					// failure — surface it verbatim.
					return planner.Finish{}, bridgeErr
				}
				obs, llmObs, execErr := out.observation, out.llmObservation, out.err
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
					// Notify the per-run dispatch hook on a successful
					// executor return, with the decision's true
					// tool-invocation count — 1 for CallTool,
					// len(Branches) for CallParallel. SpawnTask /
					// AwaitTask carry count == 0 and do NOT invoke the
					// hook: spawning or joining a task is not a tool
					// invocation. The dev binary wires this to
					// `taskReg.IncrementToolCount` so the Console Tasks
					// page's tool_count reflects the true per-task
					// tool-invocation count. Hook errors are surfaced
					// loud — silent degradation of an observability
					// counter is §13-forbidden.
					if spec.OnToolDispatched != nil {
						if n := planner.DecisionInvocationCount(decision); n > 0 {
							if hookErr := spec.OnToolDispatched(runCtx, n); hookErr != nil {
								return planner.Finish{}, fmt.Errorf("steering: tool-dispatched hook: %w", hookErr)
							}
						}
					}
				}
			}
			// Append the step to the run's Trajectory so the planner
			// sees the prior action + observation on its next step.
			// `rc` is a value-copy of `spec.Base`, but `Trajectory` is a
			// pointer — mutations are visible to the next step's rc.
			//
			// item 8: copy the captured reasoning trace
			// (delivered by the planner via the rc.OnReasoning
			// side-channel) onto Step.ReasoningTrace. Without this
			// copy, `ReasoningReplay=text` mode
			// is structurally ineffective in production because the
			// prompt builder reads from Step.ReasoningTrace and finds
			// an empty string on every prior step.
			if spec.Base.Trajectory != nil {
				spec.Base.Trajectory.Steps = append(spec.Base.Trajectory.Steps, planner.Step{
					Action:            decision,
					Observation:       observation,
					LLMObservation:    llmObservation,
					ReasoningTrace:    stepReasoning,
					AssistantPreamble: stepAssistantContent,
				})
			}
		}
	}

	return planner.Finish{}, fmt.Errorf("%w: %d steps", ErrMaxStepsExceeded, maxSteps)
}

// requestPause routes a planner's RequestPause decision through the
// unified Coordinator. It maps the planner-side PauseReason onto the
// pauseresume.Reason (the typedef bridge keeps them byte-identical) and
// hands the run's LIVE trajectory through so a
// checkpoint-store-backed Coordinator persists the planner state with
// the pause record — the premise ("the planner can pause …
// get serialised to a state store, and be resumed in a different
// process") made true on the production path. A pauseresume error
// (trajectory.ErrUnserializable from a non-serialisable payload /
// trajectory leaf, ErrInvalidReason) propagates verbatim — no silent
// degradation, no half-persisted checkpoint.
func (rl *RunLoop) requestPause(ctx context.Context, q identity.Quadruple, d planner.RequestPause, tr *planner.Trajectory) (pauseresume.Token, error) {
	req := pauseresume.PauseRequest{
		Identity:   q.Identity,
		Reason:     d.Reason, // Reason is `= planner.PauseReason` — same type
		Payload:    d.Payload,
		Trajectory: tr, // planner.Trajectory is `= trajectory.Trajectory` — same type
	}
	pause, err := rl.coord.Request(ctx, req)
	if err != nil {
		return "", fmt.Errorf("steering: routing RequestPause through Coordinator.Request: %w", err)
	}
	return pause.Token, nil
}

// pauseStatusRecheckInterval is the coarse fallback cadence at which a
// parked run re-checks Coordinator.Status for an out-of-band
// DecisionTimeout resume. The primary timeout signal is the
// `pause.resumed` bus event (delivered within milliseconds); the
// re-check makes the timeout-terminal guarantee independent of
// best-effort event delivery (a dropped event must not park a run
// forever) and is the ONLY signal on a bus-less RunLoop. The Status
// lookup is an in-memory map read — the cadence is deliberately
// coarse.
const pauseStatusRecheckInterval = 30 * time.Second

// awaitResumeSignal blocks while a pause is outstanding
// It returns:
//
//   - (false, nil) — a steering control event arrived; the caller
//     drains and applies it (the pre-111c WaitForEvent contract).
//   - (true, nil)  — the pause was resumed out-of-band with the typed
//     `timeout` Decision (the max-park sweeper reaped it); the run is
//     TERMINAL — the caller finishes with Finish{ConstraintsConflict}.
//   - (false, err) — ctx was cancelled or the inbox was retired. A
//     FAILED bus subscription does NOT error: the park degrades to
//     the Status re-check ticker (logged at Warn — §5: unexpected but
//     recovered), exactly as a bus-less RunLoop behaves.
//
// The timeout signal has two channels: the canonical `pause.resumed`
// bus event (primary, when the RunLoop carries a bus) and a coarse
// Coordinator.Status re-check (the delivery-independent backstop, and
// the only channel on a bus-less RunLoop). A non-timeout out-of-band
// resume (e.g. a tool-side OAuth completion) deliberately does NOT
// wake the run here — those flows re-enter via a steering control
// event, exactly as before this phase.
func (rl *RunLoop) awaitResumeSignal(ctx, runCtx context.Context, inbox *Inbox, q identity.Quadruple, token pauseresume.Token) (bool, error) {
	// Race-close fast path: the sweeper may have reaped the pause
	// before this wait began (tiny max-park windows). Checked BEFORE
	// subscribing / blocking so the terminal outcome is never missed.
	if rl.pauseTimedOut(runCtx, token) {
		return true, nil
	}

	// Subscribe to the run's own event stream (identity-scoped — the
	// run's triple + RunID; CLAUDE.md §6 rule 5) so a sweeper-emitted
	// pause.resumed wakes the park promptly. Best-effort: a RunLoop
	// without a bus, or a bus already shutting down, falls back to the
	// Status re-check ticker below.
	var busEvents <-chan events.Event
	if rl.bus != nil {
		sub, serr := rl.bus.Subscribe(ctx, events.Filter{
			Tenant:  q.TenantID,
			User:    q.UserID,
			Session: q.SessionID,
			Run:     q.RunID,
		})
		if serr == nil {
			defer sub.Cancel()
			busEvents = sub.Events()
		} else {
			// Surfaced loud, then recovered (§5): the park degrades
			// from the millisecond bus wake to the coarse Status
			// re-check ticker below — same behaviour as a bus-less
			// RunLoop, never a silent contract change.
			rl.logger.WarnContext(ctx, "steering: parked run could not subscribe to the bus — timeout wake degrades to the Status re-check ticker",
				slog.String("tenant_id", q.TenantID),
				slog.String("user_id", q.UserID),
				slog.String("session_id", q.SessionID),
				slog.String("run_id", q.RunID),
				slog.String("pause_token", string(token)),
				slog.Any("error", serr))
		}
	}

	// Park the inbox wait on its own goroutine so this select can also
	// observe the bus / the re-check ticker. The goroutine is joined on
	// every return path (waitCancel unblocks WaitForEvent via ctx) — no
	// leak (CLAUDE.md §5).
	waitCtx, waitCancel := context.WithCancel(ctx)
	waitDone := make(chan error, 1)
	go func() { waitDone <- inbox.WaitForEvent(waitCtx) }()
	join := func() {
		waitCancel()
		<-waitDone
	}

	recheck := time.NewTicker(rl.pauseRecheckInterval)
	defer recheck.Stop()

	// Close the subscribe-after-publish window: the sweeper may have
	// reaped the pause between the pause request and the subscription
	// established above — a bus wake published before the subscription
	// landed is never delivered, leaving only the coarse ticker. One
	// immediate Status check at park entry makes the timeout wake
	// delivery-independent (found as a CI-only flake in the
	// combination: TestRun_PauseTimeout_BusWake missed the wake and hit
	// the 30s backstop past the test's 5s bound).
	if rl.pauseTimedOut(runCtx, token) {
		join()
		return true, nil
	}

	for {
		select {
		case werr := <-waitDone:
			waitCancel()
			if werr != nil {
				return false, werr
			}
			return false, nil

		case ev, ok := <-busEvents:
			if !ok {
				// Bus shut down mid-park — drop to the inbox wait +
				// Status re-check only.
				busEvents = nil
				continue
			}
			if ev.Type != pauseresume.EventTypePauseResumed {
				continue
			}
			payload, isResumed := ev.Payload.(pauseresume.PauseResumedPayload)
			if !isResumed || payload.Token != string(token) || payload.Decision != pauseresume.DecisionTimeout {
				continue
			}
			join()
			return true, nil

		case <-recheck.C:
			if rl.pauseTimedOut(runCtx, token) {
				join()
				return true, nil
			}
		}
	}
}

// pauseTimedOut reports whether the run's outstanding pause has been
// resumed out-of-band with the typed `timeout` Decision (the max-park
// sweeper's reap —). A Status error is treated as
// "no timeout observed": the bus event remains the primary signal and
// the next re-check retries; the check never converts a Status read
// failure into a run failure.
func (rl *RunLoop) pauseTimedOut(ctx context.Context, token pauseresume.Token) bool {
	st, err := rl.coord.Status(ctx, token)
	if err != nil {
		return false
	}
	return st.State == pauseresume.StatusResumed && st.Decision == pauseresume.DecisionTimeout
}

// timeoutFinish is the terminal outcome of a max-park timeout: the
// pause's deadline elapsed with no human decision, which is a
// constraint the planner cannot resolve (the REJECT posture
// applied to deadlines — plan §"Risks", settled). The metadata names
// the timeout so observers distinguish it from a steering REJECT.
func timeoutFinish(q identity.Quadruple, token pauseresume.Token) planner.Finish {
	return planner.Finish{
		Reason: planner.FinishConstraintsConflict,
		Metadata: map[string]any{
			"run_id":          q.RunID,
			"steering_reason": "pause_timeout",
			"pause_token":     string(token),
			"pause_decision":  string(pauseresume.DecisionTimeout),
		},
	}
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
// edge projects this as the session's steering audit trail.
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
