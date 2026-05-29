# Phase 107e — SpawnTask + AwaitTask dev-executor dispatch (background-task execution)

## Summary

Closes the last `ErrDecisionShapeUnsupported` carve-out the dev binary still carries for planner-emitted background work. Phase 47 (D-056) shipped the runtime machinery — the `planner.SpawnTask` / `planner.AwaitTask` Decision shapes, the React emission of them via the native `_spawn_task` / `_await_task` meta-tools (re-confirmed on the native path by 107c), the `tasks.TaskRegistry.Spawn` surface, the `WatchGroup` + `GroupCompletion` wake-up contract, and the group surface. But the **dev `ToolExecutor`** — the only `steering.ToolExecutor` V1.1.x ships (`cmd/harbor/cmd_dev_executor.go`, Phase 83i / D-152) — still rejects both `SpawnTask` and `AwaitTask` with `ErrDecisionShapeUnsupported` (`cmd_dev_executor.go:104-109`), and the per-task RunLoop driver drives **foreground tasks only** (`cmd_dev_runloop.go:67-74`), explicitly deferring background-task execution: *"The runtime dispatch executor (a later phase) is the right home for background task execution."* This is that phase. The net effect of the gap today: a ReAct planner can *emit* `_spawn_task`, the projector translates it to a real `planner.SpawnTask` (107c), and then the dev executor refuses it — so the spawn never happens, and even if it did the spawned `KindBackground` task would sit at `StatusPending` forever (the original D-097 dead-task bug, one layer in).

107e wires the pair end-to-end on the dev path:

1. **`devToolExecutor` gains a `tasks.TaskRegistry` dependency** and a `SpawnTask` branch that maps `planner.SpawnTask` → a `tasks.SpawnRequest` (Kind; Identity from `rc.Quadruple`; ParentTaskID; Description; Query; Priority; GroupID; NotifyOnComplete) and calls `Spawn`. For a non-retain-turn spawn it returns immediately with a `{task_id, kind, status:"spawned"}` observation; for a retain-turn spawn it blocks (ctx-bounded) polling `Get` until the task is terminal and returns its outcome — both modes expressible against the **synchronous** steering RunLoop without runloop surgery.
2. **The per-task RunLoop driver learns to drive `KindBackground` tasks** (flip the foreground-only filter to accept background too), gated by a **spawn-depth cap** so a background task that itself emits `SpawnTask` cannot recurse without bound — directly answering the "recursive planner loop" concern `cmd_dev_runloop.go:71` raised. The spawned background task is picked up via the existing `task.spawned` subscription and driven through the identical RunContext-build → `Run` → `MarkComplete`/`MarkFailed` answer-envelope path foreground tasks already use.
3. **`AwaitTask` dispatch** — polls `Get` on the awaited `TaskID` (ctx-bounded) until it is terminal, and returns the awaited task's `TaskResult` answer-envelope (or error) as the observation, with D-026 `projectForLLM` applied defensively so a heavy awaited result never trips `ErrContextLeak`.

**SpawnTask and AwaitTask are wired together, not split** — CLAUDE.md §13 is explicit: *"`SpawnTask` and `AwaitTask` emission MUST land in the same phase. A planner that can spawn a background task but cannot join it produces orphan work the runtime cannot recover."* That rule pinned the *emission* twin at Phase 47; the same logic binds the *dispatch* twin here. Wiring spawn without join on the dev path would produce exactly the orphan background work §13 forbids. The user's request named `SpawnTask`; the join consumer rides with it by rule.

## RFC anchor

