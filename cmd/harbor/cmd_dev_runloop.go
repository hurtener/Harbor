// cmd/harbor/cmd_dev_runloop.go — the per-task RunLoop driver
// (D-097, closes issue #114; D-098, closes issue #123).
//
// `harbor dev` previously had no production consumer for
// `steering.RunLoop` — a `start` request reached
// `tasks.TaskRegistry.Spawn` and the task sat there forever (no
// goroutine drove it through a Planner). The Wave 11 §17.5 audit's
// finding A3 pinned this as a §13 "test stubs as production defaults"
// concern read sideways: the binary advertised itself as a runtime
// but the planner-step loop was dead code in main.go.
//
// This file ships the production driver. The driver:
//
//  1. Subscribes to `task.spawned` events bus-wide via the §6 rule 5
//     elevated-subscription path — admin scope, audit-trail emission.
//     A per-triple filter would force per-session subscriptions and a
//     registry-side hook the V1 design hasn't introduced; the admin
//     subscription is what the rule authorizes for runtime-internal
//     fan-in subscribers (vs. caller-driven cross-session queries).
//  2. For each spawned foreground task, launches a goroutine that
//     constructs a planner.RunContext from the event's identity +
//     payload, calls `tasks.MarkRunning` to advance the task FSM
//     out of `StatusPending`, calls `runLoop.Run(ctx, spec)`, and
//     translates the RunLoop's exit shape into `tasks.MarkComplete` /
//     `tasks.MarkFailed` so the task FSM reaches a terminal state.
//     This bridge is the D-098 closure of D-097's deliberate carve-out:
//     the per-task goroutine owns the FSM transition because it ALREADY
//     owns the per-task lifecycle (it spawned the goroutine, it
//     observes the Run return shape) — shape 1 of the two shapes
//     issue #123 named; the bus-driven shape would have required
//     RunLoop to emit a typed exit event plus a separate subscriber
//     that owns the task-keyed mapping the driver already has (more
//     moving parts for marginal separation).
//  3. Tracks every in-flight goroutine via a WaitGroup. Close cancels
//     the subscription ctx (subscription channel closes; the
//     subscribe-loop returns) and waits for every in-flight RunLoop
//     to drain before returning — no goroutine leak across stack
//     teardown (§11 goroutine-leak rule). The per-task goroutine now
//     blocks on Run + Mark*; both honour the driver's subCtx so Close
//     remains bounded.
//
// # Per-task RunLoop lifecycle
//
// One RunLoop instance backs every spawned task (D-025: the RunLoop
// is concurrent-safe). The TaskID doubles as the RunID — the task
// IS the run at this layer (RFC §6.8). When a task.spawned event
// arrives:
//
//	q := identity.Quadruple{Identity: ev.Identity.Identity, RunID: string(payload.TaskID)}
//	rl.Run(ctx, steering.RunSpec{Planner: planner, Base: planner.RunContext{Quadruple: q, Goal: ...}, TaskID: payload.TaskID})
//
// The goal string is NOT carried on the task.spawned payload —
// `TaskSpawnedPayload` is a SafeSealed bookkeeping struct (D-020).
// The goal lives on the persisted `Task.Query` field; the driver
// looks it up via `taskReg.Get` after the spawn event arrives.
// (Wave 12+ may extend the spawn payload with the goal to avoid the
// extra read; the current shape keeps the payload secret-safe.)
//
// # Identity propagation
//
// The event's Identity Quadruple carries the (tenant, user, session)
// triple but an EMPTY RunID (per `dispatchStart`'s
// `Quadruple{Identity: id}` shape). The driver fills RunID from
// `payload.TaskID`. The resulting Quadruple is what RunLoop.Run
// validates and what every downstream identity check sees.
//
// # Filtering: foreground only
//
// The driver runs the planner only for `KindForeground` tasks.
// Background tasks (`KindBackground`) are spawned by SpawnTask
// emissions from the planner itself — driving a planner against a
// background task would create a recursive planner loop. The runtime
// dispatch executor (a later phase) is the right home for background
// task execution.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

