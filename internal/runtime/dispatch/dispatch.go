// Package dispatch is the runtime's production tool-dispatch concrete —
// the one full `steering.ToolExecutor` implementation (
// originally, as the dev binary's executor).
//
// Previously the runloop's default case dropped every planner CallTool
// decision on the floor (the deliberately-punted scope), which
// made multi-step ReAct structurally broken against real LLMs because
// the planner never saw tool observations. The audit pinned this as
// the root cause of the "64 steps, 0 tools called" failure mode that
// surfaced in the v1.1 operator validation. An earlier phase promoted the
// executor out of `package main` (the SDK friction audit's Pattern 1 /
// P1 finding) so every assembly — `cmd/harbor`, `harbortest/devstack`,
// a headless embedder's own run loop — wires the SAME dispatch
// semantics via [NewToolExecutor].
//
// The executor closes the planner→runtime seam against the production
// `tools.ToolCatalog`:
//
//   - CallTool: look up the descriptor by name, call Invoke under
//     the per-step ctx, return the typed ToolResult.Value (plus Meta)
//     as the observation. The planner's next step sees this on
//     `RunContext.Trajectory.Steps[N].Observation`.
//
//   - CallParallel: fan the branches out concurrently via
//     `internal/runtime/parallel` in non-atomic mode.
//
//   - SpawnTask / AwaitTask: drive the background-task registry with
//     spawn-depth caps and terminal-status polling.
//
// The executor is constructed once per stack and shared across every
// run; it holds only the catalog + artifact store + task registry
// (immutable after construction) + a logger, so the reuse
// contract is satisfied.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// ErrTaskNotOwnDescendant is returned when a task observation/cancel
// meta-tool (_task_status / _cancel_task) names a target the calling run
// did not spawn — a task whose ParentTaskID chain does not reach the
// run's own task. It is a SCOPE (authorization) violation, distinct from
// a not-found or identity-secrecy boundary: the caller's own session may
// already know the sibling task exists (it is Console-visible), so the
// message says "not your descendant" so the model can self-correct rather
// than retry a typo. This is IN ADDITION to the registry's own
// (tenant, user, session) identity-visibility check, never instead of it.
var ErrTaskNotOwnDescendant = errors.New("dispatch: task is not a descendant this run spawned")

// toolExecutor is the production `steering.ToolExecutor`
// implementation. Concurrent-safe — the catalog + artifact store are
// immutable after the stack's boot wiring runs.
//
// heavy-content discipline: tool results whose JSON encoding
// exceeds `heavyThreshold` get stored in the artifact store; the
// runloop's trajectory append uses the small llmObservation summary
// so the next LLM prompt never carries raw heavy content. Before
// this discipline the v1.1 operator validation hit `ErrContextLeak`
// after the first tool call (the youtube_get_metadata tool returns
// ~1.5 MB which would otherwise reach the LLM verbatim).
type toolExecutor struct {
	cat            tools.ToolCatalog
	artifacts      artifacts.ArtifactStore
	heavyThreshold int
	logger         *slog.Logger

	// parallel dispatches CallParallel decisions.
	// Constructed once over the same catalog (which satisfies
	// parallel.Resolver via Resolve); immutable after construction.
	// The native React path drives it in non-atomic mode so a
	// single bad-args branch becomes that branch's error result rather
	// than aborting the whole call (every provider tool_call_id must be
	// answered).
	parallel *parallel.Executor

	// tasks is the background-task registry SpawnTask / AwaitTask dispatch
	// drives. SpawnTask creates a KindBackground
	// task the per-task RunLoop driver picks up and runs; AwaitTask and a
	// retain-turn SpawnTask poll Get until the task reaches a terminal
	// status. Immutable after construction; the registry is itself
	// concurrent-safe. Nil only in degraded / legacy wiring — Spawn / Await
	// then fail loud with ErrDecisionShapeUnsupported rather than panic.
	tasks tasks.TaskRegistry

	// maxSpawnDepth caps the ParentTaskID-chain depth of planner-spawned
	// background tasks (planner.absolute_max_spawn_depth).
	// A SpawnTask whose child would exceed it is rejected loudly so a
	// background sub-agent that itself emits SpawnTask cannot recurse
	// without bound. The cap bounds depth, not breadth.
	maxSpawnDepth int

	// maxBatchSpawns caps the number of spawn branches ONE Batch decision
	// may carry (planner.max_batch_spawns) — the BREADTH ceiling that
	// maxSpawnDepth's chain-depth bound leaves open (sibling spawns in one
	// response share the parent's depth and are otherwise unlimited). A
	// Batch whose Spawns length exceeds it is rejected in FULL before any
	// tool branch dispatches or any spawn registers — never truncated.
	// Immutable after construction.
	maxBatchSpawns int
}

// defaultHeavyThreshold is the heavy-output safety floor applied when
// the operator-configured threshold is unset / non-positive. Single-
// sourced on `config.DefaultHeavyOutputThresholdBytes` (the
// `artifacts.heavy_output_threshold_bytes` default — the
// DefaultSpawnDepthCap precedent; no literal copy allowed).
const defaultHeavyThreshold = config.DefaultHeavyOutputThresholdBytes

// spawnAwaitPollInterval is the cadence at which AwaitTask + a retain-turn
// SpawnTask poll the registry for a terminal status. The registry's
// documented poll wake mode (internal/tasks/groups.go) is a cheap
// in-memory Get on the dev path; the wait is bounded by the caller's ctx.
const spawnAwaitPollInterval = 100 * time.Millisecond

// Option configures the executor [NewToolExecutor] constructs.
type Option func(*toolExecutor)

// WithHeavyThreshold sets the heavy-output threshold in bytes:
// tool results whose JSON encoding meets or exceeds it get promoted to
// artifact-backed truncation summaries before reaching the planner /
// LLM edge. Non-positive values fall back to the 32 KiB safety floor
// (the default) — the same normalization the pre-110a
// constructor applied, so passing an unset config value through is
// safe.
func WithHeavyThreshold(bytes int) Option {
	return func(e *toolExecutor) { e.heavyThreshold = bytes }
}

// WithMaxSpawnDepth caps the ParentTaskID-chain depth of
// planner-spawned background tasks (planner.absolute_max_spawn_depth).
// Non-positive values fall back to `config.DefaultSpawnDepthCap` (the
// one source of the spawn-depth default).
func WithMaxSpawnDepth(n int) Option {
	return func(e *toolExecutor) { e.maxSpawnDepth = n }
}