- RFC §6.8 — Tasks (unified foreground/background). The `SpawnRequest` / `TaskHandle` / `WatchGroup` / `GroupCompletion` surface is unchanged; this phase is its first dev-binary planner-driven consumer for the background kind.
- RFC §6.2 — Planner interface + Decision sum. `SpawnTask` / `AwaitTask` shapes are unchanged (Phase 42); this phase wires their dispatch into the dev `ToolExecutor` and flips the dev driver's task-kind filter.
- RFC §6.5 — LLM client tool-calling contract / D-026 heavy-output discipline. The `AwaitTask` observation (an awaited task's result) and a retain-turn `SpawnTask` observation are projected through the same `projectForLLM` discipline the `CallTool` path uses, so an awaited heavy result is artifact-stub-shaped before it reaches the next prompt.

## Briefs informing this phase

- brief 02 — planner + steering + HITL (the `Decision` sum's `SpawnTask`/`AwaitTask` shapes; the steering RunLoop's synchronous `ToolExecutor` dispatch seam; the D-032 wake-mode contract).
- brief 05 — state, tasks, artifacts, sessions, distributed (the unified foreground/background `TaskRegistry`, the `WatchGroup` + `GroupCompletion` wake-up mechanism, the group lifecycle FSM).
- brief 03 — tools + integrations + LLM client (the D-026 heavy-output projection the await/retain-turn observation must honour before reaching the LLM edge).

## Brief findings incorporated

- **brief 02 §"Decision sum is orthogonal to dispatch".** The planner→runtime contract is the `Decision` sum; how the runtime dispatches each shape is the runtime's concern. 107e changes only the dev `ToolExecutor`'s handling of two already-emitted shapes — no `Decision`-shape change, no planner change.
- **brief 05 §"one wake mechanism".** `WatchGroup` + `GroupCompletion` + per-task `Get` are reads of the ONE wake surface (push / poll / hybrid — `groups.go:11-25`). 107e consumes the **poll** mode (`Get` until terminal) for the retain-turn spawn and the await join; it adds no second wake mechanism (§13).
- **brief 05 §"identity is mandatory on spawn".** `Spawn` returns `ErrIdentityRequired` on an incomplete triple. 107e fills `SpawnRequest.Identity` from `rc.Quadruple.Identity` and never from a global — the spawned task inherits the originating run's `(tenant, user, session)` triple (CLAUDE.md §6).
- **brief 02 §"the runloop is synchronous at V1".** The steering RunLoop dispatches every non-`Finish`/non-`RequestPause` decision through `ToolExecutor.ExecuteDecision` and appends one trajectory step (`runloop.go:585-629`); there is no eager push re-entry in the loop. 107e fits both spawn modes into that synchronous shape rather than adding a re-entry path.

## Findings I'm departing from (if any)

- **Departure from D-032's eager push wake-on-resolution, on the dev path.** D-032 / the master-plan Phase 45 + 47 detail describe ReAct's `push` wake mode as: a non-retain-turn `SpawnTask` returns control to the runtime, the runtime registers against `tasks.WatchGroup`, and on `GroupCompletion` the runtime **re-invokes `Planner.Next`** with the resolved `MemberOutcome` surfaced through `RunContext.Trajectory.Background`. The steering RunLoop V1.1.x ships (`internal/runtime/steering/runloop.go`) is **synchronous** — it has no group-watcher re-entry path; it dispatches a decision, appends the observation, and re-enters the planner on the next step. Wiring eager push re-entry would require steering-runloop surgery (a watcher registration + a re-entry trigger keyed off `GroupCompletion`) that is out of scope for a dev-executor dispatch phase. **On the dev path the realizable shapes are:** (a) **retain-turn `SpawnTask`** — the executor spawns and blocks in-decision on `WatchGroup`, returning the resolved outcome as that step's observation (synchronous spawn-and-join); and (b) **non-retain-turn `SpawnTask` + explicit `AwaitTask`** — the executor spawns, returns `{task_id}` immediately, the planner continues stepping, and joins by emitting `AwaitTask` which blocks on `WatchGroup`. Both are correct and produce concurrent background execution; what is NOT realized is "the runtime wakes the planner with the result *without* an explicit `AwaitTask`." Documented in D-170; the eager-push re-entry is filed as a steering-runloop follow-up (see Risks). `RunContext.Trajectory.Background` is populated by the `AwaitTask`/retain-turn observation path rather than by a runtime-injected wake.
- **Departure from the plan's original `WatchGroup` join → `Get`-poll (D-170 call 3).** `WatchGroup(sessionID, groupID)` watches a *group*; `AwaitTask` carries a single `TaskID`, and the persisted `tasks.Task` record exposes no `GroupID` to resolve a group from. The registry's own group docs (`internal/tasks/groups.go`) bless `Get(taskID)` polling as a first-class poll-mode wake (cheap in-memory map lookup on the dev path). So the executor polls `Get` until the task is terminal (bounded by ctx, `spawnAwaitPollInterval` cadence) instead of subscribing to `WatchGroup`. This is identity-safe (`Get` rejects cross-session reads — closing AC-11 for free), needs no group resolution, and adds no second wake mechanism. `WatchGroup` stays the group-fan-in surface for programmatic planners and a future eager-push runloop.
- **Departure from `cmd_dev_runloop.go:67-74`'s "foreground only" filter.** That filter was a deliberate Phase 83i placeholder ("driving a planner against a background task would create a recursive planner loop … the runtime dispatch executor (a later phase) is the right home"). 107e is that later phase: it flips the filter to also drive `KindBackground`, and closes the recursion concern with an explicit **spawn-depth cap** (a bounded `ParentTaskID`-chain depth read at spawn time; a background task at max depth that emits `SpawnTask` fails the spawn loudly with a capped-depth error surfaced as the step observation, never a silent drop). Documented in D-170.

## Goals

- **Executor `SpawnTask` branch.** `cmd/harbor/cmd_dev_executor.go`'s `ExecuteDecision` replaces the `SpawnTask` `ErrDecisionShapeUnsupported` reject with a real `tasks.TaskRegistry.Spawn` dispatch. Identity comes from `rc.Quadruple`; `ParentTaskID` is the originating run's task. Non-retain-turn returns `{task_id, kind, status}` immediately; retain-turn blocks (ctx-bounded) polling `Get` until terminal and returns the outcome.
- **Executor `AwaitTask` branch.** `ExecuteDecision` replaces the `AwaitTask` reject with: poll `Get` on the awaited task (ctx-bounded) until terminal, return its `TaskResult` answer-envelope (or error) as the observation. `CallParallel` keeps its existing handling (107d wires it; 107e does not touch it).
- **Background-task driving.** The per-task RunLoop driver (`cmd/harbor/cmd_dev_runloop.go`) drives `KindBackground` tasks in addition to `KindForeground`, so a spawned background task actually runs its `Query` through a planner sub-run and reaches a terminal `MarkComplete`/`MarkFailed` with the standard answer-envelope.
- **Spawn-depth cap (recursion guard).** A configurable absolute cap on the `ParentTaskID`-chain depth; a `SpawnTask` that would exceed it fails loudly (the step observation carries a capped-depth error) so a background sub-agent cannot recurse without bound. No silent drop (§13).
- **D-026 projection on the await/retain observation.** The awaited / retained outcome is run through the existing `projectForLLM` so a heavy result becomes an artifact-stub before it reaches the LLM edge — a parallel/await observation never trips `ErrContextLeak`.
- **Identity propagation + isolation.** The spawned task inherits the originating run's `(tenant, user, session)` triple; the await `Get` poll runs under that triple; one session's spawned/awaited tasks are never visible to another (`Get` rejects cross-session reads).

## Non-goals

- **Eager push wake-on-resolution in the steering RunLoop.** Re-entering `Planner.Next` on `GroupCompletion` *without* an explicit `AwaitTask` requires steering-runloop changes; out of scope. The dev path uses retain-turn block-in-decision or explicit `AwaitTask` join (see Findings I'm departing from / Risks). Filed as a steering follow-up.
- **`CallParallel` dispatch.** Phase 107d owns the `CallParallel` executor branch; 107e leaves it exactly as 107d ships it.
- **Parallel / batch spawn in one turn.** 107d's AC-21 rejects a reserved control name (`_spawn_task`) co-occurring with other tool-calls; 107e does not revisit that. Background tasks already run concurrently once spawned (spawn N across N steps → N concurrent tasks); a same-turn batch-spawn meta-tool (`_spawn_tasks`) stays a documented future option, not 107e scope.
- **Durable background tasks across restart.** Phase 87 (Post-V1) owns the durable TaskService backend. 107e uses whatever driver the dev stack already opened (in-mem default); a background task does not survive a `harbor dev` restart. Documented limitation.
- **A production (non-dev) server `ToolExecutor`.** As with 107d, the dev `ToolExecutor` is the only consumer V1.1.x ships; a production server executor is out of scope.
- **`SpawnTool` (the tool-task lifecycle).** `tasks.SpawnTool` (Phase 20 stub) is a separate surface; 107e wires planner `SpawnTask` (a sub-agent run), not `SpawnTool`.

## Acceptance criteria

The bullets below are binding. Numbering is sequential.

### Executor dispatch

- [ ] **AC-1** `devToolExecutor` gains a `tasks.TaskRegistry` field, set once in its constructor (`newDevToolExecutor`) from the SAME registry `bootDevStack` already constructs (`cmd/harbor/cmd_dev.go`). The field is immutable after construction (D-025); the executor stays concurrent-safe (the registry is itself concurrent-safe per its interface contract).
- [ ] **AC-2** `ExecuteDecision`'s `case planner.SpawnTask` replaces the `ErrDecisionShapeUnsupported` reject with a `taskReg.Spawn` dispatch. The `SpawnRequest` is built as: `Identity` = `rc.Quadruple.Identity` (NEVER a global); `Kind` = `d.Kind` (default `KindBackground` when empty — the projector already defaults it, 107c); `ParentTaskID` = the originating run's `TaskID` (from `rc`); `Description`/`Query`/`Priority` = `d.Spec.*`; `GroupID` = `d.GroupID`; `NotifyOnComplete` = true. A `Spawn` error (incl. `ErrIdentityRequired`) is surfaced as the step's error observation — never swallowed.
- [ ] **AC-3** Non-retain-turn `SpawnTask` (`d.Spec.RetainTurn == false`) returns immediately. The observation is `{task_id, kind, status:"spawned"}`; the `llmObservation` is the same compact shape (small — no D-026 promotion needed). The planner's next step sees the `task_id` and may join via `AwaitTask`.
- [ ] **AC-4** Retain-turn `SpawnTask` (`d.Spec.RetainTurn == true`) blocks (bounded by the per-step `ctx`) polling `taskReg.Get` until the spawned task reaches a terminal status (`Complete`/`Failed`/`Cancelled`), then returns its outcome observation (the answer-envelope or error). A `ctx` cancellation surfaces as a wrapped error observation, not a hang. **Deviation from the original WatchGroup wording (D-170 call 3):** the join polls `Get` rather than `WatchGroup` because `tasks.Task` exposes no `GroupID` to resolve a single task's group from, and the registry blesses `Get`-polling as a poll-mode wake (`internal/tasks/groups.go`).
- [ ] **AC-5** `ExecuteDecision`'s `case planner.AwaitTask` replaces the `ErrDecisionShapeUnsupported` reject with: an identity-scoped poll of `taskReg.Get(ctx, d.TaskID)` (ctx-bounded) until the task reaches a terminal status, then return the task's outcome (its `TaskResult` answer-envelope, or `{code, message}` when failed/cancelled) as the observation. `Get` rejects a task not visible to the ctx identity, so a cross-session/cross-tenant or missing id surfaces as an error observation (closing AC-11 for free). An empty `TaskID` fails loud (`projectResponse` already rejects empty await ids at emission, 107c; the executor re-asserts it defensively).
- [ ] **AC-6** D-026 projection. The retain-turn (AC-4) and await (AC-5) observations are run through the existing `projectForLLM` so a heavy awaited result becomes an artifact-stub `llmObservation` while the raw `observation` keeps the full value. An await/retain observation with a heavy result MUST NOT trip the LLM-edge `ErrContextLeak` guard.

### Background-task driving

- [ ] **AC-7** The per-task RunLoop driver (`cmd/harbor/cmd_dev_runloop.go`) drives `KindBackground` tasks in addition to `KindForeground` — the `taskKind` filter (`cmd_dev_runloop.go:67-74`) is widened (or the driver is constructed to accept both kinds). A spawned background task is picked up via the existing `task.spawned` subscription, advanced through `MarkRunning` → `Run` → `MarkComplete`/`MarkFailed`, and produces the standard `{answer, finish_reason, tool_calls_seen}` answer-envelope `TaskResult`. The existing foreground path is unchanged.
- [ ] **AC-8** Spawn-depth cap. The dispatch reads the spawning run's `ParentTaskID`-chain depth (via repeated `Get`, or a depth field threaded onto `SpawnRequest`/`Task`) and rejects a `SpawnTask` that would exceed `absolute_max_spawn_depth` (config, AC-12) — the reject surfaces as the step's error observation (the planner re-plans / finishes), NEVER a silent drop. A background task at depth `< cap` may itself spawn; one at the cap may not. Default cap is conservative (e.g. 4).
- [ ] **AC-9** Join by `task_id`, not group. `AwaitTask` resolves the awaited task directly by `task_id` via `Get` (AC-5) — the planner only needs the `task_id` the spawn observation returned. A `SpawnTask` may still carry a `GroupID` (passed through to `SpawnRequest.GroupID` for cross-task fan-in), but the dev join path does not depend on group resolution (D-170 call 3).

### Identity / isolation

- [ ] **AC-10** Identity propagation. The spawned task carries the originating run's full `(tenant, user, session)` triple (AC-2); the spawned sub-run's `RunContext.Quadruple.Identity` equals the parent's triple (RunID differs — the child task IS its own run). The await `Get` poll (AC-5) runs under that same triple. An integration test asserts the triple flows to the spawned run.
- [ ] **AC-11** Cross-session isolation. A session-A run that spawns + awaits a task cannot `Get` a session-B task; the registry rejects the cross-session read and the executor surfaces it as an error observation. The concurrent-reuse test (AC-16) runs N goroutines each under its own identity and asserts no cross-talk (each await sees only its own child's result).

### Config

- [ ] **AC-12** `internal/config/config.go` gains `planner.absolute_max_spawn_depth int` (yaml: `absolute_max_spawn_depth`, default 4; ≤ 0 resolves to the default). `internal/config/validate.go` accepts it (reject negatives with a clear message, or clamp-to-default — pin the choice with a test). `cmd/harbor/cmd_dev.go::bootDevStack` plumbs it into the executor + driver. Document it in the example config.

### Tests

- [ ] **AC-13** Unit (Go) — `cmd/harbor/cmd_dev_spawn_await_test.go`: non-retain-turn `SpawnTask` → `taskReg.Spawn` called with the parent triple + (when RunID set) parent task id; observation carries `task_id`+`status`. Nil registry → `ErrDecisionShapeUnsupported` (no panic). `AwaitTask` empty id → error; unknown/cross-session id → error observation (no hang); completed task → observation carries the parsed answer-envelope `result`; failed task → observation carries `{error:{code,message}}`. A heavy awaited result → artifact-stub `llmObservation` (`truncated:true`) while raw keeps the full value (AC-6).
- [ ] **AC-14** Unit (Go) — `cmd/harbor/cmd_dev_runloop_test.go`: with `driveBackground:true` the driver drives a `KindBackground` task end-to-end (spawned → MarkRunning → Run → MarkComplete with answer-envelope); the existing `driveBackground` unset path (foreground-only, background skipped) is preserved as a regression. Spawn-depth cap (AC-8) is exercised in `cmd_dev_spawn_await_test.go`: a spawn at the cap is rejected naming `absolute_max_spawn_depth`; below the cap succeeds.
- [ ] **AC-15** End-to-end (Go) — `cmd/harbor/cmd_dev_spawn_await_test.go::TestSpawnThenAwait_BackgroundDrivenEndToEnd` (real drivers on the seam, no mocks): a non-retain `SpawnTask` through the production `devToolExecutor` spawns a background task; a real per-task RunLoop driver (`driveBackground:true`) over a real `TaskRegistry` drives it to completion; the parent's `AwaitTask` step receives the child's answer-envelope; identity propagates to the child run; a sibling test covers the failure mode (a `MarkFailed` child → the await observation carries the `{error}` block); runs under `-race`. **Deviation from the plan's original `test/integration/` location:** the test lives in `cmd/harbor` (package `main`) because the executor + driver are unexported package-main types `test/integration` cannot import; the devstack mirror (D-094) is intentionally CallTool-only (107d's `callParallel` isn't mirrored either), so no devstack change.
- [ ] **AC-16** Concurrent-reuse (Go) — `cmd/harbor/cmd_dev_spawn_await_test.go::TestSpawnAwait_ConcurrentReuse`: N≥100 concurrent spawn+complete+await cycles against ONE shared `devToolExecutor` + ONE shared `TaskRegistry`, each goroutine under its own identity, under `-race`. Asserts no cross-talk (each await sees only its own child's answer), no data race, baseline goroutine count restored after all cycles return — the await pollers stop on return (ticker stopped via defer) (CLAUDE.md §11 + D-025).

### Drift / hygiene

- [ ] **AC-17** `docs/decisions.md` D-170 entry — pins: the dev-executor `SpawnTask`/`AwaitTask` dispatch; the synchronous-runloop adaptation (retain-turn block-in-decision + explicit `AwaitTask` join in lieu of eager push re-entry); the `Get`-poll join (not `WatchGroup`, forced by `Task` having no `GroupID`; no second wake mechanism); the driver's background-kind widening + spawn-depth cap; the SpawnTask+AwaitTask-twin-by-§13 rationale. References D-056, D-032, D-152, D-097/D-098, brief 02, brief 05.
- [ ] **AC-18** Glossary: update `_spawn_task` / `_await_task` (now have a dev-binary dispatch consumer; note the synchronous-runloop / `Get`-poll join semantics) and add `absolute_max_spawn_depth` (the operator-yaml recursion cap).
- [ ] **AC-19** `docs/skills/drive-the-playground/SKILL.md` (+ any skill demonstrating background work) — per CLAUDE.md §18 same-PR drift: note that an agent can now spawn background sub-tasks (`_spawn_task`) and join them (`_await_task`) within a dev run, and the `absolute_max_spawn_depth` cap. Keep operator-facing and minimal.

## Files added or changed

### Runtime — dev executor + bootstrap

- `cmd/harbor/cmd_dev_executor.go` — AC-1..AC-6 + AC-8: `tasks.TaskRegistry` field; `SpawnTask` + `AwaitTask` branches (spawn / retain-turn block / await-join), spawn-depth check, per-branch `projectForLLM` on the await/retain observation. ~150 LOC.
- `cmd/harbor/cmd_dev.go` — pass the `TaskRegistry` into `newDevToolExecutor`; plumb `planner.absolute_max_spawn_depth` into the executor + driver. ~20 LOC.
- `cmd/harbor/cmd_dev_runloop.go` — AC-7: `driveBackground` opt-in on the driver (opts + struct + constructor + `drivesKind` filter). ~40 LOC.
- `cmd/harbor/cmd_dev_spawn_await_test.go` — **NEW** AC-13 + AC-15 + AC-16 (executor unit + end-to-end + concurrent-reuse). ~430 LOC.
- `cmd/harbor/cmd_dev_runloop_test.go` — AC-14 (`DrivesBackgroundTasks_WhenEnabled`; the existing `SkipsBackgroundTasks` becomes the `driveBackground` unset regression). ~60 LOC.
- `cmd/harbor/cmd_dev_executor_parallel_test.go` — updated for the `newDevToolExecutor` signature change (107d test caller).

### Runtime — config

- `internal/config/config.go` — AC-12 `absolute_max_spawn_depth` field + `SpawnDepthCap()` accessor. ~12 LOC.
- `internal/config/validate.go` — AC-12 validation (reject negative). ~6 LOC.
- `internal/config/validate_test.go` — AC-12 tests (reject negative; accessor default + override).

### Docs

- `docs/decisions.md` — AC-17 D-170 entry. ~30 lines.
- `docs/glossary.md` — AC-18 updates + `absolute_max_spawn_depth`. ~8 lines.
- `docs/skills/drive-the-playground/SKILL.md` — AC-19 prose. ~8 lines.
- `examples/dev.yaml` (or equivalent) — illustrate `planner.absolute_max_spawn_depth`. ~3 lines.

### Smoke

- `scripts/smoke/phase-107e.sh` — **NEW**. PREFLIGHT_REQUIRES: live-server. See "Smoke script additions".

## Public API surface

- `newDevToolExecutor` signature gains a `tasks.TaskRegistry` parameter (cmd/harbor-internal; not an exported-package change).
- `config`: `planner.absolute_max_spawn_depth int` — new optional yaml field, default 4.
- No new `Decision` shapes, no Protocol-surface change, no new LLM wire types, no new `TaskRegistry` interface methods — `SpawnTask`, `AwaitTask`, `SpawnRequest`, `WatchGroup`, `GroupCompletion`, `MemberOutcome` all already exist.

## Test plan

### Unit (Go)

- `cmd/harbor/cmd_dev_executor_*_test.go` — AC-13 (spawn / retain-turn / await / heavy-result projection / error observations), AC-16 (concurrent reuse + no watcher leak).
- `cmd/harbor/cmd_dev_runloop_test.go` — AC-14 (background-kind driving + spawn-depth cap; foreground regression).
- `internal/config/*_test.go` — AC-12 (field round-trips; validation choice pinned).

### Integration (Go)

- `test/integration/phase107e_spawn_await_test.go` — AC-15: real `TaskRegistry` on the seam, ReAct + scripted LLM emitting `_spawn_task`/`_await_task`, identity propagation to the child run + watcher, ≥1 failure mode (failed child → parent await observation carries the error), `-race`.

### Conformance

- N/A — the `Decision` sum, the `TaskRegistry` interface, and the identity contract are unchanged. Verify the Phase 49 planner conformance pack + the tasks driver conformance suite stay green (`go test -race ./internal/planner/conformance/... ./internal/tasks/...`).

### Concurrency / leak

- AC-16 — N≥100 concurrent spawn+await dispatches against one shared executor + one shared registry, `-race`, baseline goroutine restore (await pollers stop on return — ticker stopped via defer).

## Smoke script additions

`scripts/smoke/phase-107e.sh` — PREFLIGHT_REQUIRES: live-server. Assertions:

1. SKIP when no LLM provider key.
2. Static: `cmd/harbor/cmd_dev_executor.go` no longer returns `ErrDecisionShapeUnsupported` for `SpawnTask` / `AwaitTask` (greps the dispatch branches).
3. Bootstrap a dev token (Phase 105 endpoint).
4. POST `/v1/control/start` with a query that elicits a spawn-then-join shape (e.g. "research these two sub-questions independently, then combine the answers").
5. Subscribe to `/v1/events/subscribe`; assert a `task.spawned` event fires for a `KindBackground` task during the parent run, and the spawned task reaches `task.completed`.
6. Wait for the parent `task.completed`; fetch `tasks.get`; assert the parent trajectory has a `SpawnTask` step (observation carries a `task_id`) and an `AwaitTask` step (observation carries the child's answer).
7. Assert no `ErrContextLeak` in the server log (D-026 projection held on the await observation).
8. Assert a spawn beyond `absolute_max_spawn_depth` is rejected (optional — only if a depth-exceeding query is scripted).

## Coverage target

- `cmd/harbor/`: maintain existing target (the dev executor + driver are the main new surface).
- `internal/config/`: 80% (existing).
- `internal/tasks/`: unchanged (no new code; reuse only).

## Dependencies

- 107c — native tool-calling cutover (D-167): the source of the `_spawn_task` / `_await_task` native meta-tool emissions and the `translateNativeSpawn`/`translateNativeAwait` projectors. **Hard dependency — has landed.**
- 47 — parallel + `SpawnTask`/`AwaitTask` emission + the `TaskRegistry` group surface (D-056). This phase is the first dev-binary planner-driven consumer of the background-spawn + `WatchGroup` surface (closes the §13 primitive-without-(dev)-consumer gap the spawn/await dispatch has carried since Phase 47).
- 83i — the dev `ToolExecutor` seam (D-152). This phase extends `ExecuteDecision`'s switch.
- 83f — the per-task driver's RunContext consumer wiring (D-149) the background sub-run reuses.
- D-097 / D-098 — the per-task RunLoop driver + FSM-transition bridge this phase extends to the background kind.
- (Soft) 107d — `CallParallel` dev dispatch; independent of 107e, but both extend the same `ExecuteDecision` switch — sequence the merges to avoid a trivial conflict in that switch.

## Risks / open questions

- **Eager push wake-on-resolution is deferred, not delivered.** D-032's "runtime re-invokes `Planner.Next` on `GroupCompletion` without an explicit `AwaitTask`" is NOT realized here (the steering RunLoop is synchronous). The dev path uses retain-turn block-in-decision or explicit `AwaitTask`. If an operator workload wants fire-and-forget background work the planner joins implicitly, that's the trigger to pull a steering-runloop re-entry extension forward — **file the follow-up; do not silently approximate it.** The plan's Findings section pins this; D-170 records it.
- **Blocking a foreground step on the await poll holds a planner step open.** A retain-turn spawn or an `AwaitTask` blocks the synchronous runloop step until the child task resolves (bounded by ctx / the run's deadline). For long-running background work this ties up the foreground turn — acceptable for V1.1.x dev workloads (short sub-goals), but the await must be ctx-bounded and the poll ticker must always be stopped (AC-4/AC-5/AC-16). A child that never terminates must surface as a ctx-deadline error observation, not a hang.
- **Spawn-depth cap shape.** AC-8 leaves the depth-read mechanism to the implementer (repeated `Get` up the `ParentTaskID` chain vs. a depth int threaded onto `SpawnRequest`/`Task`). The threaded-int is cheaper but touches the task record shape; the chain-walk is read-only but O(depth). Pin the choice with AC-14's test. Whichever is chosen, the cap MUST fail loud (error observation), never silently drop the spawn.
- **In-mem driver = no durability.** A background task does not survive a `harbor dev` restart (Phase 87 owns durability). If a restart lands mid-await, the await surfaces a not-found/cancelled error observation — acceptable, but the smoke + integration tests should not assume cross-restart survival.
- **Merge ordering with 107d.** Both phases edit the `ExecuteDecision` switch (`cmd_dev_executor.go`). The user is implementing 107d in parallel; whichever merges second rebases the switch. Trivial, but call it out in the PR so the reviewer expects the touch.
- **Recursive background agents.** Even with the depth cap, a fan-out of background tasks (each spawning the max allowed) can multiply work. The cap bounds depth, not breadth. If breadth becomes a problem, a per-run total-spawn budget is the follow-up — out of scope here, noted so it isn't a surprise.

## Glossary additions

- **`absolute_max_spawn_depth`** — Phase 107e operator-yaml knob at `planner.absolute_max_spawn_depth` (int, default 4). Caps the `ParentTaskID`-chain depth of planner-spawned background tasks so a background sub-agent that itself emits `SpawnTask` cannot recurse without bound. A spawn that would exceed the cap is rejected loudly (the planner sees a capped-depth error observation), never silently dropped. (Alphabetised under A.)
- Updates (not new): `_spawn_task` / `_await_task` (now have a dev-binary dispatch consumer — the dev `ToolExecutor`; on the synchronous V1.1.x runloop a retain-turn `_spawn_task` blocks in-decision polling `Get`, and a non-retain-turn `_spawn_task` is joined by an explicit `_await_task`, not by eager push re-entry).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation: identity propagates to the spawned run AND the await `Get` poll (AC-10); concurrent-reuse test confirms no cross-talk under per-goroutine identities (AC-11/AC-16)
- [ ] **Concurrent-reuse — AC-16 (N≥100 spawn+await dispatches against one shared executor + one shared registry, `-race`, no watcher leak, goroutine baseline restored).**
- [ ] **Integration test — AC-15 (real `TaskRegistry` on the seam, identity propagation, ≥1 failing child, `-race`).**
- [ ] `SpawnTask` and `AwaitTask` dispatch landed TOGETHER (§13 — spawn without join is orphan work)
- [ ] Glossary updated per AC-18
- [ ] `docs/decisions.md` D-170 entry per AC-17
- [ ] Skill(s) updated per AC-19 (CLAUDE.md §18 same-PR drift)
- [ ] `examples/dev.yaml` shows `absolute_max_spawn_depth`
- [ ] Phase 49 planner conformance + tasks driver conformance packs green
- [ ] Live smoke against a tool-calling-capable provider confirms spawn → background run → await join, no `ErrContextLeak`
