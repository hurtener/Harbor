# Phase 186 — Batch executor: heterogeneous dispatch, auto-grouping, ordered observations

## Summary

Dispatches `planner.Batch` (the fourth sealed `Decision` shape phase 185
ships): tool branches fan out through the SAME `parallel.Executor` the
existing `CallParallel` dispatch already uses; spawn branches register
through the existing `TaskRegistry.Spawn` path, auto-grouped into one
`ResolveOrCreateGroup` group when ≥2 share no explicit `GroupID`. Both halves
inherit the D-169 native-path posture verbatim — `Join` always resolves to
`JoinAll` and dispatch is non-atomic per branch — with whole-batch loud
rejection reserved for structural setup invariants only: the new operator
breadth cap (`planner.max_batch_spawns`), `FailFast` disagreement across
auto-grouped spawns, and a defensive re-check of phase 185's
non-retain-turn invariant. The executor produces a call-id/index-keyed
`BatchObservation` so reply reconstruction never depends on Go map iteration
order, and this phase closes a previously-unwired production seam
(`steering.WithHardCancelHook`) so a run-level hard cancel actually
cascades into a batch's spawned descendants.

## RFC anchor

- RFC §6.2
- RFC §6.8

## Briefs informing this phase

- brief 16
- brief 05
- brief 02

## Brief findings incorporated

- brief 16 §4: "Executor: tool branches dispatch exactly as `CallParallel`;
  spawn branches register via the existing `ResolveOrCreateGroup` + `Spawn`
  path (auto-group, no new registry method); 'spawn completion' = task
  registered (not finished — that is `WatchGroup`'s job); observation keyed
  by `call_id` with `RoleTool` replies reconstructed in the ORIGINAL
  `resp.ToolCalls` order (provider protocols require one result per
  `call_id`; Go map iteration must never determine reply order). `FailFast`
  disagreement across auto-grouped spawns fails the batch loud." — adopted
  as this phase's dispatch table, auto-grouping rule, and the
  `BatchObservation` ordering invariant.
- brief 16 §6: "spawn depth is capped at dispatch (`toolExecutor.spawnTask`,
  `dispatch.go:352-357`) by `planner.absolute_max_spawn_depth`... walking
  `ParentTaskID` hops. The cap bounds DEPTH only — sibling spawns in one
  Batch share the parent's depth and are NOT mutually limited
  (`dispatch.go:95`), which is exactly why the Batch decision needs its own
  explicit breadth cap (`MaxBatchSpawns`) with whole-batch loud rejection."
  — adopted as the new `planner.max_batch_spawns` config knob, mirroring
  the `absolute_max_spawn_depth` precedent (`config.go:1694-1728`) field
  for field.