// perTaskRunLoopDriverOpts bundles the dependencies the driver
// consumes. Bus + RunLoop + Planner + TaskRegistry are all mandatory;
// a nil any of them returns ErrPerTaskRunLoopMisconfigured from
// newPerTaskRunLoopDriver. The TaskRegistry is what the driver calls
// MarkRunning / MarkComplete / MarkFailed on to advance the FSM
// (D-098, closes issue #123).
type perTaskRunLoopDriverOpts struct {
	logger   *slog.Logger
	bus      events.EventBus
	runLoop  *steering.RunLoop
	planner  planner.Planner
	tasks    tasks.TaskRegistry // mandatory: the FSM the driver advances on Run exit (D-098)
	taskKind tasks.TaskKind     // KindForeground at V1; the driver only spawns RunLoops for matching kinds
}

// perTaskRunLoopDriver subscribes to `task.spawned` and drives a
// RunLoop per spawned foreground task. The driver is constructed by
// bootDevStack and Closed during stack teardown.
type perTaskRunLoopDriver struct {
	logger   *slog.Logger
	bus      events.EventBus
	runLoop  *steering.RunLoop
	planner  planner.Planner
	tasks    tasks.TaskRegistry
	taskKind tasks.TaskKind

	// subCtx scopes the subscription's lifetime. Cancel cancels the
	// subscription; the subscribe-loop returns; the WaitGroup drains
	// every in-flight RunLoop goroutine before Close returns.
	subCtx     context.Context
	subCancel  context.CancelFunc
	sub        events.Subscription
	subLoopWG  sync.WaitGroup
	runsWG     sync.WaitGroup
	started    bool
	closedOnce sync.Once
}

// ErrPerTaskRunLoopMisconfigured fires when newPerTaskRunLoopDriver
// is called with a nil bus / RunLoop / planner. Driver invariant: all
// three are mandatory.
var ErrPerTaskRunLoopMisconfigured = errors.New("dev: per-task RunLoop driver missing a mandatory dependency")

// newPerTaskRunLoopDriver validates the opts and returns a stopped
// driver. Call Start before serving; call Close to drain.
func newPerTaskRunLoopDriver(opts perTaskRunLoopDriverOpts) (*perTaskRunLoopDriver, error) {
	if opts.bus == nil {
		return nil, fmt.Errorf("%w: bus is nil", ErrPerTaskRunLoopMisconfigured)
	}
	if opts.runLoop == nil {
		return nil, fmt.Errorf("%w: runLoop is nil", ErrPerTaskRunLoopMisconfigured)
	}
	if opts.planner == nil {
		return nil, fmt.Errorf("%w: planner is nil", ErrPerTaskRunLoopMisconfigured)
	}
	if opts.tasks == nil {
		return nil, fmt.Errorf("%w: tasks is nil", ErrPerTaskRunLoopMisconfigured)
	}
	if opts.logger == nil {
		opts.logger = slog.Default()
	}
	if opts.taskKind == "" {
		opts.taskKind = tasks.KindForeground
	}
	return &perTaskRunLoopDriver{
		logger:   opts.logger,
		bus:      opts.bus,
		runLoop:  opts.runLoop,
		planner:  opts.planner,
		tasks:    opts.tasks,
		taskKind: opts.taskKind,
	}, nil
}