// WithMaxBatchSpawns caps a Batch decision's Spawns length
// (planner.max_batch_spawns). Non-positive values fall back to
// `config.DefaultMaxBatchSpawns` (the one source of the batch-spawn
// breadth default). Exceeding the cap rejects the WHOLE batch before any
// tool branch dispatches or any spawn registers — never a silent
// truncation to the first N spawns.
func WithMaxBatchSpawns(n int) Option {
	return func(e *toolExecutor) { e.maxBatchSpawns = n }
}

// WithLogger sets the executor's logger. Nil falls back to
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(e *toolExecutor) { e.logger = l }
}

// NewToolExecutor binds the catalog + artifact store + task registry
// the run loop dispatches against — the SAME instances the stack's
// boot wiring constructs. The returned executor covers every
// non-Finish Decision shape (CallTool, CallParallel, SpawnTask,
// AwaitTask, and the heterogeneous Batch) with the heavy-result→artifact promotion applied
// to every observation that reaches the planner / LLM edge.
//
// `taskReg` MAY be nil in degraded wiring — SpawnTask / AwaitTask then
// fail loud with `steering.ErrDecisionShapeUnsupported` rather than
// panic. `store` MAY be nil — heavy results then degrade to a loudly
// logged truncated preview instead of an artifact promotion.
//
// Concurrent reuse: the executor is a compiled artifact —
// construct once per stack, share across N concurrent runs. Per-run
// state lives in ctx + RunContext, never on the executor.
func NewToolExecutor(cat tools.ToolCatalog, store artifacts.ArtifactStore, taskReg tasks.TaskRegistry, opts ...Option) steering.ToolExecutor {
	e := &toolExecutor{
		cat:       cat,
		artifacts: store,
		// AC-1 (107d): the catalog already satisfies parallel.Resolver via
		// Resolve(name); reuse it as the dispatcher's resolver. No second
		// fanout engine (§13).
		parallel: parallel.New(cat),
		tasks:    taskReg,
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.logger == nil {
		e.logger = slog.Default()
	}
	if e.heavyThreshold <= 0 {
		e.heavyThreshold = defaultHeavyThreshold
	}
	if e.maxSpawnDepth <= 0 {
		// The spawn-depth default is single-sourced on
		// `config.DefaultSpawnDepthCap` (the
		// cross-region reference wired by per the
		// handoff). No other literal copy of the value is allowed.
		e.maxSpawnDepth = config.DefaultSpawnDepthCap
	}
	if e.maxBatchSpawns <= 0 {
		// The batch-spawn breadth default is single-sourced on
		// `config.DefaultMaxBatchSpawns`; no other literal copy is allowed.
		e.maxBatchSpawns = config.DefaultMaxBatchSpawns
	}
	return e
}

// ExecuteDecision implements `steering.ToolExecutor`. Dispatches the
// decision per its shape:
//
//   - CallTool: catalog.Resolve(name) → descriptor.Invoke(ctx, args)
//     under the run's identity-scoped ctx. The raw `observation` is
//     the typed ToolResult.Value; the `llmObservation` is the same
//     value unless the encoded result exceeds `heavyThreshold`, in
//     which case it gets promoted to an artifact-backed summary.
//   - CallParallel: dispatch the branches concurrently via the shared
//     parallel.Executor in non-atomic mode, then
//     assemble a per-branch aggregate observation (raw +
//     projected) so the prompt builder can round-trip N RoleTool
//     messages.
//   - SpawnTask: spawn a KindBackground task via the TaskRegistry under
//     the run's identity triple, bounded by the spawn-depth cap (
//     ). A non-retain-turn spawn returns {task_id, kind,
//     status} immediately; a retain-turn spawn blocks (ctx-bounded) until
//     the spawned task reaches a terminal status and returns its outcome.
//   - AwaitTask: poll the named task (Get) until it reaches a terminal
//     status, then return its answer-envelope / error as the observation.
//   - TaskStatusQuery: report the state of the tasks THIS run spawned
//     (descendant-scoped), one {task_id, status, description, group_id}
//     row per task.
//   - CancelTask: cancel one task THIS run spawned (descendant-scoped),
//     returning {task_id, cancelled}.
//
// Both spawn/await observations go through the same projectForLLM
// discipline so a heavy result never trips ErrContextLeak. The task
// observation/cancel observations are compact (status rows / a cancel
// bool) and carry no heavy content.
func (e *toolExecutor) ExecuteDecision(ctx context.Context, rc planner.RunContext, decision planner.Decision) (any, any, error) {
	switch d := decision.(type) {
	case planner.CallTool:
		return e.callTool(ctx, rc, d)
	case planner.CallParallel:
		return e.callParallel(ctx, rc, d)
	case planner.SpawnTask:
		return e.spawnTask(ctx, rc, d)
	case planner.AwaitTask:
		return e.awaitTask(ctx, rc, d)
	case planner.TaskStatusQuery:
		return e.taskStatus(ctx, rc, d)
	case planner.CancelTask:
		return e.cancelTask(ctx, rc, d)
	case planner.Batch:
		return e.batch(ctx, rc, d)
	default:
		return nil, nil, fmt.Errorf("%w: %T", steering.ErrDecisionShapeUnsupported, decision)
	}
}

// callTool dispatches a single CallTool. Errors:
//   - tool name does not resolve → wrapped tools.ErrToolNotFound.
//   - descriptor.Invoke returns an error → wrapped + surfaced.
//   - the result's Value is the observation the planner sees on its
//     next step (after heavy-content projection).
func (e *toolExecutor) callTool(ctx context.Context, rc planner.RunContext, d planner.CallTool) (any, any, error) {
	if d.Tool == "" {
		return nil, nil, errors.New("CallTool.Tool is empty")
	}
	desc, ok := e.cat.Resolve(d.Tool)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q", tools.ErrToolNotFound, d.Tool)
	}
	if desc.Invoke == nil {
		return nil, nil, fmt.Errorf("tool %q is registered without an Invoke function", d.Tool)
	}
	result, err := desc.Invoke(ctx, d.Args)
	if err != nil {
		e.logger.Warn("dispatch: tool invoke failed",
			slog.String("tool", d.Tool),
			slog.String("err", err.Error()))
		return nil, nil, fmt.Errorf("tool %q invoke: %w", d.Tool, err)
	}
	raw := result.Value
	if raw == nil && len(result.Meta) > 0 {
		raw = map[string]any{"meta": result.Meta}
	}
	llmObs := e.projectForLLM(ctx, rc, d.Tool, raw)
	return raw, llmObs, nil
}