- brief 16 §2e: "For Harbor's fail-loud posture, a hard
  operator-configurable cap with whole-batch loud rejection (mirroring
  `CallParallel`'s atomic setup validation, `decision.go:60-62`) beats both
  per-excess truncation and uncapped-and-hope." — adopted; the cap rejects
  the WHOLE batch (zero tool branches dispatch, zero spawns register), never
  truncates to the first N spawns.
- brief 16 §5: "The invariant to make explicit: `isolate` detaches a task
  from its parent's cascade, never from direct operator control; a
  session-scoped operator cancel sweeps isolate-marked tasks too. There is
  no uncancellable task." — adopted verbatim as this phase's cancellation
  hierarchy statement (Goals / Cancellation section below); `isolate` itself
  stays unreachable from the model until phase 187 (D-324).
- brief 05 (subsystem overview, "Cancellation propagation honors
  `propagate_on_cancel` (`\"cascade\"` | `\"isolate\"`). Group sealing
  freezes membership; `retain_turn` blocks the foreground until the group
  completes.") — the existing `TaskGroup`/`Cancel` machinery this phase
  reuses without inventing a second cancellation mechanism.
- brief 02 (`CallParallel{Branches, Join}` — branches execute concurrently
  via ... atomic setup, best-effort execution) — the precedent this phase's
  Tools half inherits unchanged, and the shape the Spawns half's per-branch
  error-as-value posture mirrors.

## Findings I'm departing from (if any)

None. This phase implements — and does not depart from — the settled D-169
native-path posture, extended to `Batch`:

- **D-169 item 2 (`JoinAll`-only on the native path).** `Batch.Join`
  follows the identical rule already settled for `CallParallel`: the native
  projector (phase 185) always emits `Join: nil` (→ `JoinAll`);
  `JoinFirstSuccess` / `JoinN` would cancel losing branches and orphan their
  `tool_call_id`s, malforming the next provider request. This phase's
  executor passes `d.Join` straight through to the same
  `parallel.Executor.Execute` call `callParallel` already uses, inheriting
  the existing validation/normalization — no new Join-handling logic.
- **D-169 item 3 (non-atomic per-branch dispatch on the native path).** A
  tool branch's resolve-miss or args-validation failure becomes that
  branch's error result while the remaining branches fan out — unchanged
  from today's `CallParallel` dispatch. This phase applies the identical
  posture to the Spawns half: a spawn's registry reject (depth-cap exceeded,
  group sealed, malformed request) becomes THAT spawn's error result in
  `BatchObservation.Spawns`, never a whole-batch abort. Whole-batch loud
  rejection is reserved for the structural setup class D-323 names: the
  `max_batch_spawns` breadth cap, `FailFast` disagreement across
  auto-grouped spawns, and a defensive re-check that no `Spawns[i]` carries
  `Spec.RetainTurn == true` (phase 185's `NewBatch` constructor already
  rejects this at construction; the executor re-checks it because `Decision`
  is a sealed interface any future planner concrete can construct against
  — CLAUDE.md §4.4 — and the executor is the one shared dispatch boundary
  for all of them, not just the react projector).

A related, additive (non-breaking) amendment: phase 185's shipped `Batch`
shape reuses `SpawnTask{Kind, Spec, GroupID}` unchanged, which carries no
provider `call_id`. A heterogeneous batch with ≥2 spawns needs each spawn's
original native `tool_call_id` preserved (exactly as `CallTool.CallID`
already is) so `BatchObservation.Spawns` can be keyed the same way
`BatchObservation.Tools` is. This phase adds `SpawnTask.CallID string`
(empty for the pre-existing standalone/prompt-engineered emission paths,
mirroring `CallTool.CallID`'s doc contract) and phase 185's projector
partition step populates it for `Batch.Spawns` entries. This is scoped as a
small, zero-value-compatible field addition on a type `internal/planner`
already owns across many phases — not a reopening of phase 185's shipped
design.

## Goals

- Dispatch `planner.Batch` through `steering.ToolExecutor.ExecuteDecision`:
  tool branches via the same `parallel.Executor` call `callParallel` uses;
  spawn branches via the existing `TaskRegistry.Spawn` path.
- Auto-group ≥2 no-explicit-`GroupID` spawns in one `Batch` into a single
  `ResolveOrCreateGroup` group; never touch a spawn's explicit `GroupID`.
- Add the operator-configurable `planner.max_batch_spawns` breadth cap
  (whole-batch loud rejection on exceed — never silent truncation),
  mirroring the `absolute_max_spawn_depth` config precedent.
- Produce `planner.BatchObservation` (call-id/index-keyed, mirroring
  `ParallelObservation`) so reply reconstruction is never at the mercy of
  Go map iteration order, and never blocks on "not done yet" — a spawn's
  observation is its registration outcome (`{task_id, group_id}` or an
  error), not its eventual terminal result.
- State and test the FULL cancellation hierarchy for batch-spawned
  descendants: human (any task, always, via direct `Cancel`) > agent (own
  descendants only — phase 187) > cascade defaults (`PropagateOnCancel`
  BFS, the always-on default until phase 187 makes `isolate`
  model-expressible).
- Close the currently-unwired `steering.WithHardCancelHook` production seam
  in the ONE stack assembler (`internal/runtime/assemble/assemble.go`) so a
  run-level hard cancel actually reaches — and cascades through — a batch's
  spawned descendants, not just the in-flight tool branches.
- Confirm (via test, not new code) that `internal/runtime/steering/runloop.go`'s
  generic `default:` dispatch case needs NO change: it already routes any
  non-`Finish`/`RequestPause` decision through `ExecuteDecision` and counts
  invocations via `planner.DecisionInvocationCount` (phase 185 already
  ships the `Batch` case returning `len(Tools)`).

## Non-goals

- **Task-management meta-tools** (`_task_status`, `_cancel_task`) and
  making `propagate_on_cancel: isolate` model-expressible — phase 187
  (D-324). Until then every batch spawn cascades on parent cancel; there is
  no model-reachable way to mark one `isolate`.
- **Await-in-batch.** `_await_task` stays standalone per phase 185's AC-21′;
  unchanged here.
- **The conversational wake-message / notification-class event** on group
  resolution — phase 188 (D-325). `WatchGroup`'s typed `GroupCompletion`
  path is untouched.
- **Prompt-builder reconstruction** (`internal/planner/react/prompt.go`'s
  `Batch` counterpart to `renderNativeParallelStep`). This phase ships the
  call-id-keyed `BatchObservation` CONTRACT the reconstruction depends on
  and proves it via an executor-level ordering test; wiring the actual
  `RoleTool` message assembly into the react prompt builder is a follow-up
  this phase's `BatchObservation` shape makes mechanical (the same
  index-into-a-keyed-map pattern `parallelBranchBodiesByIndex` already
  uses), tracked as a risk below rather than silently deferred.
- **True cross-half concurrency guarantees beyond "neither half starts
  before structural setup passes."** The Tools half and Spawns half both
  dispatch once setup validation clears; this phase does not add new
  synchronization primitives beyond what `parallel.Executor` and
  `TaskRegistry.Spawn` already provide.
- **Durable / cross-restart background-task survival** — pre-existing
  scope boundary (phase 87), unaffected by this phase.

## Acceptance criteria

- [ ] `steering.ToolExecutor.ExecuteDecision` gains a `case planner.Batch`
      dispatching to a new `batch` method; `runloop.go`'s generic
      `default:` case is unchanged (verified by a passing test, not by
      inspection alone).
- [ ] Structural setup validation runs BEFORE any dispatch and rejects the
      WHOLE batch (zero tool branches dispatch, zero spawns register) on:
      `len(d.Spawns) > planner.max_batch_spawns`; `FailFast` disagreement
      among the spawns that would auto-group (empty `GroupID`); any
      `Spawns[i].Spec.RetainTurn == true` (defensive re-check of phase
      185's constructor invariant).
- [ ] Tool branches dispatch via `e.parallel.Execute(ctx,
      planner.CallParallel{Branches: d.Tools, Join: d.Join},
      parallel.WithNonAtomicSetup())` — byte-identical dispatch contract to
      `callParallel`, including per-branch resolve/validate failures
      surfacing as that branch's error result (D-169 item 3 parity).
- [ ] Spawn branches register via `TaskRegistry.Spawn`, one call per spawn,
      each carrying `ParentTaskID` = the run's task (existing depth-cap
      wiring unchanged) and `GroupID` = its explicit `GroupID` when set, or
      the ONE auto-created group's ID when ≥2 spawns share no explicit
      `GroupID` (a single ungrouped spawn keeps today's ad-hoc
      single-member-group behavior — auto-grouping activates only at ≥2).
      A spawn's own registry reject (depth cap, sealed group, etc.) becomes
      that spawn's error entry in `BatchObservation.Spawns` — never a
      whole-batch abort.
- [ ] `planner.BatchObservation{Tools []ParallelBranchObservation, Spawns
      []BatchSpawnObservation}` is index-aligned to `d.Tools` / `d.Spawns`
      declaration order REGARDLESS of actual dispatch completion order —
      proven by a test that artificially reorders per-branch/per-spawn
      completion latency and asserts output order is unchanged.
- [ ] `OnToolDispatched` fires with exactly `len(d.Tools)` on a successful
      Batch dispatch (spawns never increment tool-invocation accounting) —
      reusing phase 185's `DecisionInvocationCount(Batch)` case; no
      duplicate counting logic added in dispatch.
- [ ] `planner.max_batch_spawns` is a new `PlannerConfig` field (`yaml:
      "max_batch_spawns,omitempty"`) with a `BatchSpawnCap()` resolver
      (non-positive → `config.DefaultMaxBatchSpawns`) and a
      `validate.go` rule rejecting a negative value — exact structural
      mirror of `absolute_max_spawn_depth` / `SpawnDepthCap()`.
      `examples/dev.yaml` documents the new key next to
      `absolute_max_spawn_depth`.
- [ ] `internal/runtime/assemble/assemble.go` wires
      `dispatch.WithMaxBatchSpawns(cfg.Planner.BatchSpawnCap())` into the
      ONE `dispatch.NewToolExecutor` call, and
      `steering.WithHardCancelHook(...)` into the ONE `steering.NewRunLoop`
      call — closing the previously-dangling hook (confirmed unwired in
      production prior to this phase: `WithHardCancelHook` had zero
      non-test call sites). The hook cancels the run's own task
      (`TaskRegistry.Cancel(ctx, tasks.TaskID(runID), reason)`), whose
      existing cascade (BFS over `ParentTaskID`, honoring each descendant's
      `PropagateOnCancel`) reaches every batch-spawned child because each
      carries `ParentTaskID` = the run's task — no new cascade mechanism is
      built.
- [ ] Cancellation hierarchy is tested end-to-end with real drivers: (a) a
      hard run-level CANCEL mid-batch aborts an in-flight tool branch's
      ctx AND transitions the batch's spawned descendants to `Cancelled`
      (cascade default); (b) a direct `Cancel` on one spawned descendant
      succeeds regardless of ITS `PropagateOnCancel` value (isolate is not
      model-reachable yet, but the direct-target path is exercised as the
      "no uncancellable task" invariant); (c) the hierarchy statement itself
      (human > agent > cascade defaults) is documented at the call site
      (godoc) and asserted by the test names, not just prose.
- [ ] `scripts/smoke/phase-186.sh` passes as a unit-tests-class smoke
      (`PREFLIGHT_REQUIRES: unit-tests`), replacing today's skeleton
      `skip`.
- [ ] Coverage on `internal/runtime/dispatch` and the touched
      `internal/runtime/steering` / `internal/runtime/assemble` paths meets
      the stated targets.

## Files added or changed

- `internal/runtime/dispatch/dispatch.go` — `case planner.Batch`, the new
  `batch` method, `maxBatchSpawns` field, `WithMaxBatchSpawns` option, the
  auto-grouping helper, the structural setup validators, and the
  `BatchObservation` assembly.
- `internal/runtime/dispatch/dispatch_batch_test.go` — dispatch-table test,
  auto-grouping test (single `ResolveOrCreateGroup` call for ≥2 ungrouped
  spawns; explicit `GroupID` never overwritten), breadth-cap rejection
  test, `FailFast`-disagreement rejection test, `RetainTurn=true` defensive
  rejection test, per-branch/per-spawn error-as-value tests (a bad tool arg
  or a depth-cap-exceeded spawn does NOT abort the batch), tool-count
  accounting test, ordering-invariant test under randomized completion
  latency, `TestExecutor_Batch_ConcurrentReuse` (N≥100, `-race`).
- `internal/planner/decision.go` — additive `SpawnTask.CallID string`
  field (empty for pre-existing standalone paths).
- `internal/planner/batch_observation.go` — new file: `BatchObservation`,
  `BatchSpawnObservation` types (JSON-encodable, mirroring
  `parallel_observation.go`'s doc pattern and package placement rationale).
- `internal/planner/batch_observation_test.go` — JSON round-trip +
  index-alignment invariant tests.
- `internal/config/config.go` — `PlannerConfig.MaxBatchSpawns`,
  `BatchSpawnCap()`, `config.DefaultMaxBatchSpawns = 5`.
- `internal/config/validate.go` — negative-value rejection for
  `planner.max_batch_spawns`.
- `internal/config/validate_core_test.go` / `validate_test.go` — resolver
  default/explicit tests + negative-value validation test, mirroring the
  existing `absolute_max_spawn_depth` test pairs.
- `examples/dev.yaml` — documented `max_batch_spawns` example line.
- `internal/runtime/assemble/assemble.go` — wires
  `dispatch.WithMaxBatchSpawns` and `steering.WithHardCancelHook` into the
  ONE production stack assembly.
- `internal/runtime/assemble/assemble_test.go` (or nearest existing
  assembler test file) — asserts both options are threaded from config
  into the constructed executor/run loop.
- `test/integration/batch_executor_test.go` — real-driver end-to-end test
  (see Test plan).
- `scripts/smoke/phase-186.sh` — real assertions replacing the skeleton
  `skip`.

## Public API surface

```go
// internal/runtime/dispatch/dispatch.go

// WithMaxBatchSpawns caps a planner.Batch's Spawns length
// (planner.max_batch_spawns). Non-positive values fall back to
// config.DefaultMaxBatchSpawns. Exceeding the cap rejects the WHOLE batch
// before any tool branch dispatches or any spawn registers — never a
// silent truncation to the first N spawns.
func WithMaxBatchSpawns(n int) Option

// batch dispatches a planner.Batch (D-323): the Tools half fans out
// through the same parallel.Executor call callParallel uses (Join always
// nil -> JoinAll on the native path, D-169 item 2; non-atomic per-branch
// dispatch, D-169 item 3); the Spawns half registers through
// TaskRegistry.Spawn, auto-grouped via ONE ResolveOrCreateGroup call when
// >=2 spawns share no explicit GroupID. Whole-batch loud rejection is
// reserved for structural setup: the breadth cap, FailFast disagreement
// across auto-grouped spawns, and a defensive non-retain-turn re-check.
// Per-branch/per-spawn failures are error values, never batch-killing
// aborts (every call_id is always answered).
func (e *toolExecutor) batch(ctx context.Context, rc planner.RunContext, d planner.Batch) (any, any, error)
```

```go
// internal/planner/batch_observation.go

// BatchObservation is the aggregate observation a ToolExecutor produces
// for a dispatched Batch. Tools mirrors ParallelObservation.Branches
// (index-aligned to Batch.Tools); Spawns is index-aligned to Batch.Spawns.
// Both preserve DECLARATION order regardless of dispatch completion
// order — the caller MUST index/look up by CallID or Index, never range
// a map, to reconstruct per-call_id replies (provider wire contracts
// require exactly one reply per call_id).
type BatchObservation struct {
    Tools  []ParallelBranchObservation `json:"tools,omitempty"`
    Spawns []BatchSpawnObservation     `json:"spawns,omitempty"`
}

// BatchSpawnObservation is one Batch.Spawns entry's registration outcome.
// "Complete" here means REGISTERED, not terminal — a spawn's eventual
// result arrives later via WatchGroup, never through this observation.
// Exactly one of (TaskID+GroupID) or Error is populated.
type BatchSpawnObservation struct {
    CallID  string `json:"call_id,omitempty"`
    Index   int    `json:"index"`
    TaskID  string `json:"task_id,omitempty"`
    GroupID string `json:"group_id,omitempty"`
    Error   string `json:"error,omitempty"`
}
```

```go
// internal/config/config.go

// BatchSpawnCap resolves the optional max_batch_spawns knob. A
// non-positive value (unset or zero) resolves to
// DefaultMaxBatchSpawns: a Batch whose Spawns length exceeds this is
// rejected loudly in full — never truncated to the first N.
func (p PlannerConfig) BatchSpawnCap() int

// DefaultMaxBatchSpawns is the ONE source of the batch-spawn breadth
// default (conservative; operator-revisable via planner.max_batch_spawns).
const DefaultMaxBatchSpawns = 5
```

## Test plan

- **Unit:**
  - Dispatch table: `ExecuteDecision` routes `planner.Batch` to `batch`
    (mirrors the existing `TestExecutor_*` naming for `CallTool` /
    `CallParallel` / `SpawnTask`).
  - Auto-grouping: exactly ONE `ResolveOrCreateGroup` call for ≥2
    no-`GroupID` spawns in one batch (spy `TaskRegistry`); an explicit
    `GroupID` on any spawn is passed through unchanged and never routed
    into the auto-created group; a single no-`GroupID` spawn in an
    otherwise-tools batch does NOT trigger `ResolveOrCreateGroup` (keeps
    today's ad-hoc single-member-group path).
  - Breadth cap: `len(Spawns) > max_batch_spawns` → zero `Invoke` calls,
    zero `Spawn` calls, one whole-batch error naming the cap and the
    actual count.
  - `FailFast` disagreement: two no-`GroupID` spawns with
    `Spec.FailFast` = `true` and `false` → zero dispatch, error names both
    conflicting values.
  - Defensive `RetainTurn=true` re-check → zero dispatch, wrapped
    `planner.ErrInvalidDecision`.
  - Per-branch/per-spawn error-as-value: a Batch with one resolve-miss tool
    branch and one depth-cap-exceeded spawn alongside otherwise-valid
    branches/spawns dispatches everything else successfully; only the
    two failing entries carry `Error` in `BatchObservation`.
  - Tool-count accounting: `OnToolDispatched` invoked once with
    `len(Tools)`; a Batch with only spawns (zero tools) does NOT invoke
    the hook at all (matches `SpawnTask`'s existing zero-count/no-call
    behavior).
  - Ordering invariant (`TestExecutor_Batch_PreservesDeclarationOrder`):
    a fake catalog tool and a spy `TaskRegistry` are both instrumented to
    complete in REVERSE declaration order (last branch/spawn finishes
    first); asserts `BatchObservation.Tools[i].CallID == d.Tools[i].CallID`
    and `BatchObservation.Spawns[i].CallID == d.Spawns[i].CallID` for all
    `i`, regardless of completion timing — the concrete regression guard
    for "Go map iteration must never determine reply order."
  - `TestExecutor_Batch_ConcurrentReuse` — N≥100 concurrent
    `ExecuteDecision(Batch)` calls against ONE shared `toolExecutor` under
    `-race`, asserting no data races, no context bleed across runs (each
    run's identity/session stays isolated), no cancellation cross-talk
    (cancelling run A's ctx never aborts run B's batch), and
    `runtime.NumGoroutine()` returns to baseline after all calls join —
    mirroring `TestExecutor_CallParallel_ConcurrentReuse` /
    `TestExecutor_SpawnAwait_ConcurrentReuse`'s existing shape (D-025).
  - `internal/config`: `BatchSpawnCap()` default/explicit resolution;
    `Validate` rejects a negative `max_batch_spawns`; mirrors the existing
    `absolute_max_spawn_depth` test pair.
  - `internal/planner/batch_observation_test.go`: JSON round-trip; a
    `BatchObservation` containing a non-JSON-encodable `Value` in either
    half surfaces the same `ErrUnserializable` contract `Trajectory.Serialize`
    already enforces (no silent drop).
- **Integration:** `test/integration/batch_executor_test.go` — real
  drivers end-to-end: a real inmem `TaskRegistry` (backed by a real inmem
  `StateStore` + `EventBus`, mirroring
  `dispatch_spawn_await_test.go::mkSpawnAwaitTestTaskRegistry`), a real
  in-proc tool catalog, and the production `dispatch.NewToolExecutor`
  wired exactly as `assemble.go` wires it (including the new
  `WithMaxBatchSpawns` / hard-cancel-hook wiring — this is the seam this
  phase closes, so the test exercises the ASSEMBLED stack, not a
  hand-built executor). Covers:
  - A Batch mixing 2 tool branches + 3 spawns dispatches all 5 branches;
    identity (tenant/user/session) propagates correctly onto every spawned
    task's record and every tool invocation's provenance.
  - Failure mode 1: breadth cap exceeded (`max_batch_spawns` set to 2,
    batch carries 3 spawns) → whole-batch rejection, zero side effects
    (asserted via `TaskRegistry.List` returning no new tasks).
  - Failure mode 2: `FailFast` disagreement across auto-grouped spawns →
    whole-batch rejection, zero side effects.
  - Cancellation hierarchy: a hard CANCEL fired mid-batch (via the
    steering inbox, `payload.hard=true`) cascades through the
    auto-created group's members (all transition to `Cancelled`) while a
    sibling run in a DIFFERENT session is unaffected (cross-session
    isolation, CLAUDE.md §6); a direct operator `Cancel` on one spawned
    descendant succeeds independent of the run's own state (the
    "no uncancellable task" invariant).
  - N≥10 concurrent sessions each dispatching a Batch concurrently — no
    cross-session leakage of spawned tasks, groups, or observations
    (CLAUDE.md §11 cross-session isolation requirement).
- **Conformance:** N/A — no new persistence driver ships in this phase;
  `BatchObservation` is a value type, not a driver-backed interface.
- **Concurrency / leak:** covered by `TestExecutor_Batch_ConcurrentReuse`
  (unit, N≥100 single-instance reuse) and the integration suite's N≥10
  concurrent-session stress (cross-package boundary, per AGENTS.md §17.3).

## Smoke script additions

`scripts/smoke/phase-186.sh` flips from the skeleton `skip` to a
`PREFLIGHT_REQUIRES: unit-tests` script asserting:

- `go build ./internal/runtime/dispatch/... ./internal/planner/... ./internal/config/... ./internal/runtime/assemble/...` succeeds.
- `go test ./internal/runtime/dispatch/... -run 'Batch' -race` passes
  (dispatch table, auto-group, breadth-cap rejection, `FailFast`
  disagreement, ordering invariant, concurrent-reuse).
- `go test ./test/integration/... -run 'BatchExecutor' -race` passes.
- Grep assertion: `internal/runtime/dispatch/dispatch.go` declares
  `case planner.Batch:` inside `ExecuteDecision`.
- Grep assertion: `internal/runtime/assemble/assemble.go` calls both
  `dispatch.WithMaxBatchSpawns(` and `steering.WithHardCancelHook(`
  (regression guard against the hook silently going unwired again).
- `skip` (not `fail`) if `internal/planner/decision.go` doesn't yet define
  `Batch` (the phase-185 not-yet-built case) — standard 404-equivalent.

## Coverage target

- `internal/runtime/dispatch`: 85%
- touched `internal/runtime/steering` paths (hard-cancel hook plumbing):
  85%
- `internal/runtime/assemble` touched lines: 80%
- `internal/planner` (new `batch_observation.go` + `SpawnTask.CallID`):
  85%
- `internal/config` touched lines: 85%

## Dependencies

- 185 (`planner.Batch`, AC-21′, the projector partition, `Batch`'s
  `DecisionInvocationCount` case — D-322)
- 47 (`CallParallel` executor + `SpawnTask`/`AwaitTask` emission, D-056)
- 107d (native `CallParallel` dispatch + non-atomic setup + `JoinAll`-only,
  D-169 — the posture this phase extends to `Batch`)
- 107e (`SpawnTask`/`AwaitTask` dev-executor dispatch, spawn-depth cap,
  D-170)

## Risks / open questions

- **Prompt-builder reconstruction is a follow-up, not shipped here.**
  `BatchObservation` gives the react prompt builder everything it needs
  (call-id-keyed, declaration-order-stable) to emit N `RoleTool` messages
  for a `Batch` step, mirroring `renderNativeParallelStep`'s existing
  index-keyed-map pattern — but this phase does not touch
  `internal/planner/react/prompt.go`. Until that follow-up lands, a
  `Batch` step's trajectory replay is INCOMPLETE for prompt reconstruction
  purposes even though dispatch itself is fully correct; flag this
  explicitly in the PR rather than let it read as silently finished. Not
  scoping it into 186 is deliberate — the source-grounding for this phase
  is dispatch/steering/tasks/config, and prompt-builder work belongs with
  the phase that owns the projection round-trip (185's family).
- **`SpawnTask.CallID` is an additive field phase 185 didn't anticipate.**
  If 185 lands first without it, this phase's PR adds it as a small,
  backward-compatible amendment (see "Findings I'm departing from"). If a
  later reviewer of 185 already added an equivalent field under a
  different name, reconcile naming in this PR rather than shipping two
  provider-id fields on `SpawnTask`.
- **Cross-half dispatch ordering (Tools vs. Spawns) is unspecified beyond
  "both wait on structural setup."** This phase does not mandate that
  Tools and Spawns fire in literally the same instant; a future
  performance pass could tighten this if batches with many spawns show
  measurable tool-branch latency inflation. Not a correctness risk today
  (per-branch/per-spawn results are independent regardless of relative
  start order), but worth a follow-up perf test if operator batches grow
  large under the (small, revisable) `max_batch_spawns` default.
- **The hard-cancel hook's cascade reach is broader than "this batch."**
  Because it cancels the run's OWN task, cascade reaches every descendant
  the run has EVER spawned (across all of its batches and single spawns
  in the run, not just the most recent one) — this matches existing
  `PropagateOnCancel` semantics exactly (it is not new scope creep
  introduced by this phase) but is worth stating explicitly since "cascade
  the batch's auto-created group" (D-323's phrasing) is a subset of what
  actually happens; the plan's acceptance criteria test the broader,
  correct behavior.

## Glossary additions

- **`max_batch_spawns` (breadth cap)** — the operator-configurable ceiling
  on a single `Batch` decision's `Spawns` length; whole-batch loud
  rejection when exceeded, never per-excess truncation. Distinguished from
  `absolute_max_spawn_depth`, which bounds spawn-chain DEPTH, not the
  BREADTH of one response's spawns. Phase 186, D-323.
- **`BatchObservation`** — the call-id/index-keyed aggregate observation a
  `ToolExecutor` produces for a dispatched `Batch`
  (`Tools []ParallelBranchObservation` + `Spawns []BatchSpawnObservation`),
  index-aligned to the originating `Batch.Tools`/`Batch.Spawns` declaration
  order regardless of concurrent dispatch completion order. Phase 186,
  D-323.
- **Auto-grouped batch spawn** — a `Batch.Spawns` entry with no explicit
  `GroupID` that joins the ONE group the executor resolves-or-creates for
  every such entry in the same batch (vs. an explicit-`GroupID` spawn,
  which the executor never redirects). Requires ≥2 qualifying entries to
  activate; a single ungrouped spawn keeps the pre-existing ad-hoc
  single-member-group path. Phase 186, D-323.
- **Cascade-by-default batch cancellation** — the invariant that a
  run-level hard cancel aborts in-flight `Batch` tool branches and
  cascades through every batch-spawned descendant
  (`PropagateOnCancel=cascade`, the only reachable value until phase 187
  ships `isolate` as model-expressible, D-324); a human operator can
  always cancel any task directly regardless of propagation mode — the
  hierarchy is human (any task, always) > agent (own descendants, phase
  187) > cascade defaults. Phase 186, D-323.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — YES: the integration suite's N≥10 concurrent-session stress and the hard-cancel cross-session-unaffected assertion both cover this.
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** YES: `TestExecutor_Batch_ConcurrentReuse` extends the existing `toolExecutor` D-025 coverage to the new `Batch` case.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** YES: `test/integration/batch_executor_test.go` — real inmem `TaskRegistry`/`StateStore`/`EventBus`/catalog, the assembled production executor, identity propagation, two failure modes (breadth cap, `FailFast` disagreement), `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A: this phase implements D-323 (already logged) and departs from no brief/RFC finding; see "Findings I'm departing from."
