# Phase 187 — Task-management planner meta-tools + the cancel hierarchy

## Summary

Ships `_task_status` and `_cancel_task`, two new reserved planner-control
meta-tools giving the model observation and control over the background
tasks its OWN run spawned — descendant-scoped via the `ParentTaskID` chain,
never arbitrary session tasks. Pairs that new power with its brake in the
same phase: `propagate_on_cancel: isolate` becomes model-expressible on
`_spawn_task` for the first time. Closes a gap the shipped cascade-cancel
walk left dormant because `isolate` was previously unreachable: an
ancestor's cascade must skip an isolate-marked descendant's whole subtree,
never just the direct-cancel target.

## RFC anchor

- RFC §6.2
- RFC §6.4
- RFC §6.8

## Briefs informing this phase

- brief 16
- brief 02
- brief 05

## Brief findings incorporated

- brief 16 §5: "as a power-with-brake pairing (the §13 primitive-with-consumer
  rule read as governance): `propagate_on_cancel: isolate` becomes
  model-expressible ONLY in the same wave as `_task_status` ... and
  `_cancel_task` ... — descendant-scoped, never arbitrary session tasks."
- brief 16 §5: "The invariant to make explicit: `isolate` detaches a task
  from its parent's cascade, never from direct operator control; a
  session-scoped operator cancel sweeps isolate-marked tasks too. There is
  no uncancellable task." (Verbatim in RFC §6.2 post-185/186 amendment —
  this phase is the one that makes the invariant load-bearing, since
  `isolate` was structurally unreachable before it.)