// callParallel dispatches a CallParallel decision (/
// AC-1..AC-4). The branches fan out concurrently through the shared
// parallel.Executor in NON-ATOMIC mode (AC-2): a branch whose tool fails
// to resolve or whose args fail Validate surfaces as that branch's error
// result, NOT a whole-call abort — so every provider tool_call_id can be
// answered. dispatchAll returns one Result per branch in branch-index
// order; this method assembles two aggregates:
//
//   - raw observation: the untruncated per-branch tool values, what the
//     trajectory persists as Step.Observation.
//   - llmObservation: each branch's value run through the same
//     projectForLLM discipline as the single-CallTool path (AC-3), so a
//     parallel observation with several heavy branches never trips the
//     LLM-edge ErrContextLeak guard. The prompt builder decomposes this
//     into N RoleTool messages (AC-9).
//
// Whole-call aborts that survive non-atomic mode — branch-count cap
// exceeded, missing identity, empty branch set, malformed join — come
// back as the Execute error and are surfaced verbatim (fail-loud per
// §13); the runloop wraps them as the step's error observation and the
// planner re-plans.
func (e *toolExecutor) callParallel(ctx context.Context, rc planner.RunContext, d planner.CallParallel) (any, any, error) {
	results, err := e.parallel.Execute(ctx, d, parallel.WithNonAtomicSetup())
	if err != nil {
		return nil, nil, fmt.Errorf("parallel dispatch: %w", err)
	}
	rawBranches, llmBranches := e.branchObservations(ctx, rc, d.Branches, results)
	return planner.ParallelObservation{Branches: rawBranches},
		planner.ParallelObservation{Branches: llmBranches}, nil
}

// branchObservations assembles the raw + LLM-projected per-branch
// observation slices from a set of parallel.Executor results. Each
// result carries its originating branch Index; the returned slices stay
// in the executor's returned order (branch-index order under JoinAll),
// and each entry's CallID is stamped from `branches[Index].CallID` so a
// downstream renderer can pair every provider tool_call_id with its
// answer. A branch error surfaces as that entry's Error (identical raw +
// LLM entry); a success gets its Value projected through the same
// heavy-content discipline the single-CallTool path applies, so a heavy
// branch never trips the LLM-edge context-leak guard. Shared by the
// CallParallel path and the Batch decision's tool half so both preserve
// the identical non-atomic, JoinAll-only native posture.
func (e *toolExecutor) branchObservations(ctx context.Context, rc planner.RunContext, branches []planner.CallTool, results []parallel.Result) (raw, llm []planner.ParallelBranchObservation) {
	raw = make([]planner.ParallelBranchObservation, 0, len(results))
	llm = make([]planner.ParallelBranchObservation, 0, len(results))
	for _, r := range results {
		callID := ""
		if r.Index >= 0 && r.Index < len(branches) {
			callID = branches[r.Index].CallID
		}
		if r.Err != nil {
			branchErr := planner.ParallelBranchObservation{
				CallID: callID,
				Tool:   r.Tool,
				Index:  r.Index,
				Error:  r.Err.Error(),
			}
			raw = append(raw, branchErr)
			llm = append(llm, branchErr)
			continue
		}
		var rawVal any
		if r.Result != nil {
			rawVal = r.Result.Value
			if rawVal == nil && len(r.Result.Meta) > 0 {
				rawVal = map[string]any{"meta": r.Result.Meta}
			}
		}
		raw = append(raw, planner.ParallelBranchObservation{
			CallID: callID,
			Tool:   r.Tool,
			Index:  r.Index,
			Value:  rawVal,
		})
		// Per-branch projection — heavy branch values get promoted to an
		// artifact-stub summary independently.
		llm = append(llm, planner.ParallelBranchObservation{
			CallID: callID,
			Tool:   r.Tool,
			Index:  r.Index,
			Value:  e.projectForLLM(ctx, rc, r.Tool, rawVal),
		})
	}
	return raw, llm
}

// spawnTask dispatches a planner.SpawnTask.
//
// It maps the decision into a tasks.SpawnRequest under the run's
// identity triple (NEVER a global — CLAUDE.md §6) and calls Spawn. The
// spawned task's Kind defaults to KindBackground (the projector already
// defaults it); its ParentTaskID is the current run's task so the
// spawn-depth cap (AC-8) and the registry's cancel-cascade both see the
// lineage. A Spawn error (including ErrIdentityRequired) is surfaced as
// the step's error — never swallowed (§13).
//
//   - Non-retain-turn (Spec.RetainTurn == false): returns immediately
//     with {task_id, kind, status:"spawned"}. The per-task RunLoop driver
//     picks up the task.spawned event and drives the background sub-run;
//     the planner joins later by emitting AwaitTask.
//   - Retain-turn (Spec.RetainTurn == true): blocks (ctx-bounded) until
//     the spawned task reaches a terminal status, returning its outcome —
//     a synchronous spawn-and-join in one decision.
func (e *toolExecutor) spawnTask(ctx context.Context, rc planner.RunContext, d planner.SpawnTask) (any, any, error) {
	if e.tasks == nil {
		return nil, nil, fmt.Errorf("%w: SpawnTask (no TaskRegistry wired)", steering.ErrDecisionShapeUnsupported)
	}
	taskCtx, idErr := identity.With(ctx, rc.Quadruple.Identity)
	if idErr != nil {
		return nil, nil, fmt.Errorf("SpawnTask: attach identity: %w", idErr)
	}

	handle, kind, err := e.spawnOne(taskCtx, rc, d)
	if err != nil {
		return nil, nil, err
	}

	if d.Spec.RetainTurn {
		task, awaitErr := e.awaitTerminal(taskCtx, handle.ID)
		if awaitErr != nil {
			return nil, nil, fmt.Errorf("SpawnTask(retain-turn, %q): await: %w", handle.ID, awaitErr)
		}
		raw := taskOutcomeObservation(task)
		return raw, e.projectForLLM(ctx, rc, react.SpawnTaskToolName, raw), nil
	}

	raw := map[string]any{
		"task_id": string(handle.ID),
		"kind":    string(kind),
		"status":  "spawned",
	}
	return raw, raw, nil
}