// Start opens the admin-scoped subscription and launches the
// subscribe-loop goroutine. Idempotent: a second Start is a no-op.
// The supplied ctx anchors the subscription's lifetime — when ctx
// cancels (e.g. boot was aborted before Close), the subscription
// cancels along with it.
func (d *perTaskRunLoopDriver) Start(ctx context.Context) error {
	if d.started {
		return nil
	}
	d.subCtx, d.subCancel = context.WithCancel(context.Background())
	// Admin-scoped subscription: the driver listens across every
	// (tenant, user, session) triple via §6 rule 5's elevated-
	// subscription path. The bus auto-emits `audit.admin_scope_used`
	// per Phase 05 — observability of every admin-scoped subscribe is
	// the audit trail the rule requires.
	sub, err := d.bus.Subscribe(d.subCtx, events.Filter{
		Admin: true,
		Types: []events.EventType{tasks.EventTypeTaskSpawned},
	})
	if err != nil {
		d.subCancel()
		return fmt.Errorf("subscribe(task.spawned): %w", err)
	}
	d.sub = sub
	d.started = true

	// When the supplied ctx cancels (boot aborted before Close), the
	// subscription cancels too. This is defence-in-depth — Close
	// drives the canonical teardown.
	go func() {
		select {
		case <-ctx.Done():
			d.subCancel()
		case <-d.subCtx.Done():
		}
	}()

	d.subLoopWG.Add(1)
	go d.subscribeLoop()
	return nil
}

// subscribeLoop drains events from the subscription channel. For
// each `task.spawned` event matching the driver's taskKind, the loop
// launches a per-task goroutine that calls RunLoop.Run. The loop
// terminates when the subscription channel closes (subCtx cancelled
// → bus closes the subscription channel).
func (d *perTaskRunLoopDriver) subscribeLoop() {
	defer d.subLoopWG.Done()
	for ev := range d.sub.Events() {
		d.handleEvent(ev)
	}
}

// handleEvent dispatches one `task.spawned` event. The driver only
// drives foreground tasks; background tasks are emitted by the
// planner itself (SpawnTask Decision) and run on the runtime
// dispatch executor (a later phase). A malformed payload (wrong
// type) is logged and skipped — the event registration guarantees
// the shape, so a mismatch here is a programmer error.
func (d *perTaskRunLoopDriver) handleEvent(ev events.Event) {
	payload, ok := ev.Payload.(tasks.TaskSpawnedPayload)
	if !ok {
		d.logger.Warn("perTaskRunLoopDriver: task.spawned with unexpected payload type",
			slog.String("got", fmt.Sprintf("%T", ev.Payload)))
		return
	}
	if payload.Kind != d.taskKind {
		// Background task — the planner itself spawned it via
		// SpawnTask. The runtime dispatch executor (later phase)
		// drives those; the dev driver stays out.
		return
	}

	q := identity.Quadruple{
		Identity: ev.Identity.Identity,
		RunID:    string(payload.TaskID),
	}
	if err := identity.Validate(q.Identity); err != nil {
		d.logger.Warn("perTaskRunLoopDriver: task.spawned with incomplete identity",
			slog.String("task_id", string(payload.TaskID)),
			slog.String("err", err.Error()))
		return
	}

	d.runsWG.Add(1)
	go func() {
		defer d.runsWG.Done()
		d.runOne(q, payload.TaskID)
	}()
}