- brief 16 §5: "These meta-tools earn their place independently: a model
  that fanned out four explorations and got its answer from the first
  should cancel the other three (`JoinFirstSuccess` economics under the
  model's own judgment)." — the design driver for `_cancel_task` being a
  direct per-task call, not a group-level operation.
- brief 16 §3: "Does not transfer: ... uncapped parallelism on
  prompt-hope alone" — informs keeping `_task_status` / `_cancel_task`
  OUT of the Batch-eligible set (a conservative, structurally-enforced
  non-batchable grammar for the first wave that exposes them; see Scope).
- brief 02 §5 (sharp edge 4, "Magic strings as opcodes"): "Harbor's
  `Decision` is a sum type; tool calls and runtime opcodes are different
  shapes. Future runtime-level actions ... extend the sum, not the catalog
  of magic strings." — directly informs shipping `TaskStatus` / `CancelTask`
  as two new sealed `Decision` shapes rather than overloading `CallTool`
  args or a string-typed control channel.
- brief 05 §5 (sharp edge, task interface breadth): "Harbor keeps the
  surface but groups it into named method sets" — informs dispatching the
  new meta-tools through the EXISTING `TaskRegistry.Get` / `List` / `Cancel`
  method set (no new `TaskRegistry` methods), matching how `_spawn_task` /
  `_await_task` reuse `Spawn` / `Get` rather than growing the interface.

## Findings I'm departing from (if any)

None. The one design question this phase resolves that the briefs left
implicit — whether an ancestor's cascade walk must check a swept
descendant's OWN `PropagateOnCancel` (see Goals) — is not a departure from
brief 16 §5; it is the mechanical consequence of brief 16 §5's own
invariant ("isolate detaches a task from its parent's cascade"), which the
shipped `internal/tasks/engine` code does not yet implement because
`isolate` had no producer before this phase (D-047 froze `SpawnSpec`
without it). `SpawnSpec`'s own godoc licenses the amendment explicitly:
"Future phases MAY extend this shape with additional planner-controlled
fields" (`internal/planner/decision.go`) — this phase is that future phase,
filed as D-324 per the wave's pre-assignment
(`docs/plans/wave-v116-parallel-intent-coordination.md`).

## Goals

- Ship `_task_status` and `_cancel_task` as reserved, natively-declared
  planner-control meta-tools, dispatched as two new sealed `planner.Decision`
  shapes (`TaskStatus{TaskIDs []tasks.TaskID}`, `CancelTask{TaskID
  tasks.TaskID, Reason string}` — RFC §6.2) through the same
  projector-translation → executor-dispatch seam `_spawn_task` / `_await_task`
  already use. Both are non-terminal: the runtime executor dispatches them
  like `CallTool` and appends a trajectory step the planner observes on its
  next turn.
- Enforce descendant-only scoping for BOTH meta-tools at the dispatch layer:
  a target `task_id` must be reachable by walking `ParentTaskID` hops
  upward from the target until it reaches the calling run's own task id
  (`rc.Quadruple.RunID`), bounded exactly like the existing
  `spawnChainDepth` walk. A target outside that lineage — including a
  sibling run's own spawned tasks in the SAME session — is rejected loud
  with a new `dispatch.ErrTaskNotOwnDescendant` sentinel, never silently
  narrowed or silently permitted. This check is IN ADDITION TO the
  registry's existing `(tenant, user, session)` identity-visibility check
  (`internal/tasks/engine/engine.go`'s `identityVisible`) — never instead
  of it (CLAUDE.md §6).
- Make `propagate_on_cancel` ∈ {`""` (→ cascade), `"isolate"`}
  model-expressible on `_spawn_task`'s `spec` object for the first time,
  amending `planner.SpawnSpec` (D-047) with a `PropagateOnCancel string`
  field the dispatch executor carries straight onto `tasks.SpawnRequest`.
  This lands in the SAME phase as the two meta-tools per brief 16 §5's
  power-with-brake gate — never before.
- Close the cascade-walk gap that makes `isolate` inert against an
  ancestor's cascade. Today, `internal/tasks/engine/engine.go`'s `Cancel`
  (:596-628) and `internal/tasks/engine/groups.go`'s `cancelTaskLocked`
  (:778-803) BFS through a cancelled task's descendants and transition
  EVERY reachable descendant "regardless of their own PropagateOnCancel"
  (the code's own comment, `engine.go:620-625`) — only the CANCEL TARGET's
  own flag is consulted (to decide whether to start the walk at all). That
  contradicts RFC §6.2's now-settled invariant: "`isolate` detaches a task
  from its parent's cascade, never from operator control." This phase
  extracts one shared descendant-walk helper both call sites use, which
  additionally checks EACH descendant's own `PropagateOnCancel` as the walk
  reaches it: an `isolate`-marked descendant is skipped (not cancelled) and
  its whole subtree is detached from the walk (not enqueued) — while the
  DIRECT-target semantics (calling `Cancel(id)` on a task always transitions
  that task, isolate or not) are unchanged. This is the only way the field
  the model can now set delivers what its own tool description promises.
- Prove the full cancel hierarchy end-to-end: operator direct/session-scoped
  cancel always reaches any task including isolate-marked ones; the agent
  reaches only its own descendants via `_cancel_task`; cascade is the
  default that sweeps everything not explicitly isolated.
- Teach the new tools and the isolate semantics honestly in the reserved
  planner-control prompt surface, and update the one operator skill that
  documents this surface for a Playground-observing operator.

## Non-goals

- Steering or pausing a spawned child from the parent's model turn. The
  per-run steering inbox (`internal/runtime/steering/inbox.go`) already
  exists per background sub-run; exposing it as a planner-facing verb is a
  named future extension, not this phase.
- Batchable `_task_status` / `_cancel_task`. Structurally excluded twice
  over: `planner.Batch`'s shape (`Tools []CallTool; Spawns []SpawnTask;
  Join *JoinSpec` — phase 185/186) has no slot for either type, and the
  projector's standalone-name guard (post-185's narrowed set) gains both
  names in this phase. Widening to batchable later is additive (a new
  `Batch` field + projector case); retracting a shipped batchable surface
  is not — the conservative choice is binding for this wave.
- A new session-scoped bulk-cancel Protocol method. "Session-scoped
  operator cancel sweeps isolate too" is already satisfied structurally: an
  operator/Console/TUI bulk-cancel (today, N individual `cancel` calls
  against a set of task ids — see `docs/decisions.md` D-122's bulk-Cancel
  note) is N direct `TaskRegistry.Cancel(id)` calls, and direct-target
  Cancel semantics were never gated on the TARGET's own `PropagateOnCancel`
  in the first place (only whether the target's cascade continues past it).
  No new production surface is required; this phase adds a regression test
  pinning the behavior, not new code.
- The notification-class conversational wake mirror for background task
  outcomes (brief 16 §5's "wake-with-a-message"). That is phase 188 (D-325)
  — orthogonal to this phase per the wave's staging note.
- `Batch`'s spawn+tools co-occurrence and the `max_batch_spawns` breadth
  cap. Shipped by 185/186; this phase only rides the already-amended
  `SpawnSpec` shape those consume.

## Acceptance criteria

- [ ] **AC-1** `internal/planner/decision.go`: two new sealed `Decision`
      shapes — `TaskStatus{TaskIDs []tasks.TaskID}` and `CancelTask{TaskID
      tasks.TaskID; Reason string}` — matching RFC §6.2 verbatim, each with
      an `isDecision()` marker. `SpawnSpec` gains `PropagateOnCancel string`
      (empty = runtime default `tasks.PropagateCascade`; `tasks.PropagateIsolate`
      is the only other accepted value). Godoc on both new shapes and the
      amended field cites D-324 and the RFC §6.2 cancel-hierarchy sentence,
      naming the FEATURE not an internal phase/wave number (CLAUDE.md §13).
- [ ] **AC-2** `internal/planner/react/react.go`: `TaskStatusToolName =
      "_task_status"` and `CancelTaskToolName = "_cancel_task"` reserved-name
      constants, godoc mirroring `SpawnTaskToolName` / `AwaitTaskToolName`
      (:132-149).
- [ ] **AC-3** `internal/planner/react/discovered_tools.go`:
      `reservedPlannerControlDeclarations()` returns four entries (was two).
      `jsonSchemaRawTaskStatus` — `{task_ids?: string[]}`, empty/omitted
      means "every task_id this run has spawned, including nested
      descendants." `jsonSchemaRawCancelTask` — `{task_id: string
      (required), reason?: string}`. Both pin
      `additionalProperties: false` at every object level (OpenAI
      strict-mode parity, matching the existing two schemas' documented
      rationale). `jsonSchemaRawSpawnTask`'s `spec` object gains
      `propagate_on_cancel: {type: string, enum: ["cascade", "isolate"]}`
      (optional; omitted = cascade). Tool descriptions state plainly that
      `isolate` "detaches this task from YOUR cancellation (including a
      cascade from a task you spawned it under) — it never detaches it
      from the operator's" and that `_task_status` / `_cancel_task` only
      see tasks this run itself spawned, directly or transitively.
- [ ] **AC-4** `internal/planner/react/projector.go`: `projectResponse`'s
      head switch gains `case TaskStatusToolName: return
      translateNativeTaskStatus(first)` and `case CancelTaskToolName: return
      translateNativeCancelTask(first)`. `translateNativeTaskStatus` parses
      `{"task_ids": [...]}` (missing/`null`/`[]` → `TaskIDs: nil`, meaning
      "list everything"); malformed JSON fails loud with wrapped
      `planner.ErrInvalidDecision`. `translateNativeCancelTask` parses
      `{"task_id": "...", "reason": "..."}`; empty `task_id` fails loud with
      `planner.ErrInvalidDecision` (mirrors `translateNativeAwait`'s empty-id
      guard verbatim).
- [ ] **AC-5** `internal/planner/react/projector.go`: `translateNativeSpawn`
      parses the new `spec.propagate_on_cancel` key, validates it against
      `{"", "cascade", "isolate"}` (mirroring the existing `kind` enum
      check immediately above it), and threads it onto
      `planner.SpawnSpec.PropagateOnCancel`. An out-of-enum value fails
      loud with `planner.ErrInvalidDecision` naming the offending value —
      never silently clamped to cascade.
- [ ] **AC-6** `internal/planner/react/projector.go`: the standalone-name
      guard (post-185's narrowed set, whatever its exact identifier —
      `_finish` and `_await_task` per RFC §6.2's post-185 sentence) gains
      `_task_status` and `_cancel_task`. Any response where either
      co-occurs with ANY other tool-call — a catalog tool, `_spawn_task`,
      each other, or two of the same — is rejected loud with
      `planner.ErrInvalidDecision` naming the offending control tool and
      stating it is standalone. `_spawn_task` remains batchable per 185/186
      and is UNAFFECTED by this change. Single-call `_task_status` /
      `_cancel_task` translate normally.
- [ ] **AC-7** `internal/runtime/dispatch/dispatch.go`: `ExecuteDecision`'s
      switch gains `case planner.TaskStatus: return e.taskStatus(ctx, rc,
      d)` and `case planner.CancelTask: return e.cancelTask(ctx, rc, d)`.
      Both new executor methods attach the run's identity via
      `identity.With` exactly like `spawnTask` / `awaitTask` (never a
      global context, CLAUDE.md §6).
- [ ] **AC-8** `internal/runtime/dispatch/dispatch.go`: a new
      `isOwnDescendant(ctx, targetID, callerID tasks.TaskID) bool` helper
      walks `targetID`'s `ParentTaskID` chain upward (mirroring
      `spawnChainDepth`'s bound of `e.maxSpawnDepth+1` hops) and reports
      whether `callerID` appears in that chain. `targetID == callerID`
      (a run targeting its own task) returns `false` — self is not a
      descendant, and neither meta-tool is a self-control surface. Both
      `taskStatus` and `cancelTask` call this BEFORE touching the registry
      for any explicitly-named target; a negative result produces a new
      package sentinel `dispatch.ErrTaskNotOwnDescendant` wrapped with the
      offending id, never a bare not-found (the caller needs to know this
      is a scope violation, not a typo, so it can self-correct — this is
      an authorization-scoping message, not an identity-secrecy boundary,
      since the caller's own session already has Console-visible knowledge
      the sibling task exists).
- [ ] **AC-9** `internal/runtime/dispatch/dispatch.go`: `taskStatus` with
      `TaskIDs == nil` walks the caller's own descendant subtree (BFS over
      `TaskRegistry.List` filtered by `ParentID`, bounded by
      `maxSpawnDepth` levels) and returns one row per descendant:
      `{task_id, status, description, group_id}` (brief 16 §5's named
      shape). `group_id` is resolved via `TaskRegistry.ListGroups` +
      building a member→group reverse index — the SAME derivation pattern
      `internal/tasks/protocol/aggregating_projector.go`'s documented
      "Group enrichment" comment uses for `ListTasks`, not a new engine
      accessor (no `Task.GroupID` field exists on the persisted record).
      With `TaskIDs` non-empty, EVERY named id is descendant-scope-checked
      (AC-8) before any `Get`; one out-of-scope id fails the WHOLE call
      loud (atomic validation, matching `CallParallel`'s "any branch's
      invalid args fails the whole call" precedent) — never a silent
      partial result that quietly omits ids the caller can't see.
- [ ] **AC-10** `internal/runtime/dispatch/dispatch.go`: `cancelTask`
      descendant-scope-checks `d.TaskID` (AC-8), then calls
      `TaskRegistry.Cancel(taskCtx, d.TaskID, d.Reason)` and returns
      `{task_id, cancelled: bool}` (mirrors `Cancel`'s own `(bool, error)`
      idempotent-on-terminal contract — cancelling an already-terminal
      descendant returns `cancelled: false`, not an error).
- [ ] **AC-11** `internal/tasks/engine/engine.go` + `groups.go`: the
      duplicated descendant-cascade BFS walk (`Cancel` :596-628 and
      `cancelTaskLocked` :778-803) is extracted into one shared
      `cascadeCancelDescendantsLocked(ctx, rootID tasks.TaskID, reason
      string) error` both call sites use. The shared walk now checks EACH
      descendant's own `PropagateOnCancel` as it is dequeued: `cascade`
      (or empty) → cancel + enqueue its children as before; `isolate` →
      skip cancelling it AND do not enqueue its children (the whole
      subtree detaches from the walk). Direct-target Cancel/cancelTaskLocked
      semantics (the FIRST node — the call's own target — transitions
      unconditionally, gated only by whether IT starts a cascade) are
      byte-for-byte unchanged; only the treatment of nodes reached
      mid-walk changes.
- [ ] **AC-12** `internal/planner/react/prompt.go`: `renderNativeControlStep`
      gains `case planner.TaskStatus` / `case planner.CancelTask`, each with
      a matching `taskStatusReplayArgs` / `cancelTaskReplayArgs` builder
      (mirroring `spawnTaskReplayArgs` / `awaitTaskReplayArgs` exactly), so
      a trajectory containing either meta-tool step replays as a native
      `tool_call` + `RoleTool` pair on the next prompt build, consistent
      with how `_spawn_task` / `_await_task` steps already replay.
- [ ] **AC-13** Cross-session / cross-run isolation test (mandatory per
      CLAUDE.md §6.10 and §11): two sibling runs in the SAME
      `(tenant, user, session)` — run A spawns a background task; run B
      (a concurrent, independent run in the same session) calls
      `_task_status` and `_cancel_task` naming run A's spawned task id.
      Both fail loud with `ErrTaskNotOwnDescendant`; run A's task is
      untouched (still running, never cancelled by run B). A third case
      confirms the registry's EXISTING cross-session identity check still
      independently rejects an out-of-session id (AC-8 is additive to it,
      never a replacement).
- [ ] **AC-14** Cancel-hierarchy end-to-end test asserting the RFC §6.2
      invariant in one scenario: a run spawns child C1 with
      `propagate_on_cancel: isolate` and child C2 with the cascade default,
      both under a shared parent P. (a) Cancelling P directly (simulating
      an ancestor-triggered cascade) cancels C2 (cascade swept) but leaves
      C1 RUNNING (isolate detached — AC-11). (b) A direct operator
      `TaskRegistry.Cancel(C1.ID, ...)` (simulating the Protocol's `cancel`
      method reaching the isolate task directly) DOES cancel C1 — isolate
      never blocks a direct target. (c) The run's own `_cancel_task` on its
      isolate descendant C1 also succeeds (agent-initiated direct cancel,
      descendant-scoped per AC-8, is not a cascade and is unaffected by
      C1's own isolate flag).
- [ ] **AC-15** `internal/tasks/conformancetest/conformancetest.go`: a new
      shared scenario (`Cancel_Cascade_SkipsIsolateDescendant` or similar)
      exercising AC-11 against BOTH the `inprocess` and `durable` drivers —
      a cascade-cancelled parent with a mixed cascade-child / isolate-child
      / isolate-grandchild tree; asserts the isolate subtree is fully
      untouched (parent AND its own children survive) while cascade
      siblings transition to cancelled.
- [ ] **AC-16** D-025 concurrent-reuse test: N≥100 concurrent
      `_task_status` / `_cancel_task` dispatches against the SAME shared
      `toolExecutor` instance (mirrors the existing
      `TestExecutor_SpawnAwait_ConcurrentReuse` pattern in
      `dispatch_spawn_await_test.go`), under `-race`, asserting no data
      races, no context bleed across runs, no goroutine leak.
- [ ] **AC-17** `docs/skills/drive-the-playground/SKILL.md` §3 ("Foreground
      vs background tasks") is updated in the same PR (CLAUDE.md §18): the
      agent can now check on and cancel its own spawned background tasks
      mid-run via `_task_status` / `_cancel_task`, and a spawned task can be
      marked to survive the spawning agent's own cancellation
      (`propagate_on_cancel: isolate`) while remaining reachable by a
      direct operator cancel at any time. `docs/research/INDEX.md` /
      `docs/skills/` grep confirms no other `surface: llm|agent-yaml|protocol`
      skill documents `_spawn_task`/`_await_task` today, so no second skill
      needs the update.
- [ ] **AC-18** `docs/decisions.md` gains the pre-assigned D-324 entry
      pinning: the two new sealed `Decision` shapes and why they are NOT a
      widening of `Batch` (structural — `Batch` has no slot for them); the
      `SpawnSpec` amendment as a sanctioned extension of D-047, not a
      re-litigation; the descendant-scoping mechanism (ParentTaskID walk,
      additive to identity scoping); and the cascade-walk fix (AC-11) with
      its file:line citation of the pre-existing "regardless of their own
      PropagateOnCancel" comment it supersedes.
- [ ] **AC-19** `RFC-001-Harbor.md` §6.2's `Batch` paragraph (already
      amended by 185/186 to read "Only `_finish` and `_await_task` remain
      standalone") is extended in this phase's PR to also name
      `_task_status` and `_cancel_task` as standalone, keeping the RFC and
      the shipped projector guard (AC-6) in lockstep.

## Files added or changed

- `internal/planner/decision.go` — `TaskStatus`, `CancelTask` shapes;
  `SpawnSpec.PropagateOnCancel`.
- `internal/planner/react/react.go` — `TaskStatusToolName`,
  `CancelTaskToolName` constants.
- `internal/planner/react/discovered_tools.go` — two new reserved
  declarations + JSON schemas; `_spawn_task` schema gains
  `propagate_on_cancel`.
- `internal/planner/react/projector.go` — translation functions, standalone
  guard extension, `translateNativeSpawn` amendment.
- `internal/planner/react/prompt.go` — `renderNativeControlStep` +
  replay-arg builders.
- `internal/planner/react/discovered_tools_test.go` — created by phase 185
  (description-text assertions for the rewritten `_spawn_task` description);
  this phase extends it with AC-3's declaration-presence + schema-shape
  assertions for the two new reserved tools and the amended `_spawn_task`
  schema.
- `internal/planner/react/projector_test.go` — AC-4/5/6 unit tests.
- `internal/planner/react/prompt_test.go` — AC-12 replay tests.
- `internal/runtime/dispatch/dispatch.go` — `ExecuteDecision` cases,
  `taskStatus`, `cancelTask`, `isOwnDescendant`, group-id enrichment
  helper, `ErrTaskNotOwnDescendant`, `spawnTask` amendment (carries
  `PropagateOnCancel` onto `SpawnRequest`).
- `internal/runtime/dispatch/dispatch_taskmgmt_test.go` — new; AC-7/8/9/10/16.
- `internal/tasks/engine/engine.go` — shared cascade-walk helper (AC-11).
- `internal/tasks/engine/groups.go` — `cancelTaskLocked` uses the shared
  helper.
- `internal/tasks/engine/engine_test.go` — AC-14 cancel-hierarchy scenario.
- `internal/tasks/conformancetest/conformancetest.go` — AC-15.
- `test/integration/` — AC-13 cross-run isolation, if not adequately
  covered in-package (see Test plan).
- `docs/skills/drive-the-playground/SKILL.md` — AC-17.
- `docs/decisions.md` — D-324 (AC-18).
- `docs/glossary.md` — new terms (see Glossary additions).
- `RFC-001-Harbor.md` — AC-19.
- `scripts/smoke/phase-187.sh`.

## Public API surface

```go
// internal/planner/decision.go
type TaskStatus struct {
    TaskIDs []tasks.TaskID // nil/empty = every task this run has spawned
}
func (TaskStatus) isDecision() {}

type CancelTask struct {
    TaskID tasks.TaskID
    Reason string
}
func (CancelTask) isDecision() {}

type SpawnSpec struct {
    Description string
    Query       string
    Priority    int
    RetainTurn  bool
    FailFast    bool
    // PropagateOnCancel: "" (default; runtime maps to tasks.PropagateCascade)
    // or tasks.PropagateIsolate. Amends D-047's frozen field set — D-324.
    PropagateOnCancel string
}
```

```go
// internal/runtime/dispatch
var ErrTaskNotOwnDescendant = errors.New("dispatch: task is not a descendant this run spawned")
```

Reserved tool names (LLM-facing, `internal/planner/react`):
`_task_status` (args `{task_ids?: []string}`), `_cancel_task` (args
`{task_id: string, reason?: string}`).

## Test plan

- **Unit:** AC-3 through AC-6 (declaration presence, schema shape,
  translation, standalone-co-occurrence rejection, malformed/empty args) in
  `internal/planner/react/projector_test.go`; AC-12 in `prompt_test.go`;
  AC-7 through AC-10 in `internal/runtime/dispatch/dispatch_taskmgmt_test.go`
  (nil-registry unsupported, unknown/out-of-scope target, list-all vs
  explicit-ids, atomic partial-scope rejection, idempotent-already-terminal
  cancel); AC-11 in `internal/tasks/engine/engine_test.go`.
- **Integration:** AC-13 (cross-run isolation under the SAME session) —
  an in-package test in `internal/runtime/dispatch` is the natural home
  (the dispatch package IS the planner→registry wiring boundary, per
  AGENTS.md §17.2's in-package carve-out) using the REAL `inprocess`
  TaskRegistry driver, real identity, and asserting both the new
  descendant-scope rejection AND the untouched-target failure mode. AC-14
  (cancel hierarchy) is the second integration-shaped test, same package,
  same real driver, exercising direct-operator / agent-descendant /
  ancestor-cascade cancel paths against one shared task tree.
- **Conformance:** AC-15, run against both `inprocess` and `durable`
  drivers via the shared `internal/tasks/conformancetest` suite.
- **Concurrency / leak:** AC-16, N≥100 concurrent `_task_status` /
  `_cancel_task` dispatches against one shared `toolExecutor`, `-race`,
  goroutine-baseline restored.

## Smoke script additions

`scripts/smoke/phase-187.sh` (`# PREFLIGHT_REQUIRES: unit-tests`):

- Static greps: `_task_status` / `_cancel_task` reserved declarations
  present in `discovered_tools.go`; `propagate_on_cancel` present in the
  `_spawn_task` schema; the standalone guard names both new tools in
  `projector.go`; the shared cascade-walk helper name appears in BOTH
  `engine.go` and `groups.go` (duplication actually closed, not just
  described).
- `go test ./internal/planner/react/... -run
  'TestProjector.*TaskStatus|TestProjector.*CancelTask|TestProjector.*Standalone'
  -race` — translation-table + standalone-rejection coverage.
- `go test ./internal/runtime/dispatch/... -run
  'TestExecutor_(TaskStatus|CancelTask).*|TestExecutor.*NotOwnDescendant|TestExecutor.*ConcurrentReuse' -race`
  — descendant-scope rejection + concurrent-reuse.
- `go test ./internal/tasks/engine/... ./internal/tasks/conformancetest/... -run
  'TestCancel.*Isolate|TestConformance.*Isolate' -race` — isolate-vs-cascade
  and isolate-vs-operator-cancel scenario names.
- 404/405/501 → SKIP is N/A (unit-tests class, no live server surface);
  each block SKIPs with a named reason when its target symbol/test name is
  absent, so the script coexists with pre-187 builds.

## Coverage target

- `internal/planner/react` (touched paths): 85%
- `internal/runtime/dispatch` (touched paths): 85%
- `internal/tasks` (touched paths — `engine.go`, `groups.go`,
  `conformancetest`): 85%

## Dependencies

- 185
- 186

## Risks / open questions

- The cascade-walk fix (AC-11) touches shared, load-bearing lifecycle code
  (`internal/tasks/engine`) used by every existing SpawnTask/AwaitTask/group
  path. It is scoped narrowly (only the treatment of a MID-WALK descendant's
  own flag changes; the direct-target and start-the-walk decisions are
  untouched) and is fully covered by the existing conformance suite (AC-15)
  plus the pre-existing `Cancel_Cascade_PropagatesToChildren` /
  `Cancel_Isolate_LeavesChildrenAlone` regression tests, which this phase
  must keep green unmodified (neither exercises a MID-walk isolate node, so
  neither should change behavior).
- `_task_status`'s unbounded-`TaskIDs` (list-everything) path does a
  multi-level BFS over `TaskRegistry.List` bounded by
  `planner.absolute_max_spawn_depth` — acceptable because that same config
  already bounds total spawn-tree size; no new cap is introduced, but a
  pathological wide-and-deep spawn tree makes this call proportionally
  more expensive than a single `Get`. Documented, not mitigated further at
  V1 (RFC §11 candidate if it becomes a real operator complaint).
- The wave doc (`docs/plans/wave-v116-parallel-intent-coordination.md`)
  stages this phase in parallel with 188; neither depends on the other, but
  both touch `internal/tasks/engine`'s Cancel-adjacent surface indirectly
  (188 via group resolution notifications). Coordinate merge order if a
  conflict surfaces — not a design risk, a sequencing note.

## Glossary additions

- `_task_status`
- `_cancel_task`
- cancel hierarchy

`docs/glossary.md`'s existing **PropagateOnCancel** and **SpawnSpec**
entries need amending (not fresh additions) in the same PR: PropagateOnCancel
to note the AC-11 cascade-walk fix and that a descendant's own flag now
matters mid-walk, not just at the direct target; SpawnSpec to note the
D-324 `PropagateOnCancel` field addition.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test passes (AC-13)
- [ ] N/A — this phase does not construct a NEW long-lived reusable
      artifact; it extends the existing `toolExecutor` (already
      concurrent-reuse-covered) and the existing `Engine` (already
      concurrent-reuse-covered). AC-16 extends the existing coverage rather
      than opening a new D-025 surface.
- [ ] Integration tests exist for the seam this phase closes (AC-13, AC-14),
      real `inprocess` driver, identity propagation, ≥1 failure mode,
      `-race`
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry filed (N/A — no departure; D-324 filed regardless, as an
      amendment)