// spawnOne performs the shared spawn-registration core both the plain
// SpawnTask path and the Batch decision's spawn half use: the
// recursion-guard depth check, the SpawnRequest build under the run's
// identity triple (NEVER a global — CLAUDE.md §6), and the registry
// Spawn call. `taskCtx` MUST already carry the run's identity (the
// caller attaches it once). The returned kind is the resolved task kind
// (defaulted to KindBackground) so the caller can surface it verbatim.
//
// Errors are returned as VALUES for the caller to dispose of per its
// atomicity posture: the plain SpawnTask path surfaces them as the
// step's fail-loud error; the Batch path records a registry reject
// (depth-cap exceeded, sealed group, malformed request) as THAT spawn's
// error observation while the rest of the batch proceeds (non-atomic
// per-branch — every call_id is answered).
func (e *toolExecutor) spawnOne(taskCtx context.Context, rc planner.RunContext, d planner.SpawnTask) (tasks.TaskHandle, tasks.TaskKind, error) {
	// Recursion guard: the new task's depth is the parent chain depth + 1.
	// The parent is the current run's task (RunID doubles as the TaskID at
	// the dev layer). Reject loudly above the cap — never a silent drop.
	parentID := tasks.TaskID(rc.Quadruple.RunID)
	if depth := e.spawnChainDepth(taskCtx, parentID); depth+1 > e.maxSpawnDepth {
		return tasks.TaskHandle{}, "", fmt.Errorf(
			"SpawnTask: spawn would reach depth %d, exceeding planner.absolute_max_spawn_depth=%d (parent task %q)",
			depth+1, e.maxSpawnDepth, parentID)
	}

	kind := d.Kind
	if kind == "" {
		kind = tasks.KindBackground
	}
	req := tasks.SpawnRequest{
		Identity:          identity.Quadruple{Identity: rc.Quadruple.Identity},
		Kind:              kind,
		Description:       d.Spec.Description,
		Query:             d.Spec.Query,
		Priority:          d.Spec.Priority,
		GroupID:           d.GroupID,
		PropagateOnCancel: d.Spec.PropagateOnCancel,
		NotifyOnComplete:  true,
	}
	if parentID != "" {
		req.ParentTaskID = &parentID
	}
	handle, err := e.tasks.Spawn(taskCtx, req)
	if err != nil {
		return tasks.TaskHandle{}, "", fmt.Errorf("SpawnTask: registry spawn: %w", err)
	}
	return handle, kind, nil
}

// batch dispatches a heterogeneous Batch decision — a native
// multi-call response mixing catalog-tool branches with task spawns.
// Both halves inherit the native-path posture verbatim: the tool half
// fans out through the SAME parallel.Executor call callParallel uses
// (Join passed straight through — nil collapses to JoinAll; non-atomic
// per-branch dispatch, so a branch's resolve/validate failure becomes
// that branch's error result), and the spawn half registers through the
// SAME TaskRegistry.Spawn path SpawnTask uses (RetainTurn=false), with a
// spawn's registry reject becoming that spawn's error result. Every
// branch and every spawn is always answered — provider wire contracts
// require exactly one reply per call_id.
//
// Whole-batch loud rejection (a zero-side-effect structural pre-check —
// no tool branch dispatches, no group is created, no spawn registers) is
// reserved for STRUCTURAL invariants, each wrapped in ErrInvalidDecision:
// the combined-branch-count minimum, the tool-branch parallel cap, the
// operator-configurable spawn breadth cap (planner.max_batch_spawns),
// FailFast disagreement across the spawns that would auto-group, and a
// defensive re-check that no spawn carries RetainTurn=true. The
// auto-group's resolve-or-create is the ONLY registry side effect that
// precedes the spawn loop, and it runs AFTER a successful tool-half
// dispatch so a tool-half failure never orphans a memberless group; its
// own failure degrades to a per-spawn error, not a whole-batch abort.
// Auto-grouping: when two or more spawns share no explicit GroupID, ONE
// resolve-or-create group is assigned to each; an explicit GroupID is
// never overwritten, and a single ungrouped spawn keeps the registry's
// ad-hoc single-member-group path.
//
// The observation is a BatchObservation keyed by call_id / index and
// index-aligned to the declaration order of Tools and Spawns regardless
// of dispatch completion order, so reply reconstruction never depends on
// Go map iteration order. A spawn's observation is its REGISTRATION
// outcome ({task_id, group_id} or a registry-reject error), never a
// "not done yet" — the eventual result arrives later via the group-watch
// path.
func (e *toolExecutor) batch(ctx context.Context, rc planner.RunContext, d planner.Batch) (any, any, error) {
	// === Structural pre-checks: pure inspection of the decision, ZERO
	// side effects. A structural rejection dispatches no tool branch,
	// creates no group, and registers no spawn. All are wrapped in
	// ErrInvalidDecision for uniform classification.

	// Combined-branch-count invariant (the same one NewBatch enforces at
	// construction). Re-checked here because Decision is a sealed interface
	// any concrete planner can build against, and this executor is the one
	// shared dispatch boundary for all of them — a degenerate empty/one-
	// branch Batch reaching dispatch must fail loud, never a silent no-op.
	if len(d.Tools)+len(d.Spawns) < 2 {
		return nil, nil, fmt.Errorf(
			"%w: Batch requires at least 2 combined branches, got %d tools + %d spawns",
			planner.ErrInvalidDecision, len(d.Tools), len(d.Spawns))
	}

	// Tool-count breadth vs the shared parallel cap. A structural pre-check
	// here keeps an over-cap tool set a decision-level reject rather than a
	// mid-dispatch parallel abort surfaced from Execute — the whole-batch
	// loud-reject set stays exactly the structural invariants.
	if len(d.Tools) > planner.AbsoluteMaxParallel {
		return nil, nil, fmt.Errorf(
			"%w: Batch has %d tool branches, exceeding the parallel cap of %d",
			planner.ErrInvalidDecision, len(d.Tools), planner.AbsoluteMaxParallel)
	}

	// Spawn breadth cap: reject the WHOLE batch, never truncate to the
	// first N.
	if len(d.Spawns) > e.maxBatchSpawns {
		return nil, nil, fmt.Errorf(
			"%w: Batch has %d spawns, exceeding planner.max_batch_spawns=%d (the whole batch is rejected — no branch dispatches, no spawn registers)",
			planner.ErrInvalidDecision, len(d.Spawns), e.maxBatchSpawns)
	}

	// Defensive non-retain-turn re-check.
	for i, sp := range d.Spawns {
		if sp.Spec.RetainTurn {
			return nil, nil, fmt.Errorf(
				"%w: Batch spawn %d has RetainTurn=true (a turn-retaining spawn cannot ride a non-blocking batch dispatch)",
				planner.ErrInvalidDecision, i)
		}
	}

	// FailFast agreement across the spawns that would auto-group (no
	// explicit GroupID). The one auto-created group carries a SINGLE
	// FailFast value, so a disagreement among its would-be members is
	// ambiguous — reject the whole batch loud (still a zero-side-effect
	// structural reject: this runs BEFORE the tool half dispatches or the
	// group is created) rather than silently pick one.
	var autoIdx []int
	for i, sp := range d.Spawns {
		if sp.GroupID == "" {
			autoIdx = append(autoIdx, i)
		}
	}
	autoGroup := len(autoIdx) >= 2
	if autoGroup {
		want := d.Spawns[autoIdx[0]].Spec.FailFast
		for _, i := range autoIdx[1:] {
			if d.Spawns[i].Spec.FailFast != want {
				return nil, nil, fmt.Errorf(
					"%w: Batch FailFast disagreement across auto-grouped spawns (spawn %d=%t vs spawn %d=%t); the one auto-created group cannot honour both",
					planner.ErrInvalidDecision, autoIdx[0], want, i, d.Spawns[i].Spec.FailFast)
			}
		}
	}

	// Attach identity once for every spawn-side registry call (the tool
	// half reads the run's quadruple from the caller's ctx, exactly as
	// callParallel does).
	var taskCtx context.Context
	if len(d.Spawns) > 0 {
		if e.tasks == nil {
			return nil, nil, fmt.Errorf("%w: Batch spawns (no TaskRegistry wired)", steering.ErrDecisionShapeUnsupported)
		}
		var idErr error
		taskCtx, idErr = identity.With(ctx, rc.Quadruple.Identity)
		if idErr != nil {
			return nil, nil, fmt.Errorf("batch: attach identity: %w", idErr)
		}
	}

	var raw, llm planner.BatchObservation

	// === Tools half — dispatched BEFORE any spawn-side registry side
	// effect. A tool-half whole-call error (a malformed Join, a pause
	// mid-dispatch, a missing-identity ctx — the over-cap case is
	// pre-checked above) surfaces here BEFORE the auto-group is created or
	// any spawn registers, so a tool-half failure can never orphan a
	// memberless auto-group. In non-atomic mode these setup errors abort
	// before any branch goroutine fires, so a rejected tool half runs no
	// branch either.
	if len(d.Tools) > 0 {
		results, err := e.parallel.Execute(ctx,
			planner.CallParallel{Branches: d.Tools, Join: d.Join},
			parallel.WithNonAtomicSetup())
		if err != nil {
			return nil, nil, fmt.Errorf("batch tool dispatch: %w", err)
		}
		raw.Tools, llm.Tools = e.branchObservations(ctx, rc, d.Tools, results)
	}

	// === Auto-group creation: ONE ResolveOrCreateGroup for the ungrouped
	// set, created AFTER a successful tool-half dispatch so a tool-half
	// failure above never leaves an orphaned group. Group creation failing
	// here degrades to a per-spawn error on every auto-grouped spawn (via
	// spawnOne's own reject path below) rather than a whole-batch abort —
	// whole-batch rejection stays reserved for the structural invariants.
	var autoGroupID tasks.TaskGroupID
	var autoGroupErr error
	if autoGroup {
		grp, gErr := e.tasks.ResolveOrCreateGroup(taskCtx, tasks.GroupRequest{
			SessionID:   rc.Quadruple.Identity,
			OwnerTaskID: tasks.TaskID(rc.Quadruple.RunID),
			FailFast:    d.Spawns[autoIdx[0]].Spec.FailFast,
			Description: "batch auto-group",
		})
		if gErr != nil {
			autoGroupErr = fmt.Errorf("batch: auto-group ResolveOrCreateGroup: %w", gErr)
		} else {
			autoGroupID = grp.ID
		}
	}

	// --- Spawns half (non-atomic per-branch) ---
	if len(d.Spawns) > 0 {
		spawnObs := make([]planner.BatchSpawnObservation, len(d.Spawns))
		for i, sp := range d.Spawns {
			obs := planner.BatchSpawnObservation{CallID: sp.CallID, Index: i}
			toReg := sp
			isAutoMember := autoGroup && sp.GroupID == ""
			switch {
			case isAutoMember && autoGroupErr != nil:
				// The auto-group could not be created — every auto-grouped
				// spawn's registration fails as a value (per-branch, never a
				// whole-batch abort); explicit-GroupID spawns still register.
				obs.Error = autoGroupErr.Error()
			default:
				if isAutoMember {
					toReg.GroupID = autoGroupID
				}
				handle, _, err := e.spawnOne(taskCtx, rc, toReg)
				if err != nil {
					// Per-branch error-as-value: this spawn's registry reject
					// is recorded; the rest of the batch proceeds.
					obs.Error = err.Error()
				} else {
					obs.TaskID = string(handle.ID)
					obs.GroupID = string(toReg.GroupID)
				}
			}
			spawnObs[i] = obs
		}
		// Registration outcomes are compact ({task_id, group_id} or an
		// error) — no heavy content to project — so both observations
		// carry the same spawn slice.
		raw.Spawns = spawnObs
		llm.Spawns = spawnObs
	}

	return raw, llm, nil
}

