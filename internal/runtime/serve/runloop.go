// cmd/harbor/cmd_dev_runloop.go — the per-task RunLoop driver
// (closes issue #114 and issue #123).
//
// `harbor dev` previously had no production consumer for
// `steering.RunLoop` — a `start` request reached
// `tasks.TaskRegistry.Spawn` and the task sat there forever (no
// goroutine drove it through a Planner). The §17.5 audit's
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
//     This bridge is the closure of the deliberate carve-out:
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
// One RunLoop instance backs every spawned task (the RunLoop
// is concurrent-safe). The TaskID doubles as the RunID — the task
// IS the run at this layer (RFC §6.8). When a task.spawned event
// arrives:
//
//	q := identity.Quadruple{Identity: ev.Identity.Identity, RunID: string(payload.TaskID)}
//	rl.Run(ctx, steering.RunSpec{Planner: planner, Base: planner.RunContext{Quadruple: q, Goal: ...}, TaskID: payload.TaskID})
//
// The goal string is NOT carried on the task.spawned payload —
// `TaskSpawnedPayload` is a SafeSealed bookkeeping struct.
// The goal lives on the persisted `Task.Query` field; the driver
// looks it up via `taskReg.Get` after the spawn event arrives.
// (A later phase may extend the spawn payload with the goal to avoid
// the extra read; the current shape keeps the payload secret-safe.)
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

package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// RunLoopDriverOptions bundles the dependencies the driver
// consumes. Bus + RunLoop + Planner + TaskRegistry are all mandatory;
// a nil any of them returns ErrRunLoopDriverMisconfigured from
// NewRunLoopDriver. The TaskRegistry is what the driver calls
// MarkRunning / MarkComplete / MarkFailed on to advance the FSM
// (closes issue #123).
type RunLoopDriverOptions struct {
	Logger   *slog.Logger
	Bus      events.EventBus
	RunLoop  *steering.RunLoop
	Planner  planner.Planner
	Tasks    tasks.TaskRegistry // mandatory: the FSM the driver advances on Run exit
	TaskKind tasks.TaskKind     // KindForeground at V1; the driver spawns RunLoops for this kind

	// driveBackground widens the driver to ALSO
	// drive KindBackground tasks — the ones a planner-emitted SpawnTask
	// creates. False (the default / legacy test path) keeps the
	// foreground-only behaviour. Recursion is bounded at the spawn site
	// by the dev executor's absolute_max_spawn_depth cap, not here.
	DriveBackground bool

	// RunContext consumer wiring. All three of
	// memory / skillsDirectory / planningHints are OPTIONAL: a dev
	// stack that did not open the respective subsystem hands nil; the
	// driver projects the corresponding RunContext field to nil and
	// the planner omits the wrapper. The skills
	// surface is the `skills.Directory` — the bounded,
	// pinned-then-recent, capability-filtered `<skills_context>`
	// producer (the directory carries its own MaxEntries cap; the
	// pre-111d SkillStore.Search + skillsContextMax pair is deleted).
	Memory          memory.MemoryStore
	MemoryRecall    memory.RecallSettings
	SkillsDirectory *skills.Directory
	PlanningHints   *planner.PlanningHints

	// SkillStore, SessionPersonalSkills, and SessionSkillCutover are the
	// all-or-none run-start snapshot authority. SkillStore is the shared base
	// reader for non-session rungs AND the mandatory driver-owned search policy
	// for the frozen authorized candidates; SessionPersonalSkills owns
	// agent-selected session records; and SessionSkillCutover supplies the
	// declared tenant mode.
	SkillStore            skills.SkillStore
	SessionPersonalSkills *sessionoverlay.DurableStore
	SessionSkillCutover   sessionoverlay.CutoverModeReader

	// tool dispatch + Catalog projection +
	// Trajectory. The tool catalog is the shared catalog the rest of
	// the dev stack already populated (in-process tools, MCP-discovered
	// tools, etc.). MaxStepsRunLoop caps the runloop's outer step
	// counter (separate from the planner-internal cap that goes onto
	// react via PlannerConfig.MaxSteps).
	Catalog         tools.ToolCatalog
	Executor        steering.ToolExecutor
	MaxStepsRunLoop int

	// operator-declared GrantedScopes
	// threaded into the per-run catalog view's CatalogFilter. Tools
	// whose AuthScopes exceed this set are invisible to the planner.
	// Nil / empty list means no scopes granted (the existing latent
	// default before the plumb-through).
	GrantedScopes []string

	// the artifact store the multimodal
	// materializer reads from. Required only when `task.InputArtifactIDs`
	// is non-empty (text-only tasks never touch the store). A nil
	// store with input artifacts on the task degrades gracefully —
	// the materializer emits text-stub-only references the LLM
	// routes via the catalog.
	ArtifactStore artifacts.ArtifactStore

	// trajectory compression. `tokenBudget`
	// projects onto RunSpec.Base.Budget.TokenBudget (the per-run
	// runtime budget — a run option, never
	// planner state); `compression` is the assembly-built
	// planner.CompressionRunner the runloop invokes at each step
	// boundary when the budget is non-zero. Both zero/nil (the
	// default) = compression off, byte-identical behaviour.
	TokenBudget int
	Compression *planner.CompressionRunner

	// the per-agent attachment disposition policy
	// decoded from `multimodal.disposition` (the middle precedence
	// layer of the disposition resolution). Zero value = no agent
	// policy; the runtime default applies.
	DispositionPolicy planner.DispositionPolicy

	// tenantOverrides resolves an admin-set tenant default for the run's
	// LLM parameters at run start (model / extra-instructions /
	// temperature / max-tokens / reasoning-effort). OPTIONAL — a nil
	// resolver means "no tenant defaults to apply" (the run uses the
	// agent/config defaults). When supplied, the driver reads it ONCE per
	// run and pins the resolved snapshot into the run's RunContext so the
	// swap lands on this run (next-turn relative to the admin's set), never
	// mid-flight.
	TenantOverrides TenantOverrideResolver

	// sessionOverrides is the in-process pending-override Store the
	// `runs.set_overrides` Service writes into. The driver Consumes the
	// session's pending override at run start (one-shot) and composes it
	// OVER the tenant default (session › tenant › config). OPTIONAL — a
	// nil Store means "no session overrides" (the run uses tenant/config
	// only). It is the SAME Store handed to the runs Service so a set and
	// the consume meet.
	SessionOverrides *runsprotocol.Store

	// agentConfig resolves the agent's active config revision at run start
	// (the agent-config control plane). OPTIONAL — a nil registry means
	// "no agent-config projection" (the run uses every directory-visible
	// skill). When supplied with a non-empty agentConfigID, the run reads
	// the active revision ONCE at run start and pins the resolved
	// skills-set into the per-run projection so a config edit applies on
	// the NEXT run (next-turn-only), never mid-flight.
	AgentConfig   agentcfg.Registry
	AgentConfigID string
	// EnsureBootAgentLifecycle materialises only the configured default agent
	// after trusted direct task construction selected it for this run.
	EnsureBootAgentLifecycle agentcfg.BootLifecycleEnsurer

	// sessionOverlay resolves the SESSION-scoped safe-subset overlay (the
	// non-admin lower tier) at run start: the session's user prompt layer,
	// narrow-only source/tool disables (UNIONED into the admin exclusion set
	// — can only narrow, never widen), and ephemeral personal skills. Keyed
	// by the REAL (tenant, user, session) triple, so it is session-isolated.
	// OPTIONAL — nil means "no session overlay" (the run uses the admin agent
	// config only). The SAME store the session-safe Protocol verbs write into.
	SessionOverlay sessionoverlay.Store

	// runCompletionHook is the static `runtime.hooks.run_completion`
	// projection (nil when unset). At run start the driver resolves the
	// effective hook via the shared projection (agent-config over this yaml
	// over none) and pins it into the RunSpec, so an edit lands next-run.
	RunCompletionHook *steering.CompletionHookSpec

	// connectionDetacher drives the DETACH leg of run-start reconciliation:
	// at run start the driver detaches every MCP server that is attached but
	// no longer declared by the agent's active config revision (a removed
	// connection, or a rollback past an add), so the next run's projected
	// catalog excludes it. OPTIONAL — nil means no reconcile (the
	// backward-compatible path). Injected at the cmd/harbor boundary (it
	// imports the concrete MCP driver; this driver stays driver-agnostic).
	ConnectionDetacher projection.ConnectionDetacher

	// ConnectionReattacher drives the ATTACH leg of run-start reconciliation —
	// the symmetric twin of ConnectionDetacher, in the same reconcile call. At
	// run start the driver re-establishes every MCP server the agent's active
	// config revision DECLARES that the live registry does not carry under the
	// reconciling owner, which is what makes a runtime-added connection survive a
	// process restart and what makes a rollback that re-declares a removed
	// connection bring it back. OPTIONAL — nil yields the detach-only behaviour
	// byte-for-byte. Injected at the cmd/harbor + devstack boundary (it imports
	// the concrete MCP driver; this driver stays driver-agnostic).
	ConnectionReattacher projection.ConnectionReattacher

	// bootDeclaredMCP is the set of boot-declared (yaml) MCP server names the
	// reconcile MUST NEVER detach — nor attach (they carry the zero owner and are
	// attached by the boot loader). Nil/empty when no yaml server is declared.
	BootDeclaredMCP map[string]struct{}

	// OAuthProviderReconciler drives the run-start provider-reconcile leg: it
	// makes the reconciling owner's installed OAuth providers match the current
	// active revision (a rollback past an install uninstalls+closes; a rollback
	// of a removal re-installs). OPTIONAL — nil means no provider reconcile.
	// Owner-scoped exactly like the connection reconcile.
	OAuthProviderReconciler projection.OAuthProviderReconciler

	// namingDefault is the static `runtime.naming` fleet-default auto-naming
	// policy; the driver resolves the effective policy per run (agent-config
	// over this yaml over off). Opt-in, default off.
	NamingDefault config.RuntimeNamingConfig

	// sessionTitler is the session-registry seam the auto-naming trigger
	// writes/reads through (RecordCompletedTurn / AutoNamingState /
	// SetTitleAuto). The same *sessions.Registry the sessions Protocol routes
	// project over. Nil ⇒ no auto-naming trigger even when a policy resolves.
	SessionTitler steering.SessionTitler

	// namingLLM is the run's wrapped LLM client the ONE naming Complete call
	// flows through (governance/safety via ctx identity). Nil ⇒ no auto-naming
	// trigger.
	NamingLLM steering.NamingCompleter
}

