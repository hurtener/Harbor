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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// devRuntimeSkillsContextMaxDefault is the dev-binary default when
// `planner.skills_context_max` is unset (0). Matches the cap mentioned
// in Phase 83f's plan and brief 13 §2.4's "small, bounded" guidance.
const devRuntimeSkillsContextMaxDefault = 5

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

	// Phase 83f (D-149) — RunContext consumer wiring. All three of
	// memory / skills / planningHints are OPTIONAL: a dev stack that
	// did not open the respective subsystem hands nil; the driver
	// projects the corresponding RunContext field to nil and the
	// planner omits the wrapper. `skillsContextMax` is the cap the
	// driver applies when calling `SkillStore.Search` — zero resolves
	// to the package default (5).
	memory           memory.MemoryStore
	skills           skills.SkillStore
	skillsContextMax int
	planningHints    *planner.PlanningHints

	// Phase 83i (D-152) — tool dispatch + Catalog projection +
	// Trajectory. The tool catalog is the shared catalog the rest of
	// the dev stack already populated (in-process tools, MCP-discovered
	// tools, etc.). MaxStepsRunLoop caps the runloop's outer step
	// counter (separate from the planner-internal cap that goes onto
	// react via PlannerConfig.MaxSteps).
	catalog         tools.ToolCatalog
	executor        steering.ToolExecutor
	maxStepsRunLoop int

	// Phase 83m (Item 6, D-156) — operator-declared GrantedScopes
	// threaded into the per-run catalog view's CatalogFilter. Tools
	// whose AuthScopes exceed this set are invisible to the planner.
	// Nil / empty list means no scopes granted (the existing latent
	// default before the plumb-through).
	grantedScopes []string

	// Round-7 F11 / D-166 — the artifact store the multimodal
	// materializer reads from. Required only when `task.InputArtifactIDs`
	// is non-empty (text-only tasks never touch the store). A nil
	// store with input artifacts on the task degrades gracefully —
	// the materializer emits text-stub-only references the LLM
	// routes via the catalog.
	artifactStore artifacts.ArtifactStore
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

	// Phase 83f (D-149) per-run consumer wiring. See driver opts godoc.
	memory           memory.MemoryStore
	skills           skills.SkillStore
	skillsContextMax int
	planningHints    *planner.PlanningHints

	// Phase 83i (D-152) — tool dispatch + Catalog projection.
	catalog         tools.ToolCatalog
	executor        steering.ToolExecutor
	maxStepsRunLoop int

	// Phase 83m (Item 6, D-156) — operator-declared GrantedScopes.
	grantedScopes []string

	// Round-7 F11 / D-166 — artifact store handle for the multimodal
	// materializer.
	artifactStore artifacts.ArtifactStore

	// Phase 107a — per-task trajectory map for the Enricher seam.
	// Trajectories are stored before RunLoop.Run and retained after
	// completion for tasks.get enrichment. Reads are safe under RLock;
	// writes acquire the full mutex. An evicted task returns nil.
	trajMu      sync.RWMutex
	trajectories map[tasks.TaskID]*planner.Trajectory

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
	skillsCap := opts.skillsContextMax
	if skillsCap <= 0 {
		skillsCap = devRuntimeSkillsContextMaxDefault
	}
	return &perTaskRunLoopDriver{
		logger:           opts.logger,
		bus:              opts.bus,
		runLoop:          opts.runLoop,
		planner:          opts.planner,
		tasks:            opts.tasks,
		taskKind:         opts.taskKind,
		memory:           opts.memory,
		skills:           opts.skills,
		skillsContextMax: skillsCap,
		planningHints:    opts.planningHints,
		catalog:          opts.catalog,
		executor:         opts.executor,
		maxStepsRunLoop:  opts.maxStepsRunLoop,
		grantedScopes:    append([]string(nil), opts.grantedScopes...),
		artifactStore:    opts.artifactStore,
		trajectories:     make(map[tasks.TaskID]*planner.Trajectory),
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

	// Phase 83f (D-149): build the per-run consumer state BEFORE
	// handing the RunSpec to the RunLoop. The four primitives the
	// 83-band shipped now have a real production consumer.
	//
	// Step 1: fetch the task record to read the user-facing Query.
	// The Query becomes the run's `Goal` (the planner-visible goal
	// starts equal to the user's request; runtime REDIRECT can mutate
	// it later — see RunContext.Goal godoc).
	task, gErr := d.tasks.Get(taskCtx, taskID)
	if gErr != nil {
		d.logger.Warn("perTaskRunLoopDriver: tasks.Get failed; failing run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", gErr.Error()))
		if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    "runtime_fetch_error",
			Message: fmt.Sprintf("tasks.Get: %v", gErr),
		}); fErr != nil {
			d.logger.Warn("perTaskRunLoopDriver: MarkFailed(runtime_fetch_error) failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", fErr.Error()))
		}
		return
	}

	// Step 2: fetch identity-scoped memory + skills. Each is OPTIONAL
	// — a stack without the subsystem configured leaves the
	// corresponding field nil and the planner omits the wrapper. A
	// store-side error is LOUD per CLAUDE.md §5 fail-loud: the run
	// fails with the wrapped error, the LLM is never called, and the
	// operator sees a clear `runtime_fetch_error` on the task.
	//
	// Memory + skills are SESSION-scoped per RFC §6.6/§6.7 (memory
	// spans runs within a session; skills are stored per-session). The
	// fetch quadruple zeroes RunID so the run inherits the session's
	// accumulated state rather than seeing only its own (empty) per-run
	// slice. D-149.
	sessionQ := identity.Quadruple{Identity: q.Identity}
	var memBlocks *planner.MemoryBlocks
	if d.memory != nil {
		patch, mErr := d.memory.GetLLMContext(taskCtx, sessionQ)
		if mErr != nil {
			d.logger.Warn("perTaskRunLoopDriver: memory.GetLLMContext failed; failing run",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: fmt.Sprintf("memory.GetLLMContext: %v", mErr),
			}); fErr != nil {
				d.logger.Warn("perTaskRunLoopDriver: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		if mb := projectMemoryBlocks(patch); mb != nil {
			memBlocks = mb
		}
	}

	var skillsCtx []any
	if d.skills != nil && task.Query != "" {
		// Phase 83m (Item 4, D-156): extract keyword tokens from the
		// raw task Query before handing it to the FTS5-backed skills
		// driver. The SQLite skills driver's FTS5 ranker (BM25)
		// performs poorly on full-sentence inputs — articles, common
		// stopwords, and punctuation diffuse the score across noisy
		// terms. The helper lowercases, drops stopwords + punctuation,
		// dedupes, and caps at the standard 10-term ceiling. A
		// pathological input that produces no keywords falls back to
		// the raw Query so Search still has SOMETHING to rank
		// against — the driver's empty-query handling is the
		// canonical "no ranked skills" path and we prefer the raw
		// query to that empty path.
		searchQuery := extractSkillKeywords(task.Query)
		if searchQuery == "" {
			searchQuery = task.Query
		}
		ranked, sErr := d.skills.Search(taskCtx, sessionQ, searchQuery, d.skillsContextMax)
		if sErr != nil {
			d.logger.Warn("perTaskRunLoopDriver: skills.Search failed; failing run",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", sErr.Error()))
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: fmt.Sprintf("skills.Search: %v", sErr),
			}); fErr != nil {
				d.logger.Warn("perTaskRunLoopDriver: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		skillsCtx = projectSkillsContext(ranked)
	}

	// Step 3: per-run RepairCounters. ONE pointer per run, threaded
	// onto RunContext; Phase 44's repair pipeline increments it.
	// D-145 (counters scope to RunContext, not the planner artifact).
	counters := &planner.RepairCounters{}

	// Step 4 (Phase 83i — D-152): per-run Trajectory + the per-run
	// Catalog view. The Trajectory is appended to by the runloop
	// after every non-Finish, non-RequestPause step; without it the
	// planner sees an empty trajectory every step and (with a real
	// LLM) sends the identical prompt repeatedly. The Catalog view
	// is the planner-facing schema-only projection of the production
	// catalog under the run's identity scope; without it the
	// `<available_tools>` section renders empty and the LLM has no
	// tool affordance.
	traj := &planner.Trajectory{Query: task.Query}
	var catalogView planner.ToolCatalogView
	if d.catalog != nil {
		// Phase 83m (Item 6, D-156): the catalog view's CatalogFilter
		// now receives the operator-configured `tools.granted_scopes`
		// list. Tools whose AuthScopes exceed this set are invisible
		// to the planner; an empty list preserves the prior behaviour
		// (tools without AuthScopes are always visible; tools with
		// AuthScopes are filtered out).
		catalogView = newRuntimeCatalogView(
			d.catalog,
			runtimeIdentity{Tenant: q.TenantID, User: q.UserID, Session: q.SessionID},
			d.grantedScopes,
		)
	}

	// Phase 83i (D-152) — wire the planner's event-emit closure so
	// `planner.decision` / `planner.finish` / `planner.repair_guidance_injected`
	// reach the bus. Without this the entire planner-side telemetry
	// stream is silent (operators / Console / inspect-runs see only
	// llm.cost.recorded). The closure stamps the run's identity
	// quadruple on every event and publishes under the driver's bus
	// context so a bus-close mid-run logs Warn rather than races.
	emit := func(ev events.Event) {
		if ev.Identity.Identity.TenantID == "" {
			ev.Identity = q
		}
		if pubErr := d.bus.Publish(d.subCtx, ev); pubErr != nil {
			d.logger.Warn("perTaskRunLoopDriver: bus publish failed",
				slog.String("type", string(ev.Type)),
				slog.String("err", pubErr.Error()))
		}
	}

	// Phase 83m item 7: per-run OnToolDispatched hook that advances
	// the task's `ToolCount` registry-side after every successful
	// CallTool dispatch. The dev binary closes the seam from the
	// runloop's side (the executor returned without error) to the
	// tasks.TaskRegistry surface the Console Tasks page reads. A
	// best-effort log + non-fatal continuation would mask a counter
	// drift the operator depends on for visibility — the hook
	// surfaces an IncrementToolCount error loud, matching §13.
	dispatchHook := func(hookCtx context.Context) error {
		if err := d.tasks.IncrementToolCount(hookCtx, taskID); err != nil {
			return fmt.Errorf("tasks.IncrementToolCount(%q): %w", taskID, err)
		}
		return nil
	}

	// Phase 107 — per-run OnChunk closure. Translates bifrost streaming
	// deltas into `llm.completion.chunk` bus events under the run's
	// identity quadruple. Per D-025: the closure is per-run on the
	// stack; N concurrent runs see N independent closures.
	onChunk := func(delta string, done bool, kind planner.ChunkKind) {
		payload := llm.CompletionChunkPayload{
			Identity:   q,
			TaskID:     string(taskID),
			RunID:      q.RunID,
			Delta:      delta,
			Done:       done,
			Kind:       string(kind),
			OccurredAt: time.Now(),
		}
		if pubErr := d.bus.Publish(d.subCtx, events.Event{
			Type:    llm.EventTypeCompletionChunk,
			Payload: payload,
		}); pubErr != nil {
			d.logger.Warn("perTaskRunLoopDriver: chunk publish failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", pubErr.Error()))
		}
	}

	// Round-7 F11 / D-166 — pre-resolve operator-uploaded input
	// artifacts so the planner's first-turn materializer renders them
	// as multimodal Content.Parts. The runloop clears
	// `Base.InputArtifacts` after the first step (per
	// `runloop.go::spec.Base.InputArtifacts = nil` at the end of the
	// per-step build) so subsequent steps see an empty slice.
	inputArtifacts := d.resolveInputArtifacts(taskCtx, q, task.InputArtifactIDs)

	spec := steering.RunSpec{
		Planner: d.planner,
		Base: planner.RunContext{
			Quadruple:      q,
			Query:          task.Query,
			Goal:           task.Query, // initial goal = user query; runtime REDIRECT may mutate
			MemoryBlocks:   memBlocks,
			SkillsContext:  skillsCtx,
			RepairCounters: counters,
			PlanningHints:  d.planningHints, // nil when operator left the config block empty
			Catalog:        catalogView,     // Phase 83i (D-152) — populates <available_tools>
			Trajectory:     traj,            // Phase 83i (D-152) — runloop appends per step
			Emit:           emit,            // Phase 83i (D-152) — planner-side telemetry
			OnChunk:        onChunk,         // Phase 107 — per-token streaming to bus
			InputArtifacts: inputArtifacts,  // Round-7 F11 / D-166 — first-turn multimodal inputs
		},
		TaskID:           taskID,
		ToolExecutor:     d.executor,   // Phase 83i (D-152) — dispatch CallTool decisions
		OnToolDispatched: dispatchHook, // Phase 83m item 7 — advance Task.ToolCount on dispatch
		MaxSteps:         d.maxStepsRunLoop,
	}
	// Phase 107a — save the trajectory ref before Run so the Enricher
	// can read it post-completion (including concurrently — the map is
	// mutex-guarded per D-025).
	d.trajMu.Lock()
	d.trajectories[taskID] = traj
	d.trajMu.Unlock()

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
		// Phase 83i (D-152) — Memory writeback. The 83d/83f read path
		// is wired (run loop hands MemoryBlocks to the planner); the
		// write path was the missing half. Without a writeback the
		// session-scoped memory stays empty forever and the operator's
		// multi-turn sessions cannot carry context. Best-effort: a
		// memory.AddTurn error is logged Warn but does NOT downgrade
		// the run's terminal status — the planner reached FinishGoal,
		// the operator should see Complete.
		if d.memory != nil {
			turn := memory.ConversationTurn{
				UserMessage:       task.Query,
				AssistantResponse: extractAssistantAnswer(fin),
				Timestamp:         time.Now(),
			}
			if mErr := d.memory.AddTurn(taskCtx, sessionQ, turn); mErr != nil {
				d.logger.Warn("perTaskRunLoopDriver: memory.AddTurn failed; run still marked complete",
					slog.String("task_id", string(taskID)),
					slog.String("run_id", q.RunID),
					slog.String("err", mErr.Error()))
			}
		}

		// Phase 106 (V1.2) — populate the answer envelope so Protocol
		// consumers (Console Playground, CLI, third-party UIs) read the
		// actual assistant response via tasks.get → result_inline.
		// Pre-106, this was tasks.TaskResult{} — the projector had
		// nothing to project and the Playground hardcoded a placeholder.
		payload := map[string]any{
			"answer":          extractAssistantAnswer(fin),
			"finish_reason":   string(fin.Reason),
			"tool_calls_seen": len(traj.Steps),
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			d.logger.ErrorContext(taskCtx, "perTaskRunLoopDriver: marshal TaskResult.Value failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", err.Error()))
			raw = []byte("{}")
		}
		if mErr := d.tasks.MarkComplete(taskCtx, taskID, tasks.TaskResult{Value: raw}); mErr != nil {
			d.logger.Warn("perTaskRunLoopDriver: MarkComplete failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
			return
		}
		d.logger.Info("perTaskRunLoopDriver: run finished (complete)",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("reason", string(fin.Reason)),
			slog.Int("trajectory_steps", len(traj.Steps)))
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
// resolveInputArtifacts pre-fetches the metadata (+ bytes for
// `image/*`) for every operator-uploaded artifact ID on a task,
// producing the `planner.InputArtifactView` slice the run loop hands
// to the planner's first turn. Round-7 F11 / D-166 — the synchronous
// pre-resolution keeps the planner's prompt assembly I/O-free.
//
// Failures are bounded:
//   - Nil artifact store with non-empty IDs → empty slice + Warn log
//     (the LLM still sees a text-only prompt; the operator can re-
//     attach after wiring the store). Avoids a hard fail-loud here
//     because the artifact-store dependency is genuinely optional in
//     some dev postures.
//   - `GetRef` not-found / errored → skip that ID + Warn (the rest
//     of the slice survives; the artifact may have been GC'd between
//     spawn and run).
//   - `Get` (bytes fetch) errored on an image/* → keep the entry but
//     leave Bytes nil. The materializer falls back to a stub-text
//     part for missing-bytes images — see
//     `TestMaterializeInputContent_ImageMissingBytesFallsBackToRef`.
//
// The scope on every store call is the run's identity tuple — the
// artifact store enforces tenant isolation on read.
func (d *perTaskRunLoopDriver) resolveInputArtifacts(
	ctx context.Context, q identity.Quadruple, ids []string,
) []planner.InputArtifactView {
	if len(ids) == 0 {
		return nil
	}
	if d.artifactStore == nil {
		d.logger.Warn("perTaskRunLoopDriver: input artifacts ignored — no artifact store wired",
			slog.String("run_id", q.RunID),
			slog.Int("count", len(ids)))
		return nil
	}
	scope := artifacts.ArtifactScope{
		TenantID:  q.TenantID,
		UserID:    q.UserID,
		SessionID: q.SessionID,
	}
	out := make([]planner.InputArtifactView, 0, len(ids))
	for _, id := range ids {
		ref, found, gerr := d.artifactStore.GetRef(ctx, scope, id)
		if gerr != nil {
			d.logger.Warn("perTaskRunLoopDriver: artifact GetRef failed; skipping",
				slog.String("run_id", q.RunID),
				slog.String("artifact_id", id),
				slog.String("err", gerr.Error()))
			continue
		}
		if !found || ref == nil {
			d.logger.Warn("perTaskRunLoopDriver: artifact not found; skipping",
				slog.String("run_id", q.RunID),
				slog.String("artifact_id", id))
			continue
		}
		view := planner.InputArtifactView{
			ID:        ref.ID,
			MIME:      ref.MimeType,
			SizeBytes: ref.SizeBytes,
			Filename:  ref.Filename,
		}
		// Image MIMEs need the bytes inline (Path 1 — DataURL).
		// Everything else stays as a ref the materializer renders as
		// an `ArtifactStub`.
		if strings.HasPrefix(ref.MimeType, "image/") {
			bytesPayload, getFound, berr := d.artifactStore.Get(ctx, scope, id)
			if berr != nil || !getFound || len(bytesPayload) == 0 {
				d.logger.Warn("perTaskRunLoopDriver: image artifact bytes missing; emitting ref-only fallback",
					slog.String("run_id", q.RunID),
					slog.String("artifact_id", id))
			} else {
				view.Bytes = bytesPayload
			}
		}
		out = append(out, view)
	}
	return out
}

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

// TrajectoryByTaskID returns the planner trajectory for a completed run,
// or nil when the task's trajectory has been evicted or never existed.
// Reads are safe under concurrent access (RLock / D-025).
func (d *perTaskRunLoopDriver) TrajectoryByTaskID(taskID tasks.TaskID) *planner.Trajectory {
	d.trajMu.RLock()
	defer d.trajMu.RUnlock()
	return d.trajectories[taskID]
}

// projectMemoryBlocks shapes a memory.LLMContextPatch into the
// JSON-encodable map the planner's `<read_only_conversation_memory>`
// wrapper renders. Returns nil when the patch is empty — the wrapper
// is omitted entirely. V1.1 ships only the Conversation tier; the
// External tier remains nil pending a long-term memory phase.
func projectMemoryBlocks(patch memory.LLMContextPatch) *planner.MemoryBlocks {
	if len(patch.RecentTurns) == 0 && patch.Summary == "" {
		return nil
	}
	recent := make([]map[string]any, 0, len(patch.RecentTurns))
	for _, turn := range patch.RecentTurns {
		recent = append(recent, map[string]any{
			"user":      turn.UserMessage,
			"assistant": turn.AssistantResponse,
		})
	}
	conversation := map[string]any{
		"strategy":     string(patch.Strategy),
		"recent_turns": recent,
	}
	if patch.Summary != "" {
		conversation["summary"] = patch.Summary
	}
	return &planner.MemoryBlocks{Conversation: conversation}
}

// projectSkillsContext shapes a []skills.RankedSkill into the
// []any the planner's `<skills_context>` wrapper renders. Each
// element is a small map carrying the body fields the LLM consumes
// (name / title / description / steps). An empty input returns nil
// so the wrapper is omitted.
func projectSkillsContext(ranked []skills.RankedSkill) []any {
	if len(ranked) == 0 {
		return nil
	}
	out := make([]any, 0, len(ranked))
	for _, r := range ranked {
		entry := map[string]any{
			"name":  r.Skill.Name,
			"title": r.Skill.Title,
		}
		if r.Skill.Description != "" {
			entry["description"] = r.Skill.Description
		}
		if len(r.Skill.Steps) > 0 {
			entry["steps"] = r.Skill.Steps
		}
		out = append(out, entry)
	}
	return out
}

// skillKeywordStopwords lists the common English stopwords the
// keyword extractor drops before handing the query to the FTS5
// skills driver. The list is intentionally CONSERVATIVE — domain
// keywords ("api", "config", "auth", "tool") survive because they
// drive the BM25 ranker's signal. The list mirrors the standard
// short-stopword sets shipped with SQLite FTS5 tokenizers; it is
// fixed (operator-tunable lists are a Phase 91+ concern).
// Phase 83m (Item 4, D-156).
var skillKeywordStopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "but": {},
	"if": {}, "is": {}, "are": {}, "was": {}, "were": {}, "be": {},
	"been": {}, "being": {}, "have": {}, "has": {}, "had": {},
	"do": {}, "does": {}, "did": {}, "of": {}, "to": {}, "in": {},
	"on": {}, "at": {}, "for": {}, "with": {}, "by": {}, "from": {},
	"as": {}, "into": {}, "that": {}, "this": {}, "it": {}, "i": {},
	"you": {}, "we": {}, "they": {}, "my": {}, "your": {},
}

// maxSkillKeywords caps the number of terms the helper returns. A
// longer term list dilutes the BM25 signal without improving recall;
// 10 mirrors the standard search-keyword cap.
const maxSkillKeywords = 10

// extractSkillKeywords turns a raw task Query (a full sentence, with
// punctuation + articles + stopwords) into the keyword-shaped string
// the SQLite skills driver's FTS5 ranker performs best on. The
// pipeline is intentionally CONSERVATIVE: tokens that look like
// domain vocabulary survive; only the highest-noise common-English
// stopwords + 1-char tokens get dropped. Phase 83m (Item 4, D-156).
//
// Steps (in order):
//
//  1. Lowercase the input so the case-insensitive token comparison
//     matches the FTS5 tokenizer's default case-folding.
//  2. Split on whitespace + punctuation (every rune that is neither a
//     letter nor a digit acts as a separator). Apostrophes inside a
//     word ("operator's") are split — the driver tokenizes the same
//     way, so the result is a single contiguous letter run rather
//     than the contraction.
//  3. Drop tokens in `skillKeywordStopwords`.
//  4. Drop 1-character tokens — they carry no signal at the BM25
//     edge.
//  5. Deduplicate while preserving order — the first occurrence wins
//     so the operator-visible word order is preserved.
//  6. Cap at `maxSkillKeywords` (10) terms.
//
// Returns the space-joined keyword string. An empty result is
// possible for a pathological all-stopword input; the caller MUST
// fall back to the raw Query so Search still has signal.
func extractSkillKeywords(query string) string {
	if query == "" {
		return ""
	}
	lower := strings.ToLower(query)
	// Token boundary: any rune that is not a letter or a digit.
	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if len(tok) <= 1 {
			continue
		}
		if _, drop := skillKeywordStopwords[tok]; drop {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
		if len(out) >= maxSkillKeywords {
			break
		}
	}
	return strings.Join(out, " ")
}

// extractAssistantAnswer pulls the planner's natural-language answer
// out of a terminal Finish for the memory.AddTurn writeback. Phase 83i
// (D-152). The react planner's FinishGoal carries
// Payload = map[string]any{"answer": "<the LLM's answer>"}. Other
// planners may shape Payload differently; we accept any string-valued
// "answer" key and otherwise fall back to a Sprintf so something
// always lands in memory (matching CLAUDE.md §5 fail-loud — silent
// "nothing written" would lose the run's outcome).
func extractAssistantAnswer(fin planner.Finish) string {
	switch p := fin.Payload.(type) {
	case string:
		return p
	case map[string]any:
		if v, ok := p["answer"]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	if fin.Payload == nil {
		return string(fin.Reason)
	}
	return fmt.Sprintf("%v", fin.Payload)
}