// awaitTask dispatches a planner.AwaitTask: it
// blocks (ctx-bounded) until the named task reaches a terminal status,
// then returns its answer-envelope / error as the observation. Get
// enforces identity scope, so a cross-session / cross-tenant task id
// surfaces as a not-found error observation (AC-11) rather than leaking.
func (e *toolExecutor) awaitTask(ctx context.Context, rc planner.RunContext, d planner.AwaitTask) (any, any, error) {
	if e.tasks == nil {
		return nil, nil, fmt.Errorf("%w: AwaitTask (no TaskRegistry wired)", steering.ErrDecisionShapeUnsupported)
	}
	if d.TaskID == "" {
		return nil, nil, errors.New("AwaitTask: TaskID is empty")
	}
	taskCtx, idErr := identity.With(ctx, rc.Quadruple.Identity)
	if idErr != nil {
		return nil, nil, fmt.Errorf("AwaitTask: attach identity: %w", idErr)
	}
	task, err := e.awaitTerminal(taskCtx, d.TaskID)
	if err != nil {
		return nil, nil, fmt.Errorf("AwaitTask(%q): %w", d.TaskID, err)
	}
	raw := taskOutcomeObservation(task)
	return raw, e.projectForLLM(ctx, rc, react.AwaitTaskToolName, raw), nil
}