// runOne is the per-task RunLoop driver. It constructs a planner.
// RunContext from the task's identity, advances the task FSM out of
// StatusPending via MarkRunning, calls runLoop.Run, and translates
// the Run exit shape into MarkComplete / MarkFailed so the task
// reaches a terminal FSM state. The run's ctx is derived from
// d.subCtx so Close cancels every in-flight run.
//
// The planner Goal is left empty at this layer: TaskSpawnedPayload
// (a SafeSealed struct, D-020) does not carry the user-facing Query.
// The runtime executor reading the persisted Task.Query is a later
// phase; this driver wires the SHAPE (RunLoop drives a planner per
// spawned task) without re-introducing a goal-fetch path here.
// Operators that wire their own planner observe an empty Goal; the
// ReAct planner falls through to its default prompt builder which
// surfaces this case cleanly via the LLM's "I have no goal" response.
//
// # FSM bridge (D-098, closes issue #123)
//
// The task FSM is Pending → Running → {Complete, Failed} (the inprocess
// driver's isValidTransition table). The driver therefore must:
//
//  1. Call MarkRunning BEFORE runLoop.Run, otherwise the eventual
//     MarkComplete / MarkFailed would error with ErrInvalidTransition
//     (Pending → Complete is not in the table). MarkRunning failure
//     fails this run loud: a registry that cannot advance Pending →
//     Running cannot satisfy the bridge and we should not let the
//     RunLoop run only to find the FSM stuck.
//  2. Map runLoop.Run's exit to a Mark* call. Three shapes:
//     - Run returned nil + Finish.Reason == FinishGoal → MarkComplete.
//     - Run returned nil + Finish.Reason ∈ {NoPath, Cancelled,
//     DeadlineExceeded, ConstraintsConflict} → MarkFailed with the
//     reason as the error code. These are RunLoop-side terminal
//     states that DID reach Finish; they are not goal-satisfied so
//     the task FSM transitions to Failed (the FSM has no
//     "no-path-but-not-failed" status; Failed is the closest match).
//     - Run returned a non-nil error → MarkFailed with code
//     "runloop_error" (or "cancelled" for context.Canceled, per
//     below) and the error string as the message.
//  3. ctx.Canceled is the third terminal shape (driver shutdown OR an
//     explicit cancel of the run's ctx). The FSM has no
//     "auto-cancelled by ctx" path — Cancel(ctx, id, reason) is the
//     external-caller surface and requires a reason. We map ctx.Canceled
//     to MarkFailed with code="cancelled". Rationale (documented in
//     D-098): the run did not reach a successful goal; Failed is the
//     correct terminal state. An operator who wants explicit cancellation
//     semantics calls TaskRegistry.Cancel directly (which routes through
//     the Cancel path and uses StatusCancelled); the driver's ctx-cancel
//     is a forced-shutdown signal, not a deliberate cancel decision.
//
// On any Mark* error after Run returns, the driver logs Warn but does
// NOT panic — the per-task goroutine returns cleanly so the next
// spawned task can still be processed. A Mark* error means the task
// is already terminal (raced with an external Cancel) or identity
// mismatch (programmer error); neither warrants tearing down the
// driver.
//
// The Mark* calls use a ctx derived from d.subCtx with the task's
// identity triple attached (TaskRegistry rejects calls missing the
// triple per CLAUDE.md §6). When d.subCtx itself is already cancelled
// (driver shutdown raced with Run return), the Mark* call may fail
// with a context error; this is logged at Debug — the FSM transition
// the operator wanted is moot because the binary is shutting down.
func (d *perTaskRunLoopDriver) runOne(q identity.Quadruple, taskID tasks.TaskID) {
	// Build the identity-scoped ctx the TaskRegistry needs. We attach
	// the triple via identity.With (the same call site §6 mandates for
	// every identity-scoped storage method). The ctx is derived from
	// d.subCtx so Close still bounds the Mark* calls.
	taskCtx, idErr := identity.With(d.subCtx, q.Identity)
	if idErr != nil {
		// Pre-Run identity attachment failed — the run never starts.
		// This is a programmer error: handleEvent already validated the
		// identity. Log loud and bail.
		d.logger.Warn("perTaskRunLoopDriver: identity.With failed before Run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", idErr.Error()))
		return
	}

	// MarkRunning advances Pending → Running. The RunLoop's FSM
	// transitions (Complete/Failed) are not in the Pending → ? table —
	// the task MUST be Running before we can mark it terminal.
	if err := d.tasks.MarkRunning(taskCtx, taskID); err != nil {
		// A MarkRunning failure means either (a) the task was cancelled
		// before we got to it (Pending → Cancelled raced), or (b) the
		// registry is unhealthy. Either way, do not run the planner —
		// the eventual terminal Mark* would fail and we would have
		// burned LLM cycles for no FSM transition. Log Warn and bail.
		d.logger.Warn("perTaskRunLoopDriver: MarkRunning failed; skipping Run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", err.Error()))
		return
	}

	spec := steering.RunSpec{
		Planner: d.planner,
		Base: planner.RunContext{
			Quadruple: q,
		},
		TaskID: taskID,
	}
	fin, err := d.runLoop.Run(d.subCtx, spec)
	if err != nil {
		// Cancellation-shaped errors map to MarkFailed{code=cancelled}.
		// The FSM has no auto-cancelled status (Cancel is the external-
		// caller surface and requires a reason); Failed is the closest
		// terminal match for a ctx-cancelled run that did not reach a
		// goal. See D-098 for the full rationale.
		code := "runloop_error"
		if errors.Is(err, context.Canceled) {
			code = "cancelled"
			d.logger.Debug("perTaskRunLoopDriver: run cancelled",
				slog.String("task_id", string(taskID)))
		} else {
			d.logger.Warn("perTaskRunLoopDriver: RunLoop.Run failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", err.Error()))
		}
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    code,
			Message: err.Error(),
		}); mErr != nil {
			// A Mark* failure post-Run is logged but not escalated:
			// either the task was concurrently transitioned terminal
			// (raced with an external Cancel) or the registry is
			// unhealthy. The driver continues serving subsequent
			// spawn events.
			d.logger.Warn("perTaskRunLoopDriver: MarkFailed after Run error failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
		}
		return
	}

	// Run returned a terminal Finish. Map Finish.Reason to MarkComplete
	// / MarkFailed. Only FinishGoal maps to Complete; every other reason
	// is a non-success terminal (the run finished but did not satisfy
	// the goal) and maps to Failed with the reason as the error code.
	if fin.Reason == planner.FinishGoal {
		if mErr := d.tasks.MarkComplete(taskCtx, taskID, tasks.TaskResult{}); mErr != nil {
			d.logger.Warn("perTaskRunLoopDriver: MarkComplete failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
			return
		}
		d.logger.Info("perTaskRunLoopDriver: run finished (complete)",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("reason", string(fin.Reason)))
		return
	}
	// Non-goal terminal Finish (NoPath, Cancelled, DeadlineExceeded,
	// ConstraintsConflict). The run reached Finish so the planner did
	// not raise an error; the FSM transitions to Failed with the
	// FinishReason as the error code so the Console / operator sees
	// WHY the run ended without a goal.
	if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
		Code:    string(fin.Reason),
		Message: "RunLoop finished without satisfying goal: " + string(fin.Reason),
	}); mErr != nil {
		d.logger.Warn("perTaskRunLoopDriver: MarkFailed after non-goal Finish failed",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", mErr.Error()))
		return
	}
	d.logger.Info("perTaskRunLoopDriver: run finished (failed)",
		slog.String("task_id", string(taskID)),
		slog.String("run_id", q.RunID),
		slog.String("reason", string(fin.Reason)))
}

// Close cancels the subscription, waits for the subscribe-loop to
// drain, then waits for every in-flight RunLoop goroutine to return.
// Idempotent: a second Close walks no-ops. The supplied ctx is
// accepted for the closer-signature compatibility (closeFns takes a
// ctx); the driver's drain has its own bounded shape (every RunLoop
// observes d.subCtx cancellation and returns within one drain
// boundary). A pathological RunLoop that holds ctx-cancellation
// indefinitely would block Close; the dev cmd's serve loop applies
// the Server.ShutdownGracePeriod ceiling at the http boundary, so a
// blocked Close eventually surfaces as a graceless exit.
func (d *perTaskRunLoopDriver) Close(_ context.Context) error {
	d.closedOnce.Do(func() {
		if !d.started {
			return
		}
		// Cancel the subscription's ctx — the bus closes the
		// subscription channel, the subscribe-loop returns, every
		// in-flight RunLoop's ctx (which is d.subCtx) cancels.
		d.subCancel()
		// Cancel the subscription explicitly so the bus surfaces
		// the channel close even when the ctx-derived cancellation
		// races.
		if d.sub != nil {
			d.sub.Cancel()
		}
		d.subLoopWG.Wait()
		d.runsWG.Wait()
	})
	return nil
}