// TenantOverrideResolver is the narrow read seam the run loop uses to
// resolve an admin-set tenant default at run start. The
// `*governance.TenantOverridePolicy` concrete satisfies it.
type TenantOverrideResolver interface {
	Get(ctx context.Context, tenant string) (governance.TenantOverrideSpec, bool, error)
}

// trackedTrajectory pairs a run's Trajectory with the per-run
// `*sync.RWMutex` that guards its append-only Steps slice. The same
// mutex is handed to the steering RunLoop via `RunSpec.TrajectoryMu`,
// so the loop's per-step append and the Enricher's tasks.get snapshot
// serialize against each other (the Trajectory type delegates
// concurrency to the Runtime by contract).
type trackedTrajectory struct {
	traj *planner.Trajectory
	mu   *sync.RWMutex
}

// RunLoopDriver subscribes to `task.spawned` and drives a
// RunLoop per spawned foreground task. The driver is constructed by
// bootDevStack and Closed during stack teardown.
type RunLoopDriver struct {
	logger          *slog.Logger
	bus             events.EventBus
	runLoop         *steering.RunLoop
	planner         planner.Planner
	tasks           tasks.TaskRegistry
	taskKind        tasks.TaskKind
	driveBackground bool // also drive KindBackground tasks

	// per-run consumer wiring; the canonical-skills work
	// — Directory as the skills surface. See driver opts godoc.
	memory                memory.MemoryStore
	memoryRecall          memory.RecallSettings
	skillsDirectory       *skills.Directory
	planningHints         *planner.PlanningHints
	skillStore            skills.SkillStore
	sessionPersonalSkills *sessionoverlay.DurableStore
	sessionSkillCutover   sessionoverlay.CutoverModeReader

	// tool dispatch + Catalog projection.
	catalog         tools.ToolCatalog
	executor        steering.ToolExecutor
	maxStepsRunLoop int

	// operator-declared GrantedScopes.
	grantedScopes []string

	// artifact store handle for the multimodal
	// materializer.
	artifactStore artifacts.ArtifactStore

	// trajectory compression projection.
	tokenBudget int
	compression *planner.CompressionRunner

	// per-agent attachment disposition policy.
	dispositionPolicy planner.DispositionPolicy

	// admin-set tenant-default override resolver (nil = none).
	tenantOverrides TenantOverrideResolver

	// session-level pending-override Store, Consumed at run start (nil = none).
	sessionOverrides *runsprotocol.Store

	// agent-config registry + the dev agent's registration id, read once at
	// run start to project the agent's active skills-set (nil = none).
	agentConfig              agentcfg.Registry
	agentConfigID            string
	ensureBootAgentLifecycle agentcfg.BootLifecycleEnsurer

	// session-scoped safe-subset overlay store, read once at run start to
	// compose the session user layer + narrow-only disables + personal skills
	// over the admin agent config (nil = none).
	sessionOverlay sessionoverlay.Store

	// runCompletionHook is the static run-completion hook default (from
	// `runtime.hooks.run_completion`); nil when unset. The driver resolves
	// the effective hook per run via the shared projection.
	runCompletionHook *steering.CompletionHookSpec

	// connectionDetacher + connectionReattacher + bootDeclaredMCP drive the
	// run-start reconcile's two connection passes (see the opts godoc). Both
	// concretes are nil when the MCP registry / catalog are absent (no
	// reconcile); a nil reattacher alone leaves the leg detach-only.
	connectionDetacher      projection.ConnectionDetacher
	connectionReattacher    projection.ConnectionReattacher
	bootDeclaredMCP         map[string]struct{}
	oauthProviderReconciler projection.OAuthProviderReconciler

	// session auto-naming wiring: the static fleet-default policy, the
	// session-registry titler seam, and the run's wrapped LLM client. All
	// three are needed for the trigger to fire; any nil = no auto-naming.
	namingDefault config.RuntimeNamingConfig
	sessionTitler steering.SessionTitler
	namingLLM     steering.NamingCompleter

	// per-task trajectory map for the Enricher seam.
	// Trajectories are stored before RunLoop.Run and retained after
	// completion for tasks.get enrichment. `trajMu` guards the MAP; each
	// entry carries its OWN per-run `*sync.RWMutex` (shared with the
	// steering RunLoop via RunSpec.TrajectoryMu) that guards the
	// Trajectory's append-only Steps slice — so a tasks.get of an
	// IN-FLIGHT run snapshots the steps without racing the run loop's
	// per-step append. An evicted task returns nil.
	trajMu       sync.RWMutex
	trajectories map[tasks.TaskID]*trackedTrajectory

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

// ErrRunLoopDriverMisconfigured fires when NewRunLoopDriver
// is called with a nil bus / RunLoop / planner. Driver invariant: all
// three are mandatory.
var ErrRunLoopDriverMisconfigured = errors.New("dev: per-task RunLoop driver missing a mandatory dependency")

// NewRunLoopDriver validates the opts and returns a stopped
// driver. Call Start before serving; call Close to drain.
func NewRunLoopDriver(opts RunLoopDriverOptions) (*RunLoopDriver, error) {
	if opts.Bus == nil {
		return nil, fmt.Errorf("%w: bus is nil", ErrRunLoopDriverMisconfigured)
	}
	if opts.RunLoop == nil {
		return nil, fmt.Errorf("%w: runLoop is nil", ErrRunLoopDriverMisconfigured)
	}
	if opts.Planner == nil {
		return nil, fmt.Errorf("%w: planner is nil", ErrRunLoopDriverMisconfigured)
	}
	if opts.Tasks == nil {
		return nil, fmt.Errorf("%w: tasks is nil", ErrRunLoopDriverMisconfigured)
	}
	snapshotDeps := 0
	for _, present := range []bool{opts.SkillStore != nil, opts.SessionPersonalSkills != nil, opts.SessionSkillCutover != nil} {
		if present {
			snapshotDeps++
		}
	}
	if snapshotDeps != 0 && snapshotDeps != 3 {
		return nil, fmt.Errorf("%w: skill snapshot dependencies must be wired together", ErrRunLoopDriverMisconfigured)
	}
	if opts.SkillsDirectory != nil && snapshotDeps == 0 {
		return nil, fmt.Errorf("%w: skills directory requires the complete run snapshot authority", ErrRunLoopDriverMisconfigured)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.TaskKind == "" {
		opts.TaskKind = tasks.KindForeground
	}
	return &RunLoopDriver{
		logger:                opts.Logger,
		bus:                   opts.Bus,
		runLoop:               opts.RunLoop,
		planner:               opts.Planner,
		tasks:                 opts.Tasks,
		taskKind:              opts.TaskKind,
		driveBackground:       opts.DriveBackground,
		memory:                opts.Memory,
		memoryRecall:          opts.MemoryRecall,
		skillsDirectory:       opts.SkillsDirectory,
		planningHints:         opts.PlanningHints,
		skillStore:            opts.SkillStore,
		sessionPersonalSkills: opts.SessionPersonalSkills,
		sessionSkillCutover:   opts.SessionSkillCutover,
		catalog:               opts.Catalog,
		executor:              opts.Executor,
		maxStepsRunLoop:       opts.MaxStepsRunLoop,
		grantedScopes:         append([]string(nil), opts.GrantedScopes...),
		artifactStore:         opts.ArtifactStore,
		// disposition policy passthrough.
		dispositionPolicy:        opts.DispositionPolicy,
		tenantOverrides:          opts.TenantOverrides,
		sessionOverrides:         opts.SessionOverrides,
		agentConfig:              opts.AgentConfig,
		agentConfigID:            opts.AgentConfigID,
		ensureBootAgentLifecycle: opts.EnsureBootAgentLifecycle,
		sessionOverlay:           opts.SessionOverlay,
		runCompletionHook:        opts.RunCompletionHook,
		connectionDetacher:       opts.ConnectionDetacher,
		connectionReattacher:     opts.ConnectionReattacher,
		bootDeclaredMCP:          opts.BootDeclaredMCP,
		oauthProviderReconciler:  opts.OAuthProviderReconciler,
		namingDefault:            opts.NamingDefault,
		sessionTitler:            opts.SessionTitler,
		namingLLM:                opts.NamingLLM,
		trajectories:             make(map[tasks.TaskID]*trackedTrajectory),
		tokenBudget:              opts.TokenBudget,
		compression:              opts.Compression,
	}, nil
}

// SessionOverridesStore returns the pending-override Store the driver
// consumes at run start. It is the seam a caller wires the runs.set_overrides
// service to so a set reaches the run; exposed so a caller can assert the two
// share one Store.
func (d *RunLoopDriver) SessionOverridesStore() *runsprotocol.Store {
	return d.sessionOverrides
}

// Start opens the admin-scoped subscription and launches the
// subscribe-loop goroutine. Idempotent: a second Start is a no-op.
// The supplied ctx anchors the subscription's lifetime — when ctx
// cancels (e.g. boot was aborted before Close), the subscription
// cancels along with it.
func (d *RunLoopDriver) Start(ctx context.Context) error {
	if d.started {
		return nil
	}
	d.subCtx, d.subCancel = context.WithCancel(context.Background())
	// Admin-scoped subscription: the driver listens across every
	// (tenant, user, session) triple via §6 rule 5's elevated-
	// subscription path. The bus auto-emits `audit.admin_scope_used`
	// observability of every admin-scoped subscribe is
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
func (d *RunLoopDriver) subscribeLoop() {
	defer d.subLoopWG.Done()
	for ev := range d.sub.Events() {
		d.handleEvent(ev)
	}
}

// drivesKind reports whether the driver runs a planner sub-run for a
// task of the given kind. It always drives its configured taskKind; with
// driveBackground set it additionally drives
// KindBackground.
func (d *RunLoopDriver) drivesKind(kind tasks.TaskKind) bool {
	if kind == d.taskKind {
		return true
	}
	return d.driveBackground && kind == tasks.KindBackground
}

// handleEvent dispatches one `task.spawned` event. The driver drives
// its configured `taskKind` (KindForeground) and — when driveBackground
// is set — KindBackground tasks too, the ones a
// planner-emitted SpawnTask creates. A KindBackground task is driven
// identically to a foreground one (a planner sub-run against its Query);
// recursion is bounded at the spawn site by the dev executor's
// absolute_max_spawn_depth cap. A malformed payload (wrong type) is
// logged and skipped — the event registration guarantees the shape, so a
// mismatch here is a programmer error.
func (d *RunLoopDriver) handleEvent(ev events.Event) {
	payload, ok := ev.Payload.(tasks.TaskSpawnedPayload)
	if !ok {
		d.logger.Warn("RunLoopDriver: task.spawned with unexpected payload type",
			slog.String("got", fmt.Sprintf("%T", ev.Payload)))
		return
	}
	if !d.drivesKind(payload.Kind) {
		// A kind this driver does not drive (e.g. a background task on a
		// foreground-only driver). The runtime dispatch executor / a
		// background-enabled driver owns those; this driver stays out.
		return
	}

	q := identity.Quadruple{
		Identity: ev.Identity.Identity,
		RunID:    string(payload.TaskID),
	}
	if err := identity.Validate(q.Identity); err != nil {
		d.logger.Warn("RunLoopDriver: task.spawned with incomplete identity",
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
// (a SafeSealed struct) does not carry the user-facing Query.
// The runtime executor reading the persisted Task.Query is a later
// phase; this driver wires the SHAPE (RunLoop drives a planner per
// spawned task) without re-introducing a goal-fetch path here.
// Operators that wire their own planner observe an empty Goal; the
// ReAct planner falls through to its default prompt builder which
// surfaces this case cleanly via the LLM's "I have no goal" response.
//
// # FSM bridge (closes issue #123)
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
//     to MarkFailed with code="cancelled". Rationale: the run did not
//     reach a successful goal; Failed is the correct terminal state. An
//     operator who wants explicit cancellation semantics calls
//     TaskRegistry.Cancel directly (which routes through the Cancel
//     path and uses StatusCancelled); the driver's ctx-cancel is a
//     forced-shutdown signal, not a deliberate cancel decision.
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
// resolveLLMOverrides resolves the effective per-run LLM-parameter
// override at run start by composing the two layers in precedence order
// **session › tenant › config**:
//
//   - the TENANT default (admin-set, persistent) is read from the
//     resolver — model / temperature / max-tokens / reasoning-effort /
//     additive extra-instructions;
//   - the SESSION override (operator-set via `runs.set_overrides`,
//     one-shot) is Consumed from the Store and, where present, WINS over
//     the tenant default per field; it also carries the full
//     system-prompt REPLACE (a session-only affordance).
//
// A nil result means "no overrides — use the agent/config defaults". A
// tenant-resolver error is returned (the caller fails the run loudly);
// the session Consume cannot error (in-process map read).
func (d *RunLoopDriver) resolveLLMOverrides(ctx context.Context, agentID string, q identity.Quadruple) (*planner.LLMOverrides, error) {
	// Tenant arm.
	var tenant *planner.LLMOverrides
	if d.tenantOverrides != nil {
		spec, set, err := d.tenantOverrides.Get(ctx, q.TenantID)
		if err != nil {
			return nil, err
		}
		if set {
			tenant = &planner.LLMOverrides{
				Model:             spec.Model,
				Temperature:       spec.Temperature,
				MaxTokens:         spec.MaxTokens,
				ReasoningEffort:   spec.ReasoningEffort,
				ExtraInstructions: spec.ExtraInstructions,
			}
		}
	}
	// Per-agent arm — the agent-config LLM-params section (admin-pinned,
	// versioned), resolved from the active revision via the shared projection
	// (the devstack twin calls the SAME helper, so the two binaries cannot
	// drift). It overrides the tenant-wide baseline per field.
	agentLayer, err := projection.ActiveLLMOverrides(ctx, d.agentConfig, agentID, q)
	if err != nil {
		return nil, err
	}
	// Session arm — Consume the one-shot pending override (read-once).
	var session *runsprotocol.PendingOverride
	if d.sessionOverrides != nil {
		if po, found := d.sessionOverrides.Consume(q.Identity); found {
			session = &po
		}
	}
	// Compose via the single production function (shared with the devstack
	// twin + the integration test — CLAUDE.md §17.4): session › per-agent ›
	// tenant-wide baseline.
	return runsprotocol.ComposeLLMOverrides(session, agentLayer, tenant), nil
}

// projectAgentConfigSkills resolves the agent's active config revision at
// run start and, when it pins a skills membership, narrows the directory
// views to that set (the agent-config control plane's run-start
// projection). The logic is shared verbatim with the devstack twin via the
// projection package so the two binaries cannot drift (CLAUDE.md §17.6).
func (d *RunLoopDriver) projectAgentConfigSkills(ctx context.Context, agentID string, q identity.Quadruple, views []skills.SkillView) ([]skills.SkillView, error) {
	return projection.ActiveSkillViews(ctx, d.agentConfig, d.sessionOverlay, agentID, q, views)
}

// captureRunSkillSnapshot resolves the selected agent's AGENT/USER membership
// once, builds the fence-stable composite resolver once, and binds that reader
// to the exact run quadruple. A driver with no skill snapshot dependencies is
// the valid no-skills subsystem shape; partial wiring is rejected by
// NewRunLoopDriver.
func (d *RunLoopDriver) captureRunSkillSnapshot(ctx context.Context, effectiveAgentID string, q identity.Quadruple) (skills.RunSkillReaderSnapshot, bool, error) {
	if d.skillStore == nil {
		return skills.RunSkillReaderSnapshot{}, false, nil
	}
	membership, err := projection.ActiveSessionSkillMembership(ctx, d.agentConfig, effectiveAgentID, q)
	if err != nil {
		return skills.RunSkillReaderSnapshot{}, false, fmt.Errorf("capture membership: %w", err)
	}
	resolver, err := sessionoverlay.NewSessionSkillResolver(ctx, sessionoverlay.SessionSkillResolverConfig{
		Run:        q,
		AgentID:    effectiveAgentID,
		Base:       d.skillStore,
		Personal:   d.sessionPersonalSkills,
		Cutover:    d.sessionSkillCutover,
		Membership: membership,
	})
	if err != nil {
		return skills.RunSkillReaderSnapshot{}, false, fmt.Errorf("build resolver: %w", err)
	}
	snapshot, err := skills.NewRunSkillReaderSnapshot(q, effectiveAgentID, resolver)
	if err != nil {
		return skills.RunSkillReaderSnapshot{}, false, fmt.Errorf("bind run reader: %w", err)
	}
	return snapshot, true, nil
}

// projectAgentConfigCatalog builds the run's planner catalog view, applying
// the agent's active-config tool exposure (paused MCP servers + disabled
// tools excluded) via the SAME shared projection the devstack twin uses
// (CLAUDE.md §17.6). Next-turn-only; the live transport stays warm.
func (d *RunLoopDriver) projectAgentConfigCatalog(ctx context.Context, agentID string, q identity.Quadruple, filter tools.CatalogFilter) (tools.PlannerCatalogView, error) {
	return projection.ActivePlannerCatalogView(ctx, d.agentConfig, d.sessionOverlay, agentID, q, d.catalog, filter)
}

// projectAgentConfigPromptLayers overlays the agent's durable layered system
// prompt (operator base + optional user layer) resolved from the active
// config onto the run's resolved override bundle at run start, via the SAME
// shared projection the devstack twin uses (CLAUDE.md §17.6). Next-turn-only;
// the immutable per-run snapshot is undisturbed for in-flight runs.
func (d *RunLoopDriver) projectAgentConfigPromptLayers(ctx context.Context, agentID string, q identity.Quadruple, ov *planner.LLMOverrides) (*planner.LLMOverrides, error) {
	return projection.ApplyPromptLayers(ctx, d.agentConfig, d.sessionOverlay, agentID, q, ov)
}

// projectRunCompletionHook resolves the effective run-completion hook for
// this run at run start via the SAME shared projection the devstack twin
// uses (CLAUDE.md §17.6): agent-config `hooks` section over the static
// `runtime.hooks.run_completion` yaml over none. Next-turn-only; an edit is
// invisible to an in-flight run.
func (d *RunLoopDriver) projectRunCompletionHook(ctx context.Context, agentID string, q identity.Quadruple) (*steering.CompletionHookSpec, error) {
	hook, _, err := projection.ActiveRunCompletionHook(ctx, d.agentConfig, agentID, q, d.runCompletionHook)
	return hook, err
}

// projectNaming resolves the effective session auto-naming spec for this run
// at run start via the SAME shared projection the devstack twin uses (the shared projection,
// CLAUDE.md §17.6): agent-config `naming` section over the static
// `runtime.naming` yaml over off. Returns nil when no policy is active OR the
// naming dependencies (titler + wrapped LLM client) are not wired — opt-in,
// default off. The naming model resolves to the policy's model, else the run's
// effective model override, else "" (the client default). Next-turn-only.
func (d *RunLoopDriver) projectNaming(ctx context.Context, agentID string, q identity.Quadruple, ov *planner.LLMOverrides) (*steering.NamingSpec, error) {
	if d.sessionTitler == nil || d.namingLLM == nil {
		return nil, nil
	}
	res, active, err := projection.ActiveNamingPolicy(ctx, d.agentConfig, agentID, q, d.namingDefault)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, nil
	}
	model := res.Model
	if model == "" && ov != nil && ov.Model != nil {
		model = *ov.Model
	}
	return &steering.NamingSpec{
		Policy: res.Policy,
		Titler: d.sessionTitler,
		LLM:    d.namingLLM,
		Model:  model,
	}, nil
}

// reconcileConnections runs run-start MCP reconciliation before the catalog is
// projected. Its connection leg is BIDIRECTIONAL: it DETACHES every MCP server
// attached but no longer declared by the agent's active config revision (a
// removed connection or a rollback past an add), so this and every later run's
// projected catalog excludes it, the registry no longer lists it, and the
// transport drains — and then RE-ATTACHES every server the active revision
// DECLARES that the live registry does not carry under the reconciling owner,
// which is how a runtime-added connection survives a process restart and how a
// rollback that re-declares a removed connection brings it back. Detach runs
// first. The attach pass is skipped entirely when no attach concrete is wired.
// Exposure correctness is next-turn and independent of teardown; teardown is
// process-global — a DIFFERENT session's in-flight run whose next step calls
// the detached server fails LOUDLY (typed catalog not-found / closed
// transport), never a hang or a silent success (see the
// projection.ReconcileConnections godoc for the full honest-semantics
// contract). A reconcile error is logged LOUD (never silently swallowed,
// CLAUDE.md §13) but does NOT fail the run — neither detach nor re-attach is a
// run precondition; a run continues even if an old transport refuses to close or
// a declared third party is unreachable. Shared verbatim with the devstack twin
// via the projection package (CLAUDE.md §17.6).
// reconcileConnectionsSweepBudget bounds the WHOLE run-start connection
// reconcile when an attach concrete is wired. Each individual re-attach is
// separately bounded inside that concrete; this caps the aggregate so a revision
// declaring several unreachable servers cannot stack their per-connection bounds
// onto every run's start. It is a runtime constant, not an operator knob.
const reconcileConnectionsSweepBudget = 45 * time.Second

// splitReconcileErrors partitions a joined run-start reconcile error into the
// three buckets the caller treats differently: `other` (a detach failure or a
// fail-loud read — logged Error and aborts the remaining legs, the shipped
// behaviour), `reattachLoud` (a refused or unreachable declared connection —
// logged Error, already reported on its own canonical event, does NOT abort), and
// `suppressed` (a re-attach parked by its bounded retry window — logged Debug).
// It walks the joined tree rather than string-matching, so a wrapped leaf is
// classified by its sentinel.
func splitReconcileErrors(err error) (other, reattachLoud, suppressed error) {
	if err == nil {
		return nil, nil, nil
	}
	var others, louds, quiets []error
	for _, leaf := range flattenJoined(err) {
		switch {
		case errors.Is(leaf, ErrReattachSuppressed):
			quiets = append(quiets, leaf)
		case errors.Is(leaf, projection.ErrReconcileReattach):
			louds = append(louds, leaf)
		default:
			others = append(others, leaf)
		}
	}
	return errors.Join(others...), errors.Join(louds...), errors.Join(quiets...)
}

// flattenJoined returns the leaves of an errors.Join tree (the error itself when
// it is not a join). Only the join shape is unwrapped: a leaf's own %w chain is
// left intact so errors.Is still sees its sentinels.
func flattenJoined(err error) []error {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	var out []error
	for _, child := range joined.Unwrap() {
		out = append(out, flattenJoined(child)...)
	}
	return out
}

func (d *RunLoopDriver) reconcileConnections(ctx context.Context, agentID string, q identity.Quadruple) {
	// The PROVIDER-reconcile leg runs independently of the connection detacher:
	// make the reconciling owner's installed OAuth providers match the current
	// active revision (a rollback past an install uninstalls+closes; a rollback
	// of a removal re-installs). Owner-scoped, so a run for owner A never
	// closes tenant-B's provider. A reconcile error is logged LOUD but does not
	// fail the run.
	if d.oauthProviderReconciler != nil {
		changed, perr := projection.ReconcileOAuthProviders(ctx, d.agentConfig, agentID, q, d.oauthProviderReconciler)
		if perr != nil {
			d.logger.ErrorContext(ctx, "RunLoopDriver: run-start OAuth-provider reconcile failed",
				slog.String("agent_id", agentID), slog.String("run_id", q.RunID), slog.String("err", perr.Error()))
		} else if changed > 0 {
			d.logger.InfoContext(ctx, "RunLoopDriver: run-start OAuth-provider reconcile applied",
				slog.String("agent_id", agentID), slog.Int("changed", changed))
		}
	}
	if d.connectionDetacher == nil {
		return
	}
	// The reconcile's two connection passes (detach, then attach) run under a
	// BOUNDED total when an attach concrete is wired: each attach is separately
	// bounded inside that concrete, and this budget caps the whole sweep so a
	// revision declaring many unreachable servers cannot stack their bounds onto
	// every run's start. Applied only when a reattacher is present, so the
	// detach-only path keeps its exact prior ctx.
	reconcileCtx := ctx
	if d.connectionReattacher != nil {
		var cancel context.CancelFunc
		reconcileCtx, cancel = context.WithTimeout(ctx, reconcileConnectionsSweepBudget)
		defer cancel()
	}
	detached, attached, err := projection.ReconcileConnections(reconcileCtx, d.agentConfig, agentID, q,
		d.connectionDetacher, d.connectionReattacher, d.bootDeclaredMCP)
	if err != nil {
		// Partition the joined error. A re-attach parked by its bounded retry
		// window is NOT a new failure (it was counted, and its count rides the next
		// emitted lifecycle event), so it logs at Debug — otherwise a permanently
		// unreachable declared server would write an Error line on every run start.
		// A loud re-attach failure logs at Error but does NOT skip the remaining
		// legs: one refused third party must not stop the discovery-allowance
		// re-apply for every connection. Anything else (a detach failure, a
		// fail-loud read) keeps its shipped behaviour and returns.
		other, reattachLoud, suppressed := splitReconcileErrors(err)
		if suppressed != nil {
			d.logger.DebugContext(ctx, "RunLoopDriver: run-start MCP re-attach suppressed by its retry window",
				slog.String("agent_id", agentID), slog.String("run_id", q.RunID), slog.String("detail", suppressed.Error()))
		}
		if reattachLoud != nil {
			d.logger.ErrorContext(ctx, "RunLoopDriver: run-start MCP re-attach failed",
				slog.String("agent_id", agentID), slog.String("run_id", q.RunID), slog.String("err", reattachLoud.Error()))
		}
		if other != nil {
			d.logger.ErrorContext(ctx, "RunLoopDriver: run-start MCP reconcile detach failed",
				slog.String("agent_id", agentID), slog.String("run_id", q.RunID), slog.String("err", other.Error()))
			return
		}
	}
	if detached > 0 {
		d.logger.InfoContext(ctx, "RunLoopDriver: run-start MCP reconcile detached servers",
			slog.String("agent_id", agentID), slog.Int("detached", detached))
	}
	if attached > 0 {
		d.logger.InfoContext(ctx, "RunLoopDriver: run-start MCP reconcile re-attached declared servers",
			slog.String("agent_id", agentID), slog.Int("attached", attached))
	}
	// The ALLOWANCE-reconcile leg: re-derive each still-declared runtime-added
	// connection's OAuth-discovery allow-list from the current revision and
	// re-apply it live (a full idempotent re-prune), so a rollback / set_revision
	// past a grant revokes the origin on the live registry. Owner-scoped exactly
	// like the detach leg. The detacher concrete satisfies the reconciler seam;
	// a driver that does not is a no-op.
	if rec, ok := d.connectionDetacher.(projection.DiscoveryOriginReconciler); ok {
		applied, aerr := projection.ReconcileDiscoveryOrigins(ctx, d.agentConfig, agentID, q, rec)
		if aerr != nil {
			d.logger.ErrorContext(ctx, "RunLoopDriver: run-start MCP discovery-allowance reconcile failed",
				slog.String("agent_id", agentID), slog.String("run_id", q.RunID), slog.String("err", aerr.Error()))
			return
		}
		if applied > 0 {
			d.logger.InfoContext(ctx, "RunLoopDriver: run-start MCP discovery-allowance reconcile re-applied",
				slog.String("agent_id", agentID), slog.Int("reapplied", applied))
		}
	}
}

func (d *RunLoopDriver) runOne(q identity.Quadruple, taskID tasks.TaskID) {
	// Build the identity-scoped ctx the TaskRegistry needs. We attach
	// the triple via identity.With (the same call site §6 mandates for
	// every identity-scoped storage method). The ctx is derived from
	// d.subCtx so Close still bounds the Mark* calls.
	taskCtx, idErr := identity.With(d.subCtx, q.Identity)
	if idErr != nil {
		// Pre-Run identity attachment failed — the run never starts.
		// This is a programmer error: handleEvent already validated the
		// identity. Log loud and bail.
		d.logger.Warn("RunLoopDriver: identity.With failed before Run",
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
		d.logger.Warn("RunLoopDriver: MarkRunning failed; skipping Run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", err.Error()))
		return
	}

	// build the per-run consumer state BEFORE
	// handing the RunSpec to the RunLoop. The four primitives the
	// 83-band shipped now have a real production consumer.
	//
	// Step 1: fetch the task record. It carries the user-facing Query —
	// which becomes the run's `Goal` (the planner-visible goal starts
	// equal to the user's request; runtime REDIRECT can mutate it later,
	// see RunContext.Goal godoc) — AND the caller-named agent this run
	// executes under.
	//
	// ORDERING: this read runs BEFORE the run-start reconcile below,
	// because the reconcile legs are owner-scoped by (tenant, agent) and
	// must reconcile the RUN's effective agent, not the boot default. A
	// reconcile that ran first would tear down the wrong owner's
	// connections on every caller-named run.
	task, gErr := d.tasks.Get(taskCtx, taskID)
	if gErr != nil {
		d.logger.Warn("RunLoopDriver: tasks.Get failed; failing run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", gErr.Error()))
		if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    "runtime_fetch_error",
			Message: fmt.Sprintf("tasks.Get: %v", gErr),
		}); fErr != nil {
			d.logger.Warn("RunLoopDriver: MarkFailed(runtime_fetch_error) failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", fErr.Error()))
		}
		return
	}

	// The run's EFFECTIVE config agent id: the caller-named agent when
	// the request named one (already validated at the Protocol edge),
	// else the runtime's boot-configured default. It is a per-run LOCAL
	// threaded as a parameter — never a field on the driver, which is a
	// shared compiled artifact serving concurrent runs under different
	// agents (the concurrent-reuse contract).
	//
	// It selects CONFIGURATION only. The southbound provenance / RFC 8693
	// acting principal stays boot-derived; see the WithInvokingAgent call
	// site below.
	effectiveAgentID := d.agentConfigID
	if task.AgentID != "" {
		effectiveAgentID = task.AgentID
	}
	if d.ensureBootAgentLifecycle != nil && effectiveAgentID != "" && effectiveAgentID == d.agentConfigID {
		if err := d.ensureBootAgentLifecycle(taskCtx, q.Identity, effectiveAgentID); err != nil {
			d.logger.ErrorContext(taskCtx, "RunLoopDriver: boot agent lifecycle unavailable; failing run",
				slog.String("task_id", string(taskID)), slog.String("run_id", q.RunID),
				slog.String("agent_id", effectiveAgentID), slog.String("err", err.Error()))
			if markErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{Code: "runtime_fetch_error", Message: "boot agent lifecycle: " + err.Error()}); markErr != nil {
				d.logger.Warn("RunLoopDriver: MarkFailed(runtime_fetch_error) failed", slog.String("task_id", string(taskID)), slog.String("err", markErr.Error()))
			}
			return
		}
	}

	// Capture ONE immutable skill-reader authority after effective agent
	// selection and before any Directory/tool consumer runs. The task context
	// carries the exact run quadruple so Directory.ResolveSkillReader selects
	// this snapshot rather than its boot-time fallback. A resolver/membership
	// failure is terminal: running with a wider or divergent skill view would be
	// an authority bypass.
	skillSnapshot, hasSkillSnapshot, snapshotErr := d.captureRunSkillSnapshot(taskCtx, effectiveAgentID, q)
	if snapshotErr != nil {
		d.logger.ErrorContext(taskCtx, "RunLoopDriver: skill snapshot failed; failing run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("agent_id", effectiveAgentID),
			slog.String("err", snapshotErr.Error()))
		if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    "runtime_fetch_error",
			Message: "skills snapshot: " + snapshotErr.Error(),
		}); fErr != nil {
			d.logger.Warn("RunLoopDriver: MarkFailed(runtime_fetch_error) failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", fErr.Error()))
		}
		return
	}
	if hasSkillSnapshot {
		var runIdentityErr error
		taskCtx, runIdentityErr = identity.WithRun(taskCtx, q.Identity, q.RunID)
		if runIdentityErr != nil {
			d.logger.ErrorContext(taskCtx, "RunLoopDriver: skill snapshot identity failed; failing run",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", runIdentityErr.Error()))
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{Code: "runtime_fetch_error", Message: "skills snapshot identity: " + runIdentityErr.Error()}); fErr != nil {
				d.logger.Warn("RunLoopDriver: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)), slog.String("err", fErr.Error()))
			}
			return
		}
		taskCtx = skills.WithRunSkillReaderSnapshot(taskCtx, skillSnapshot)
	}

	// Run-start reconciliation (detach leg): before projecting the catalog,
	// detach any MCP server the agent's active revision no longer declares
	// (a removed connection / a rollback past an add). Exposure is next-turn;
	// teardown is process-global (an in-flight run calling a detached server
	// fails loud — see the reconcileConnections wrapper doc).
	d.reconcileConnections(taskCtx, effectiveAgentID, q)

	// Compile the per-task output schema ONCE at run start (the compile
	// the run consumes). A compile failure fails the run LOUD with the
	// output_invalid terminal code — the LLM is never called on a
	// degraded run (CLAUDE.md §13; the schema already passed the Protocol
	// edge, so a failure here means corruption or version skew, never a
	// silent skip). Nil for the common schemaless task.
	var compiledSchema *planner.OutputSchemaValidator
	if len(task.OutputSchema) > 0 {
		cs, cErr := planner.CompileOutputSchema(task.OutputSchema)
		if cErr != nil {
			d.logger.ErrorContext(taskCtx, "RunLoopDriver: output-schema compile failed; failing run",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", cErr.Error()))
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    planner.TaskErrorCodeOutputInvalid,
				Message: "output-schema compile failed: " + cErr.Error(),
			}); fErr != nil {
				d.logger.Warn("RunLoopDriver: MarkFailed(output_invalid) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		compiledSchema = cs
	}

	// Step 2: fetch identity-scoped memory + the skills-directory
	// view. Each is OPTIONAL — a stack without the subsystem
	// configured leaves the corresponding field nil and the planner
	// omits the wrapper. A store-side error is LOUD per CLAUDE.md §5
	// fail-loud: the run fails with the wrapped error, the LLM is
	// never called, and the operator sees a clear
	// `runtime_fetch_error` on the task.
	//
	// Memory + skills are SESSION-scoped per RFC §6.6/§6.7 (memory
	// spans runs within a session; skills are stored per-session). The
	// fetch quadruple zeroes RunID so the run inherits the session's
	// accumulated state rather than seeing only its own (empty) per-run
	// slice. Directory wiring.
	sessionQ := identity.Quadruple{Identity: q.Identity}
	var memBlocks *planner.MemoryBlocks
	if d.memory != nil {
		mb, mErr := runctx.FetchMemoryBlocks(taskCtx, d.memory, sessionQ, task.Query, d.memoryRecall, d.logger)
		if mErr != nil {
			d.logger.Warn("RunLoopDriver: FetchMemoryBlocks failed; failing run",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: fmt.Sprintf("FetchMemoryBlocks: %v", mErr),
			}); fErr != nil {
				d.logger.Warn("RunLoopDriver: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		memBlocks = mb
	}

	var skillsCtx []any
	if d.skillsDirectory != nil {
		// the skills Directory is the
		// `<skills_context>` producer — a bounded, STABLE
		// pinned-then-recent browse window (identity-scoped via
		// taskCtx, capability-filtered against the run's visible-tool
		// set, redacted). Per-query relevance retrieval is the LLM's
		// job via the `skill_search` meta-tool (107c); a stable
		// prompt prefix beats a per-turn query-churned block (the
		// manifest-pattern / KV-cache framing).
		views, sErr := d.skillsDirectory.View(taskCtx, skills.DirectoryCapability{
			AllowedTools: tools.VisibleNames(d.catalog, tools.CatalogFilter{
				TenantID:      q.TenantID,
				UserID:        q.UserID,
				SessionID:     q.SessionID,
				GrantedScopes: d.grantedScopes,
			}),
		})
		if sErr != nil {
			d.logger.Warn("RunLoopDriver: skills Directory.View failed; failing run",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", sErr.Error()))
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: fmt.Sprintf("skills Directory.View: %v", sErr),
			}); fErr != nil {
				d.logger.Warn("RunLoopDriver: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		skillsCtx = runctx.ProjectSkillsDirectory(views)
	}

	// Step 3: per-run RepairCounters. ONE pointer per run, threaded
	// onto RunContext; the repair pipeline increments it.
	// (counters scope to RunContext, not the planner artifact).
	counters := &planner.RepairCounters{}

	// Step 4: per-run Trajectory + the per-run
	// Catalog view. The Trajectory is appended to by the runloop
	// after every non-Finish, non-RequestPause step; without it the
	// planner sees an empty trajectory every step and (with a real
	// LLM) sends the identical prompt repeatedly. The Catalog view
	// is the planner-facing schema-only projection of the production
	// catalog under the run's identity scope; without it the
	// `<available_tools>` section renders empty and the LLM has no
	// tool affordance.
	traj := &planner.Trajectory{Query: task.Query}
	// Per-run guard for the trajectory's append-only Steps slice. Shared
	// between the steering RunLoop's per-step append (RunSpec.TrajectoryMu)
	// and the Enricher's tasks.get snapshot (TrajectoryByTaskID) so an
	// in-flight read never races the append.
	trajMu := &sync.RWMutex{}
	var catalogView planner.ToolCatalogView
	if d.catalog != nil {
		// the catalog view's CatalogFilter
		// now receives the operator-configured `tools.granted_scopes`
		// list. Tools whose AuthScopes exceed this set are invisible
		// to the planner; an empty list preserves the prior behaviour
		// (tools without AuthScopes are always visible; tools with
		// AuthScopes are filtered out).
		// the per-run view is the promoted
		// `tools.NewPlannerView` — constructed per run (never cached;
		// the filter carries the run's identity triple) — wrapped with the
		// agent-config tool-exposure projection: a paused MCP server's tools
		// and any disabled tool are excluded from THIS run's view
		// (next-turn-only; the live transport stays warm). The active
		// revision is read ONCE at run start; a registry read error fails the
		// run LOUDLY (the registry IS the runtime StateStore — an error means
		// the runtime is unhealthy). The shared projection keeps the cmd +
		// devstack views identical (CLAUDE.md §17.6).
		view, vErr := d.projectAgentConfigCatalog(taskCtx, effectiveAgentID, q, tools.CatalogFilter{
			TenantID:      q.TenantID,
			UserID:        q.UserID,
			SessionID:     q.SessionID,
			GrantedScopes: d.grantedScopes,
		})
		if vErr != nil {
			d.logger.ErrorContext(taskCtx, "RunLoopDriver: agent-config tool-exposure projection failed; failing run",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", vErr.Error()))
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    "runtime_fetch_error",
				Message: "agent-config tool-exposure projection: " + vErr.Error(),
			}); fErr != nil {
				d.logger.Warn("RunLoopDriver: MarkFailed(runtime_fetch_error) failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		catalogView = view
	}

	// wire the planner's event-emit closure so
	// `planner.decision` / `planner.finish` / `planner.repair_guidance_injected`
	// reach the bus. Without this the entire planner-side telemetry
	// stream is silent (operators / Console / inspect-runs see only
	// llm.cost.recorded). The closure (a promoted constructor)
	// stamps the run's identity quadruple on every event
	// and Warns loudly on publish failure, so a bus-close mid-run logs
	// rather than races. The driver-lifetime d.subCtx bounds every
	// publish (closing the correction): on the durable bus
	// driver, persistence stops at driver Close instead of late emits
	// writing past teardown.
	emit := events.IdentityStampingEmitterContext(d.subCtx, d.bus, q, d.logger)

	// Compose the caller-supplied memory block into the run's external
	// memory tier, if the `start` that created this task carried one.
	//
	// This runs AFTER the emitter is built, and the ordering is
	// load-bearing rather than incidental: the admission event is the
	// only Protocol-visible signal that caller-asserted content entered
	// this run, and emitting it through a nil emitter would drop it
	// silently. The phase smoke pins the line ordering mechanically.
	//
	// It runs OUTSIDE the `d.memory != nil` branch above on purpose:
	// composition must also work when the runtime has no memory subsystem
	// configured at all (memBlocks stays nil and FetchMemoryBlocks is
	// never called) and when a configured session simply has no stored
	// memory yet (ProjectMemoryBlocks returns nil).
	//
	// A payload the composer refuses fails the run LOUD. The caller was
	// told its memory would reach the model; running without it and
	// reporting success is the silent degradation this field exists to
	// avoid (CLAUDE.md §13).
	if len(task.CallerMemory) > 0 {
		composed, cmErr := runctx.ComposeCallerMemory(memBlocks, task.CallerMemory)
		if cmErr != nil {
			d.logger.ErrorContext(taskCtx, "RunLoopDriver: caller-memory composition failed; failing run",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", cmErr.Error()))
			if fErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    planner.TaskErrorCodeRunLoopError,
				Message: "caller-memory composition failed: " + cmErr.Error(),
			}); fErr != nil {
				d.logger.Warn("RunLoopDriver: MarkFailed after caller-memory-composition error failed",
					slog.String("task_id", string(taskID)),
					slog.String("err", fErr.Error()))
			}
			return
		}
		memBlocks = composed
		// Size only, never content — the payload is caller-controlled
		// bytes (CLAUDE.md §7 rules 6-7).
		emit(events.Event{
			Type:       memory.EventTypeMemoryCallerBlockAdmitted,
			Identity:   q,
			OccurredAt: time.Now(),
			Payload: memory.CallerBlockAdmittedPayload{
				Bytes: task.CallerMemoryWireBytes,
				Tier:  runctx.ExternalTierName,
				Key:   runctx.CallerSuppliedKey,
			},
		})
	}

	// per-run OnToolDispatched hook that advances the
	// task's `ToolCount` registry-side by `count` after every
	// successful tool dispatch (count is 1 for a CallTool, len(Branches)
	// for a CallParallel). The dev binary closes the seam from the
	// runloop's side (the executor returned without error) to the
	// tasks.TaskRegistry surface the Console Tasks page reads. A
	// best-effort log + non-fatal continuation would mask a counter
	// drift the operator depends on for visibility — the hook surfaces
	// an IncrementToolCount error loud, matching §13.
	dispatchHook := func(hookCtx context.Context, count int) error {
		for range count {
			if err := d.tasks.IncrementToolCount(hookCtx, taskID); err != nil {
				return fmt.Errorf("tasks.IncrementToolCount(%q): %w", taskID, err)
			}
		}
		return nil
	}

	// per-run OnChunk closure. Translates bifrost streaming
	// deltas into `llm.completion.chunk` bus events under the run's
	// identity quadruple, with identity on the Event ENVELOPE (the
	// trap the promoted constructor encodes —; see
	// `llm.NewChunkPublisher`'s godoc for the 280-rejected-chunks
	// history). Per the concurrent-reuse contract the closure is per-run on the stack; N
	// concurrent runs see N independent closures. The one-line adapter
	// bridges the constructor's string-typed kind (import direction:
	// `planner` imports `llm`, so `llm` cannot name `planner.ChunkKind`).
	// The driver-lifetime d.subCtx bounds every publish.
	chunkPub := llm.NewChunkPublisherContext(d.subCtx, d.bus, q, string(taskID), d.logger)
	onChunk := func(delta string, done bool, kind planner.ChunkKind) {
		// The run-level structured-output streaming posture, carried to
		// the per-task path: on a schema-constrained task, SUPPRESS
		// assistant-content and reasoning token DELTAS at this OnChunk →
		// llm.completion.chunk seam — a validate-and-retry loop cannot
		// retract already-streamed
		// tokens, so the validated answer arrives once via the task
		// envelope. Step-boundary `done` signals still fire (turn
		// boundaries stay observable) but forward with an EMPTY delta —
		// never the done chunk's own text — so no token content leaks on
		// the schema path regardless of driver flush behaviour;
		// tool-dispatch events are unaffected.
		if compiledSchema != nil {
			if !done {
				return
			}
			chunkPub("", done, string(kind))
			return
		}
		chunkPub(delta, done, string(kind))
	}

	// pre-resolve operator-uploaded input
	// artifacts so the planner's first-turn materializer renders them
	// as multimodal Content.Parts (promoted policy).
	// The runloop clears `Base.InputArtifacts` after the first
	// step (per `runloop.go::spec.Base.InputArtifacts = nil` at the
	// end of the per-step build) so subsequent steps see an empty
	// slice.
	// the disposition is resolved per attachment
	// by the planner-homed pure resolver (hint > agent policy >
	// runtime default); this driver is a THIN caller. The helper logs
	// the winning layer / degradation facts and emits one
	// `task.input_disposition.resolved` event per attachment through
	// the same identity-stamping emitter the planner telemetry uses.
	inputArtifacts := runctx.ResolveInputArtifacts(taskCtx, d.artifactStore, q, task.InputArtifactIDs, d.logger, runctx.InputArtifactOptions{
		Hints:   runctx.DispositionHints(task.InputArtifactDispositions),
		Policy:  d.dispositionPolicy,
		Catalog: catalogView,
		Emit:    emit,
	})

	// pre-resolve the session-artifact manifest so
	// the planner renders a read-only `<session_artifacts>` block listing
	// every artifact already in this session (uploads + prior tool
	// results), each fetchable by ref via `artifact_fetch`. List is
	// SESSION-scoped (TaskID empty wildcard); a List error yields NO
	// manifest (logged Warn) — the turn still proceeds, never a
	// fabricated or partial manifest (CLAUDE.md §5 fail-soft on the
	// awareness aid). Newest-first ordering + cap live in the resolver /
	// renderer.
	sessionArtifacts := d.resolveSessionArtifacts(taskCtx, sessionQ)

	// Resolve the admin-set tenant default for this run's LLM parameters
	// ONCE at run start and pin the snapshot into the RunContext (the
	// effective resolution order is tenant-default › config; the
	// per-session next-turn layer slots above the tenant layer when its
	// consume seam is wired). A resolution error fails the run LOUDLY
	// rather than silently dropping the admin's policy (CLAUDE.md §13) —
	// the override store IS the runtime StateStore, so a read error here
	// means the runtime is already unhealthy.
	llmOverrides, ovErr := d.resolveLLMOverrides(taskCtx, effectiveAgentID, q)
	if ovErr != nil {
		d.logger.ErrorContext(taskCtx, "RunLoopDriver: tenant-override resolution failed; failing run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", ovErr.Error()))
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    planner.TaskErrorCodeRunLoopError,
			Message: "tenant-override resolution failed: " + ovErr.Error(),
		}); mErr != nil {
			d.logger.Warn("RunLoopDriver: MarkFailed after override-resolution error failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
		}
		return
	}

	// Overlay the agent's durable layered system prompt (operator base +
	// optional user layer) resolved from the active config at run start. The
	// projection is shared verbatim with the devstack twin (CLAUDE.md §17.6);
	// a read error fails the run loudly rather than silently dropping the
	// operator's configured prompt.
	llmOverrides, plErr := d.projectAgentConfigPromptLayers(taskCtx, effectiveAgentID, q, llmOverrides)
	if plErr != nil {
		d.logger.ErrorContext(taskCtx, "RunLoopDriver: prompt-layer projection failed; failing run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", plErr.Error()))
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    planner.TaskErrorCodeRunLoopError,
			Message: "prompt-layer projection failed: " + plErr.Error(),
		}); mErr != nil {
			d.logger.Warn("RunLoopDriver: MarkFailed after prompt-layer-projection error failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
		}
		return
	}

	// Resolve the effective run-completion hook once at run start (agent-config
	// over yaml over none) via the shared projection. A read error fails the run
	// loudly rather than silently dropping the operator's configured hook.
	completionHook, chErr := d.projectRunCompletionHook(taskCtx, effectiveAgentID, q)
	if chErr != nil {
		d.logger.ErrorContext(taskCtx, "RunLoopDriver: run-completion-hook projection failed; failing run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", chErr.Error()))
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    planner.TaskErrorCodeRunLoopError,
			Message: "run-completion-hook projection failed: " + chErr.Error(),
		}); mErr != nil {
			d.logger.Warn("RunLoopDriver: MarkFailed after run-completion-hook-projection error failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
		}
		return
	}

	// Resolve the effective session auto-naming spec once at run start
	// (agent-config over yaml over off). A read error fails the run loudly
	// rather than silently dropping the operator's configured policy.
	namingSpec, nmErr := d.projectNaming(taskCtx, effectiveAgentID, q, llmOverrides)
	if nmErr != nil {
		d.logger.ErrorContext(taskCtx, "RunLoopDriver: naming-policy projection failed; failing run",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", nmErr.Error()))
		if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
			Code:    planner.TaskErrorCodeRunLoopError,
			Message: "naming-policy projection failed: " + nmErr.Error(),
		}); mErr != nil {
			d.logger.Warn("RunLoopDriver: MarkFailed after naming-policy-projection error failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
		}
		return
	}

	spec := steering.RunSpec{
		Planner: d.planner,
		Base: planner.RunContext{
			Quadruple:        q,
			Query:            task.Query,
			Goal:             task.Query,   // initial goal = user query; runtime REDIRECT may mutate
			LLMOverrides:     llmOverrides, // admin-set tenant default, pinned at run start
			MemoryBlocks:     memBlocks,
			SkillsContext:    skillsCtx,
			RepairCounters:   counters,
			PlanningHints:    d.planningHints,  // nil when operator left the config block empty
			Catalog:          catalogView,      // populates <available_tools>
			Trajectory:       traj,             // runloop appends per step
			Emit:             emit,             // planner-side telemetry
			OnChunk:          onChunk,          // per-token streaming to bus
			InputArtifacts:   inputArtifacts,   // first-turn multimodal inputs
			SessionArtifacts: sessionArtifacts, // read-only cross-turn manifest
			// the per-run token budget the runloop's
			// compression gate reads. Zero = compression off.
			Budget: planner.Budget{TokenBudget: d.tokenBudget},
			// the per-task output schema compiled at run start; setting it
			// engages the React driver's existing per-turn steering
			// (ResponseFormat + tool-call-aware Validator + retry) with
			// zero planner change. Nil for a schemaless task.
			OutputSchema: compiledSchema,
		},
		TaskID:           taskID,
		ToolExecutor:     d.executor,   // dispatch CallTool decisions
		OnToolDispatched: dispatchHook, // item 7 — advance Task.ToolCount on dispatch
		MaxSteps:         d.maxStepsRunLoop,
		Compression:      d.compression,  // trajectory compression runner
		CompletionHook:   completionHook, // run-completion transcript egress (nil = no hook)
		Naming:           namingSpec,     // session auto-naming (nil = off)
		// the per-run trajectory-append guard, shared with the trajectories
		// map entry below so a tasks.get of THIS in-flight run can snapshot
		// the steps without racing the loop's per-step append.
		TrajectoryMu: trajMu,
	}
	// save the trajectory ref before Run so the Enricher
	// can read it post-completion AND concurrently while the run is
	// in-flight — the per-run trajMu (shared with the RunLoop's append
	// above) serialises the Enricher's snapshot against the append.
	d.trajMu.Lock()
	d.trajectories[taskID] = &trackedTrajectory{traj: traj, mu: trajMu}
	d.trajMu.Unlock()

	// Stamp the acting agent's registration id as southbound provenance for
	// this run: tool transports (the MCP driver) carry it into `_meta.agent_id`
	// for a shared server's attribution. Provenance only — never an isolation
	// principal (§6); an empty agentConfigID (bare-embed run) is a no-op.
	//
	// DELIBERATELY the BOOT value, never `effectiveAgentID`. This id also
	// becomes the RFC 8693 `actor_token` on the token-exchange path, which
	// the runtime documents as its VERIFIED acting principal and never a
	// client-supplied field; and the exchanged token is cached WITHOUT the
	// acting principal in its key, so a caller-influenced value would be
	// silently nondeterministic across a cached TTL. A caller-named agent
	// selects CONFIGURATION only — the two agent-id carriers on a run have
	// different provenance and MUST NOT be unified by a tidying refactor.
	runCtx := tools.WithInvokingAgent(d.subCtx, d.agentConfigID)
	if hasSkillSnapshot {
		runCtx = skills.WithRunSkillReaderSnapshot(runCtx, skillSnapshot)
	}
	fin, err := d.runLoop.Run(runCtx, spec)
	if err != nil {
		// Cancellation-shaped errors map to MarkFailed{code=cancelled}.
		// The FSM has no auto-cancelled status (Cancel is the external-
		// caller surface and requires a reason); Failed is the closest
		// terminal match for a ctx-cancelled run that did not reach a
		// goal.
		code := planner.TaskErrorCodeRunLoopError
		switch {
		case errors.Is(err, context.Canceled):
			code = planner.TaskErrorCodeCancelled
			d.logger.Debug("RunLoopDriver: run cancelled",
				slog.String("task_id", string(taskID)))
		case compiledSchema != nil && (errors.Is(err, llm.ErrRetryExhausted) || errors.Is(err, llm.ErrDowngradeExhausted)):
			// A schema-constrained run whose generation-steering retry loop
			// or provider-downgrade chain exhausted its budget fails LOUD
			// with the output_invalid terminal code — never a schemaless
			// success (§13). This is the RunLoop.Run failure shape of the
			// two output-invalid failure modes.
			code = planner.TaskErrorCodeOutputInvalid
			d.logger.Warn("RunLoopDriver: schema-constrained run exhausted the correction budget",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", err.Error()))
		default:
			d.logger.Warn("RunLoopDriver: RunLoop.Run failed",
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
			d.logger.Warn("RunLoopDriver: MarkFailed after Run error failed",
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
		// populate the answer envelope so Protocol
		// consumers (Console Playground, CLI, third-party UIs) read the
		// actual assistant response via tasks.get → result_inline.
		// Pre-106, this was tasks.TaskResult{} — the projector had
		// nothing to project and the Playground hardcoded a placeholder.
		// The envelope is built through the ONE shared builder
		// (runctx.FinishAnswerEnvelope) that RunOnce + the devstack twin
		// also call: for a schemaless task it is the byte-identical
		// three-key envelope; for a schema-constrained task it captures +
		// validates the terminal payload and adds the validated
		// `answer_payload`. A schema-invalid answer on this goal Finish
		// fails the task LOUD with output_invalid — never a MarkComplete
		// of an unvalidated envelope (§13, the edge-validation failure
		// shape of the two output-invalid modes). Built BEFORE the memory
		// writeback so a schema failure never persists a turn for an
		// answer that never validated.
		envelope, envErr := runctx.FinishAnswerEnvelope(fin, traj, compiledSchema)
		if envErr != nil {
			d.logger.Warn("RunLoopDriver: terminal output-schema validation failed; failing run",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", envErr.Error()))
			if mErr := d.tasks.MarkFailed(taskCtx, taskID, tasks.TaskError{
				Code:    planner.TaskErrorCodeOutputInvalid,
				Message: "terminal output failed schema validation: " + envErr.Error(),
			}); mErr != nil {
				d.logger.Warn("RunLoopDriver: MarkFailed(output_invalid) failed",
					slog.String("task_id", string(taskID)),
					slog.String("run_id", q.RunID),
					slog.String("err", mErr.Error()))
			}
			return
		}

		// Memory writeback. The 83d/83f read path
		// is wired (run loop hands MemoryBlocks to the planner); the
		// write path was the missing half. Without a writeback the
		// session-scoped memory stays empty forever and the operator's
		// multi-turn sessions cannot carry context. Best-effort: a
		// memory.AddTurn error is logged Warn but does NOT downgrade
		// the run's terminal status — the planner reached FinishGoal,
		// the operator should see Complete. AssistantResponse is the
		// envelope's Answer (the validated payload string on a schema
		// run; the extracted answer text otherwise).
		if d.memory != nil {
			turn := memory.ConversationTurn{
				UserMessage:       task.Query,
				AssistantResponse: envelope.Answer,
				Timestamp:         time.Now(),
			}
			if mErr := d.memory.AddTurn(taskCtx, sessionQ, turn); mErr != nil {
				d.logger.Warn("RunLoopDriver: memory.AddTurn failed; run still marked complete",
					slog.String("task_id", string(taskID)),
					slog.String("run_id", q.RunID),
					slog.String("err", mErr.Error()))
			}
		}

		raw, err := json.Marshal(envelope)
		if err != nil {
			d.logger.ErrorContext(taskCtx, "RunLoopDriver: marshal TaskResult.Value failed",
				slog.String("task_id", string(taskID)),
				slog.String("err", err.Error()))
			raw = []byte("{}")
		}
		if mErr := d.tasks.MarkComplete(taskCtx, taskID, tasks.TaskResult{Value: raw}); mErr != nil {
			d.logger.Warn("RunLoopDriver: MarkComplete failed",
				slog.String("task_id", string(taskID)),
				slog.String("run_id", q.RunID),
				slog.String("err", mErr.Error()))
			return
		}
		d.logger.Info("RunLoopDriver: run finished (complete)",
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
		Code:    planner.TaskErrorCodeForFinish(fin.Reason),
		Message: "RunLoop finished without satisfying goal: " + string(fin.Reason),
	}); mErr != nil {
		d.logger.Warn("RunLoopDriver: MarkFailed after non-goal Finish failed",
			slog.String("task_id", string(taskID)),
			slog.String("run_id", q.RunID),
			slog.String("err", mErr.Error()))
		return
	}
	d.logger.Info("RunLoopDriver: run finished (failed)",
		slog.String("task_id", string(taskID)),
		slog.String("run_id", q.RunID),
		slog.String("reason", string(fin.Reason)))
}

// resolveSessionArtifacts builds the session-artifact manifest the
// planner renders into the read-only `<session_artifacts>` block (
// ). It lists `ArtifactStore.List` scoped to the run's
// `(tenant, user, session)` triple — TaskID is left empty so the
// wildcard match returns every artifact in the session (uploads + tool-
// and flow-materialised results from prior turns), not just the current
// task's.
//
// Fail-soft (CLAUDE.md §5): the manifest is an awareness aid, not a
// correctness primitive. A nil store, or a List error, yields NO
// manifest (a Warn is logged) and the turn proceeds — NEVER a fabricated
// or partial manifest. The model simply is not told about artifacts that
// turn.
//
// Ordering: newest-first by the artifact's `created_at` provenance stamp
// when present, with the content-addressed ID as a stable tiebreaker so
// the map-iteration-order non-determinism of `List` does not leak into
// the prompt (a stable prefix preserves KV-cache windows). The renderer
// caps the rendered rows and appends an explicit "+K more" line on
// overflow (AC-6) — this function returns the FULL slice so the renderer
// can compute the overflow count.
func (d *RunLoopDriver) resolveSessionArtifacts(
	ctx context.Context, sessionQ identity.Quadruple,
) []planner.ArtifactManifestEntry {
	if d.artifactStore == nil {
		return nil
	}
	scope := artifacts.ArtifactScope{
		TenantID:  sessionQ.TenantID,
		UserID:    sessionQ.UserID,
		SessionID: sessionQ.SessionID,
		// TaskID intentionally empty — session-wide wildcard listing.
	}
	refs, err := d.artifactStore.List(ctx, scope)
	if err != nil {
		d.logger.Warn("RunLoopDriver: session-artifact List failed; proceeding with no manifest",
			slog.String("tenant_id", sessionQ.TenantID),
			slog.String("user_id", sessionQ.UserID),
			slog.String("session_id", sessionQ.SessionID),
			slog.String("err", err.Error()))
		return nil
	}
	// planner.BuildArtifactManifest is the SINGLE manifest builder shared
	// with harbortest/devstack (§17.6 parity) — ordering, provenance
	// resolution, and the metadata-only projection live there.
	return planner.BuildArtifactManifest(refs)
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
func (d *RunLoopDriver) Close(_ context.Context) error {
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

// TrajectoryByTaskID returns a defensive SNAPSHOT of a run's trajectory
// (its Query + a copy of the append-only Steps slice), or nil when the
// task's trajectory has been evicted or never existed. Safe for an
// out-of-band Protocol reader (the Enricher on a tasks.get) to call
// while the run is IN-FLIGHT: the snapshot is taken under the run's
// per-run trajMu — the SAME lock the steering RunLoop holds around each
// per-step append — so the returned Steps slice never observes a
// mid-append slice header (the data race the Enricher would otherwise
// hit against the run loop). Only the fields the Enricher reads (Query +
// Steps) are copied; the trajectory's shared maps are deliberately NOT
// aliased into the snapshot.
func (d *RunLoopDriver) TrajectoryByTaskID(taskID tasks.TaskID) *planner.Trajectory {
	d.trajMu.RLock()
	entry := d.trajectories[taskID]
	d.trajMu.RUnlock()
	if entry == nil || entry.traj == nil {
		return nil
	}
	entry.mu.RLock()
	defer entry.mu.RUnlock()
	return &planner.Trajectory{
		Query: entry.traj.Query,
		Steps: append([]planner.Step(nil), entry.traj.Steps...),
	}
}