// awaitTerminal polls the registry for `id` until it reaches a terminal
// status (Complete / Failed / Cancelled) or `ctx` is done. The ctx-done
// path returns the ctx error so a never-terminating child surfaces as a
// deadline error observation rather than a hang (the runloop wraps it;
// the planner re-plans). The poll cadence is spawnAwaitPollInterval.
func (e *toolExecutor) awaitTerminal(ctx context.Context, id tasks.TaskID) (*tasks.Task, error) {
	ticker := time.NewTicker(spawnAwaitPollInterval)
	defer ticker.Stop()
	for {
		task, err := e.tasks.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		switch task.Status {
		case tasks.StatusComplete, tasks.StatusFailed, tasks.StatusCancelled:
			return task, nil
		default:
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// spawnChainDepth returns the number of ParentTaskID hops from `id` up to
// the root: a top-level (foreground) task has depth 0, a task spawned
// from it has depth 1, and so on. The walk is bounded to maxSpawnDepth+1
// iterations so a corrupt or cyclic parent chain cannot loop unbounded; a
// Get error mid-walk stops the walk and returns the count so far
// (best-effort — the cap still bounds creation at the spawn site).
func (e *toolExecutor) spawnChainDepth(ctx context.Context, id tasks.TaskID) int {
	depth := 0
	cur := id
	for cur != "" && depth <= e.maxSpawnDepth {
		task, err := e.tasks.Get(ctx, cur)
		if err != nil || task == nil || task.ParentTaskID == nil {
			break
		}
		depth++
		cur = *task.ParentTaskID
	}
	return depth
}

// isOwnDescendant reports whether targetID is a task the calling run
// spawned, directly or transitively: it walks targetID's ParentTaskID
// chain upward and returns true iff callerID appears in that chain. The
// walk is bounded to maxSpawnDepth+1 hops (the same bound spawnChainDepth
// uses) so a corrupt or cyclic parent chain cannot loop unbounded.
// targetID == callerID returns FALSE — a run's own task is not a
// descendant, and neither task meta-tool is a self-control surface. A Get
// error mid-walk stops the walk and returns false: an unresolvable
// lineage is treated as out-of-scope (fail closed), never permitted. ctx
// MUST already carry the run's identity so the Get calls are
// identity-scoped (the registry's own visibility check is thus additive,
// not replaced — CLAUDE.md §6).
func (e *toolExecutor) isOwnDescendant(ctx context.Context, targetID, callerID tasks.TaskID) bool {
	if targetID == "" || callerID == "" || targetID == callerID {
		return false
	}
	cur := targetID
	for hops := 0; cur != "" && hops <= e.maxSpawnDepth; hops++ {
		task, err := e.tasks.Get(ctx, cur)
		if err != nil || task == nil || task.ParentTaskID == nil {
			return false
		}
		parent := *task.ParentTaskID
		if parent == callerID {
			return true
		}
		cur = parent
	}
	return false
}

// taskStatus dispatches a planner.TaskStatusQuery — the model observing
// the background tasks its own run spawned. Identity is attached exactly
// like spawnTask / awaitTask (never a global — CLAUDE.md §6).
//
// With TaskIDs == nil it walks the caller's own descendant subtree (BFS
// over TaskRegistry.List filtered by ParentID, bounded by maxSpawnDepth
// levels) — those ids are own descendants by construction, so no
// per-id scope check is needed. With TaskIDs non-empty, EVERY named id is
// descendant-scope-checked (isOwnDescendant) BEFORE any Get; one
// out-of-scope id fails the WHOLE call loud with ErrTaskNotOwnDescendant
// (atomic validation, matching CallParallel's "any branch's invalid args
// fails the whole call" precedent — never a silent partial result that
// quietly omits ids the caller can't see).
//
// Each row is {task_id, status, description, group_id}; group_id is
// resolved from the caller's identity-scoped ListGroups reverse index
// (the same derivation the tasks/protocol registry projector uses — no
// Task.GroupID field exists on the persisted record).
func (e *toolExecutor) taskStatus(ctx context.Context, rc planner.RunContext, d planner.TaskStatusQuery) (any, any, error) {
	if e.tasks == nil {
		return nil, nil, fmt.Errorf("%w: TaskStatusQuery (no TaskRegistry wired)", steering.ErrDecisionShapeUnsupported)
	}
	taskCtx, idErr := identity.With(ctx, rc.Quadruple.Identity)
	if idErr != nil {
		return nil, nil, fmt.Errorf("TaskStatusQuery: attach identity: %w", idErr)
	}
	callerID := tasks.TaskID(rc.Quadruple.RunID)

	var ids []tasks.TaskID
	if len(d.TaskIDs) == 0 {
		collected, cErr := e.collectDescendants(taskCtx, rc.Quadruple.Identity, callerID)
		if cErr != nil {
			return nil, nil, fmt.Errorf("TaskStatusQuery: collect descendants: %w", cErr)
		}
		ids = collected
	} else {
		for _, id := range d.TaskIDs {
			if !e.isOwnDescendant(taskCtx, id, callerID) {
				return nil, nil, fmt.Errorf("%w: id=%q (caller run task %q)", ErrTaskNotOwnDescendant, id, callerID)
			}
		}
		ids = d.TaskIDs
	}

	groupOf, err := e.taskGroupIndex(taskCtx, rc.Quadruple.Identity)
	if err != nil {
		return nil, nil, fmt.Errorf("TaskStatusQuery: group index: %w", err)
	}

	rows := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		task, gErr := e.tasks.Get(taskCtx, id)
		if gErr != nil {
			return nil, nil, fmt.Errorf("TaskStatusQuery: get %q: %w", id, gErr)
		}
		rows = append(rows, map[string]any{
			"task_id":     string(task.ID),
			"status":      string(task.Status),
			"description": task.Description,
			"group_id":    string(groupOf[task.ID]),
		})
	}
	raw := map[string]any{"tasks": rows}
	return raw, raw, nil
}

// cancelTask dispatches a planner.CancelTask — the model cancelling one
// task its own run spawned. It descendant-scope-checks d.TaskID against
// the calling run's task (isOwnDescendant) BEFORE touching the registry,
// then calls TaskRegistry.Cancel and returns {task_id, cancelled}. A
// target outside the run's lineage fails loud with
// ErrTaskNotOwnDescendant (never a bare not-found). Cancel's own
// (bool, error) contract is idempotent on a terminal target — cancelling
// an already-terminal descendant returns cancelled:false, not an error.
// A direct cancel is never gated on the target's own PropagateOnCancel:
// isolate detaches a task from an ANCESTOR's cascade, never from the
// spawning run's own CancelTask. Identity is attached exactly like
// spawnTask / awaitTask (never a global — CLAUDE.md §6).
func (e *toolExecutor) cancelTask(ctx context.Context, rc planner.RunContext, d planner.CancelTask) (any, any, error) {
	if e.tasks == nil {
		return nil, nil, fmt.Errorf("%w: CancelTask (no TaskRegistry wired)", steering.ErrDecisionShapeUnsupported)
	}
	if d.TaskID == "" {
		return nil, nil, errors.New("CancelTask: TaskID is empty")
	}
	taskCtx, idErr := identity.With(ctx, rc.Quadruple.Identity)
	if idErr != nil {
		return nil, nil, fmt.Errorf("CancelTask: attach identity: %w", idErr)
	}
	callerID := tasks.TaskID(rc.Quadruple.RunID)
	if !e.isOwnDescendant(taskCtx, d.TaskID, callerID) {
		return nil, nil, fmt.Errorf("%w: id=%q (caller run task %q)", ErrTaskNotOwnDescendant, d.TaskID, callerID)
	}
	cancelled, err := e.tasks.Cancel(taskCtx, d.TaskID, d.Reason)
	if err != nil {
		return nil, nil, fmt.Errorf("CancelTask(%q): %w", d.TaskID, err)
	}
	raw := map[string]any{
		"task_id":   string(d.TaskID),
		"cancelled": cancelled,
	}
	return raw, raw, nil
}

// collectDescendants returns the ids of every task in the caller's own
// descendant subtree — a BFS over TaskRegistry.List filtered by ParentID
// starting from callerID, bounded to maxSpawnDepth levels of descent (the
// same depth the spawn tree is already capped at; no new cap is
// introduced). The caller's own task is NOT included — it is not a
// descendant of itself. ctx MUST carry the run's identity so List is
// identity-scoped.
func (e *toolExecutor) collectDescendants(ctx context.Context, sessionID identity.Identity, callerID tasks.TaskID) ([]tasks.TaskID, error) {
	var out []tasks.TaskID
	frontier := []tasks.TaskID{callerID}
	seen := map[tasks.TaskID]struct{}{callerID: {}}
	for level := 0; level < e.maxSpawnDepth && len(frontier) > 0; level++ {
		var next []tasks.TaskID
		for _, parent := range frontier {
			pid := parent
			children, err := e.tasks.List(ctx, sessionID, tasks.TaskFilter{ParentID: &pid})
			if err != nil {
				return nil, err
			}
			for _, c := range children {
				if _, dup := seen[c.ID]; dup {
					continue
				}
				seen[c.ID] = struct{}{}
				out = append(out, c.ID)
				next = append(next, c.ID)
			}
		}
		frontier = next
	}
	return out, nil
}

// taskGroupIndex builds the task→group reverse index the status rows use
// for their group_id column, mirroring the tasks/protocol registry
// projector's "Group enrichment" derivation: ListGroups is
// identity-scoped; a registry without group support returns an empty
// slice, so an absent membership is the honest "" default, never a silent
// degradation of a known value. ctx MUST carry the run's identity.
func (e *toolExecutor) taskGroupIndex(ctx context.Context, sessionID identity.Identity) (map[tasks.TaskID]tasks.TaskGroupID, error) {
	groups, err := e.tasks.ListGroups(ctx, sessionID, nil)
	if err != nil {
		return nil, err
	}
	groupOf := make(map[tasks.TaskID]tasks.TaskGroupID, len(groups))
	for _, g := range groups {
		for _, member := range g.Members {
			groupOf[member] = g.ID
		}
	}
	return groupOf, nil
}

// taskOutcomeObservation projects a terminal task record into the
// planner-readable observation for AwaitTask / retain-turn SpawnTask.
// The answer envelope on Result.Value is JSON — the per-task driver's
// [planner.AnswerEnvelope] `{answer, finish_reason, tool_calls_seen}`
// shape (exported for run-loop drivers). The additive `answer_payload`
// key rides this projection whenever the awaited task carried an output
// schema: a `start` request with `output_schema` set produces a
// validated `answer_payload` on the task envelope, and this generic
// parse surfaces it to the awaiting parent with no shape change. It is
// embedded parsed-generic (not as the typed struct) so the planner sees
// structured data rather than a JSON-string AND so non-envelope
// Result.Values (a TaskResult is not guaranteed to be an envelope)
// round-trip without field loss. A failed / cancelled task carries its
// error code + message instead (a schema-invalid answer fails the task
// with the `output_invalid` code, which the parent observes here). A
// large `answer_payload` rides the SAME heavy-output offload the rest of
// this projection path already applies (see `projectForLLM` below) — no
// second content-size mechanism.
func taskOutcomeObservation(task *tasks.Task) any {
	out := map[string]any{
		"task_id": string(task.ID),
		"status":  string(task.Status),
	}
	if task.Result != nil && len(task.Result.Value) > 0 {
		var v any
		if err := json.Unmarshal(task.Result.Value, &v); err == nil {
			out["result"] = v
		} else {
			out["result"] = string(task.Result.Value)
		}
	}
	if task.Error != nil {
		out["error"] = map[string]any{
			"code":    task.Error.Code,
			"message": task.Error.Message,
		}
	}
	return out
}

// projectForLLM applies heavy-content discipline to the tool
// result before it reaches the planner's next-step renderer. When the
// JSON encoding is small enough (< heavyThreshold), the projection is
// the raw value (the planner sees the full result). When it exceeds
// the threshold, the projection is a summary map referencing an
// ArtifactRef stored in the artifact store — the planner sees a small
// representation + a stable id it can mention in its Finish answer.
//
// If artifact storage is unavailable OR fails, the projection
// degrades to a truncated string preview. We log a Warn (silent
// degradation is forbidden) but do not fail the run — losing a
// tool's full result is recoverable in the planner's eyes.
func (e *toolExecutor) projectForLLM(ctx context.Context, rc planner.RunContext, tool string, raw any) any {
	if raw == nil {
		return raw
	}
	encoded, encErr := json.Marshal(raw)
	if encErr != nil {
		// Marshaling failure: hand the planner a summary string so
		// SOMETHING lands — silent context loss is §13-forbidden.
		return map[string]any{
			"tool":  tool,
			"error": fmt.Sprintf("observation could not be JSON-encoded: %v", encErr),
		}
	}
	size := len(encoded)
	if size < e.heavyThreshold {
		return raw
	}
	// Heavy result — promote to artifact store.
	if e.artifacts == nil {
		// No artifact store wired (degraded stack) — truncate
		// loudly so the operator can see exactly what was elided.
		e.logger.Warn("dispatch: heavy tool result without artifact store; truncating",
			slog.String("tool", tool),
			slog.Int("size_bytes", size))
		return HeavyTruncationSummary(tool, raw, encoded, size, "")
	}
	scope := artifacts.ArtifactScope{
		TenantID:  rc.Quadruple.TenantID,
		UserID:    rc.Quadruple.UserID,
		SessionID: rc.Quadruple.SessionID,
	}
	// W6: stamp `created_at` on the Source map so the
	// Protocol wire layer's `extractCreatedAt` populates the row with
	// a real timestamp. Without this the Console renders the Go
	// zero-value `0001-01-01T00:00:00Z` for every heavy-promoted
	// artifact. The wire layer accepts a time.Time directly.
	ref, putErr := e.artifacts.PutText(ctx, scope, string(encoded), artifacts.PutOpts{
		Filename: fmt.Sprintf("tool-result-%s.json", tool),
		MimeType: "application/json",
		Source: map[string]any{
			// stamp the canonical `source`
			// discriminator so artifacts.list and the session-artifact
			// manifest project a real provenance ("tool") instead of a
			// blank source. The `tool`/`producer` keys stay for the
			// originating-tool name. The `producer` value is preserved
			// verbatim from the pre-110a executor (byte-for-byte
			// provenance parity — pre-existing consumers may key on it).
			"source":     "tool",
			"producer":   "dev-tool-executor",
			"tool":       tool,
			"run_id":     rc.Quadruple.RunID,
			"created_at": time.Now().UTC(),
		},
	})
	if putErr != nil {
		e.logger.Warn("dispatch: artifact PutText failed; truncating",
			slog.String("tool", tool),
			slog.Int("size_bytes", size),
			slog.String("err", putErr.Error()))
		return HeavyTruncationSummary(tool, raw, encoded, size, "")
	}
	return HeavyTruncationSummary(tool, raw, encoded, size, ref.ID)
}

// previewFieldMaxBytes caps each top-level field's serialized form in
// the field-aware preview. Fields whose serialized value exceeds this
// budget are replaced with a `[omitted: N bytes]` sentinel so they
// still appear as keys (the model sees what's available) but don't
// blow the preview budget. 1 KiB is chosen as the threshold because
// it captures normal scalar fields (numbers, short strings, small
// arrays of tags/categories) but prunes nested heavy objects like
// yt-dlp's `automatic_captions`, `formats`, `subtitles` (each
// hundreds of KB).
const previewFieldMaxBytes = 1024

// previewTotalMaxBytes is the hard cap on the entire field-aware
// preview's serialized size. Even with per-field pruning a result
// with hundreds of small scalar fields could still grow large; this
// is the back-stop.
const previewTotalMaxBytes = 16384

// HeavyTruncationSummary builds the small llmObservation the
// planner renders for a heavy tool result. For JSON-object results
// it emits a field-aware preview that preserves every top-level
// scalar / small field verbatim and replaces oversized nested
// values with a `[omitted: N bytes]` sentinel — so the model sees
// both what's available and what was pruned. For non-object
// results (arrays, scalars at top level) it falls back to
// byte-truncation at `previewTotalMaxBytes`.
//
// Carries: the tool name, byte size of the full result, the
// preview, the `truncated: true` signal, and the artifact ID
// when available.
//
// This exported identifier IS the heavy-truncation shape contract:
// the React planner's prompt renderer
// (`internal/planner/react/prompt.go::renderHeavyContentMap`)
// pattern-matches the `{tool, size_bytes, truncated, preview,
// artifact_ref}` map this function emits. Changing a key here is a
// prompt-renderer change in the same PR (re-pointed the
// renderer's citation from the pre-promotion `cmd/harbor` copy to
// this function).
func HeavyTruncationSummary(tool string, raw any, encoded []byte, size int, artifactID string) any {
	prev := buildPreview(raw, encoded)
	out := map[string]any{
		"tool":       tool,
		"size_bytes": size,
		"truncated":  true,
		"preview":    prev,
	}
	if artifactID != "" {
		out["artifact_ref"] = artifactID
	}
	return out
}

// buildPreview renders the field-aware preview when the result is a
// JSON object (after unwrapping common single-key wrappers like
// `{"result": {...}}` that MCP tools and Go structs produce), or
// falls back to byte-truncation for non-object shapes (arrays,
// scalars at top). Returns a string so the existing `preview` key
// shape on the observation map is preserved.
//
// Why the unmarshal step: `raw` here may be a typed Go struct (the
// MCP driver returns its own value type), not a `map[string]any`.
// A type assertion against `map[string]any` would miss those. The
// encoded bytes are guaranteed JSON, so we re-derive the generic
// shape from them.
//
// Why the unwrap step: MCP tools (and many in-process tools)
// wrap their payload in `{"result": <value>}` so the top-level has
// exactly one key whose value is the real data. Applying
// field-aware preview to the outer wrapper would prune `result`
// itself as oversized — defeating the whole point. The unwrap is
// bounded to one level and only fires when the outer has exactly
// one key AND that key's value is itself a map.
func buildPreview(raw any, encoded []byte) string {
	var m map[string]any
	if asMap, ok := raw.(map[string]any); ok {
		m = asMap
	} else if err := json.Unmarshal(encoded, &m); err != nil || m == nil {
		// Top-level is an array or scalar (not a JSON object) —
		// fall back to byte truncation.
		prev := string(encoded)
		if len(prev) > previewTotalMaxBytes {
			prev = prev[:previewTotalMaxBytes] + "...(truncated)"
		}
		return prev
	}

	// Unwrap a single-key wrapper one level when its value is itself
	// a map. Captures the `{"result": {<actual metadata>}}` shape MCP
	// tools emit.
	if len(m) == 1 {
		for _, v := range m {
			if inner, ok := v.(map[string]any); ok && len(inner) > 1 {
				m = inner
			}
		}
	}

	if s, ok := fieldAwarePreview(m); ok {
		return s
	}
	// Field-aware build failed — fall back to byte truncation.
	prev := string(encoded)
	if len(prev) > previewTotalMaxBytes {
		prev = prev[:previewTotalMaxBytes] + "...(truncated)"
	}
	return prev
}

// fieldAwarePreview emits a JSON object where each top-level key from
// `m` is included verbatim when its serialized form is under
// `previewFieldMaxBytes`, OR replaced with a `[omitted: N bytes]`
// sentinel string otherwise. Returns (preview, true) on success; the
// false return signals the caller to fall back to byte-truncation
// (e.g. the marshal of the assembled preview failed somehow).
func fieldAwarePreview(m map[string]any) (string, bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]any, len(m))
	for _, k := range keys {
		v := m[k]
		vBytes, err := json.Marshal(v)
		if err != nil {
			out[k] = fmt.Sprintf("[unrenderable: %v]", err)
			continue
		}
		if len(vBytes) > previewFieldMaxBytes {
			out[k] = fmt.Sprintf("[omitted: %d bytes]", len(vBytes))
			continue
		}
		// Embed the parsed value (not the raw bytes) so the assembled
		// preview is one JSON document, not a JSON-string of JSON.
		out[k] = v
	}

	prevBytes, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	if len(prevBytes) > previewTotalMaxBytes {
		// Even after per-field pruning the assembled doc is too big
		// — most often happens when there are hundreds of small
		// scalar fields. Truncate the assembled doc but signal it.
		return string(prevBytes[:previewTotalMaxBytes]) + "...(truncated)", true
	}
	return string(prevBytes), true
}

// ensure interface satisfaction at compile time.
var _ steering.ToolExecutor = (*toolExecutor)(nil)
