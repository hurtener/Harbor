# Phase 193 — Planner-facing steer / pause / resume of a spawned child

## Summary

Phase 187 shipped `_task_status` and `_cancel_task` — the model can observe and
cancel the background tasks its OWN run spawned, descendant-scoped via the
`ParentTaskID` chain. It explicitly parked one verb as a "named future
extension": steering or pausing a spawned child from the parent's model turn.
The per-run steering inbox (`internal/runtime/steering/inbox.go`) already exists
per background sub-run and the unified pause/resume primitive already coordinates
pause — but neither was exposed as a planner-facing verb. This phase completes
the operator↔agent control taxonomy on the AGENT side: three new reserved
planner controls — `_steer_task`, `_pause_task`, `_resume_task` — become new
sealed `planner.Decision` shapes, projected in the React projector, dispatched
onto the EXISTING per-sub-run steering inbox via the unified pause/resume
primitive (never a reinvented pause path), and gated by 187's same
`isOwnDescendant` guard so a run can never steer/pause/resume a sibling's tasks.
Human/operator authority still supersedes: the operator can steer/pause/resume
ANY task; the agent only its own descendants.

## RFC anchor

- RFC §6.3
- RFC §6.8
- RFC §6.2
- RFC §3.3

## Briefs informing this phase

- brief 02
- brief 16

## Brief findings incorporated

- brief 02 §5 (sharp edge 4, "Magic strings as opcodes"): "Harbor's `Decision`
  is a sum type; tool calls and runtime opcodes are different shapes. Future
  runtime-level actions ... extend the sum, not the catalog of magic strings."
  Directly informs shipping `SteerTask` / `PauseTask` / `ResumeTask` as three
  new sealed `Decision` shapes rather than overloading `CallTool` args or a
  string-typed control channel — the same choice 187 made for `TaskStatusQuery`
  / `CancelTask`.
- brief 02 (unified pause/resume): the steering inbox + pause/resume are ONE
  primitive (RFC §3.3), shared by HITL, tool-side OAuth, A2A AUTH_REQUIRED, and
  operator/Console PAUSE. This phase adds a planner-facing *producer* of that
  primitive for a descendant sub-run — it does NOT introduce a new pause
  coordination path (§13 forbids that). Pause of a descendant parks that
  descendant's run through the existing pause mechanism; resume releases it
  through the existing resume mechanism; steer enqueues onto the existing
  per-sub-run steering inbox.
- brief 02 (authority ordering, HITL): human/operator authority is the top of
  the control hierarchy. This phase preserves that: the operator can
  steer/pause/resume ANY task via the existing Protocol control surface; the
  agent's new verbs are strictly descendant-scoped (187's `isOwnDescendant`
  guard, unchanged) and can never reach a sibling run's or the operator's own
  tasks.
- brief 16 §3 (the cancel hierarchy: human > agent > cascade): the SAME
  hierarchy 187 encoded for cancel now extends to steer/pause/resume — a run
  controls only what it spawned; the operator controls everything. This phase
  is the steer/pause/resume analogue of 187's cancel hierarchy, reusing its
  descendant-scope guard verbatim so the two taxonomies stay coherent.
- brief 16 §5 (power-with-brake, §13 primitive-with-consumer read as
  governance): each new reserved control ships in the SAME phase as a real
  dispatch consumer that exercises it end-to-end with a test — never a primitive
  without its consumer.

## Findings I'm departing from (if any)

None. This phase implements exactly the "named future extension" 187's Non-goals
section reserved ("Steering or pausing a spawned child from the parent's model
turn ... exposing [the per-run steering inbox] as a planner-facing verb is a
named future extension, not this phase."). It reuses 187's descendant-scope
guard and the existing pause/resume primitive rather than introducing new
mechanism, so no brief finding or settled decision is contradicted. D-330 is
filed as the sanctioned extension of D-324's control taxonomy, not a
re-litigation.

## Goals

- Ship `_steer_task`, `_pause_task`, `_resume_task` as reserved,
  natively-declared planner-control meta-tools, dispatched as three new sealed
  `planner.Decision` shapes (`SteerTask{TaskID tasks.TaskID; Directive string}`,
  `PauseTask{TaskID tasks.TaskID; Reason string}`, `ResumeTask{TaskID
  tasks.TaskID; Directive string}` — RFC §6.2) through the same
  projector-translation → executor-dispatch seam `_spawn_task` / `_await_task`
  / `_task_status` / `_cancel_task` already use. All three are non-terminal:
  the runtime executor dispatches them like `CallTool` and appends a trajectory
  step the planner observes on its next turn.
- Enforce descendant-only scoping for all three verbs at the dispatch layer via
  187's EXISTING `isOwnDescendant(ctx, targetID, callerID)` helper — a target
  outside the caller's `ParentTaskID` lineage (including a sibling run's tasks in
  the SAME session) is rejected loud with 187's `dispatch.ErrTaskNotOwnDescendant`
  sentinel, never silently narrowed or permitted. This is IN ADDITION TO the
  registry's `(tenant, user, session)` identity-visibility check, never instead
  of it (CLAUDE.md §6).
- Route steer onto the EXISTING per-sub-run steering inbox
  (`internal/runtime/steering/inbox.go`) — the same inbox the operator's
  steering already targets; route pause/resume through the EXISTING unified
  pause/resume primitive (RFC §3.3). No new pause coordination, no new steering
  channel (§13).
- Preserve human/operator supremacy: the operator's existing control surface
  reaches ANY task (unchanged); the agent's new verbs reach only its own
  descendants. Prove the ordering end-to-end (operator can pause/steer/resume a
  task the agent cannot; agent's verb on a sibling's task is rejected).
- Honor the fail-loud pause/resume serialization contract (§5, D-025): a pause
  of a descendant whose run state is unserializable raises `ErrUnserializable`
  loudly — never a silent `nil` drop.
- Teach the three new tools honestly in the reserved planner-control prompt
  surface, and update the one operator skill that documents this surface.

## Non-goals

- No new pause/resume MECHANISM. This phase is a new planner-facing PRODUCER of
  the existing primitive (RFC §3.3); it adds zero new pause reasons at the
  coordination layer beyond routing an agent-issued pause/resume/steer of a
  descendant through the existing inbox + pause primitive.
- No steer/pause/resume of a SIBLING run's tasks, the operator's own tasks, or
  the run's OWN task (self is not a descendant — 187's `isOwnDescendant`
  returns `false` for `targetID == callerID`; a run does not steer itself
  through this surface).
- No batchable `_steer_task` / `_pause_task` / `_resume_task`. Structurally
  excluded exactly like 187 excluded `_task_status` / `_cancel_task`:
  `planner.Batch`'s shape has no slot for these types, and the projector's
  standalone-name guard gains all three names. Widening to batchable later is
  additive; retracting a shipped batchable surface is not — the conservative
  choice is binding for this wave.
- No new operator/Console Protocol method for steering a descendant — the
  operator's existing steering + pause/resume control surface already reaches
  any task; this phase only adds the AGENT-side verbs. (188/#532-style
  conversational mirroring of an agent-issued pause is out of scope here.)
- No change to `WatchGroup` / `GroupCompletion` or the cancel hierarchy
  (187/D-324). Cancel stays cancel; this phase adds the orthogonal
  steer/pause/resume verbs.

## Acceptance criteria

- [ ] **AC-1** `internal/planner/decision.go`: three new sealed `Decision`
      shapes — `SteerTask{TaskID tasks.TaskID; Directive string}`,
      `PauseTask{TaskID tasks.TaskID; Reason string}`, `ResumeTask{TaskID
      tasks.TaskID; Directive string}` — matching RFC §6.2, each with an
      `isDecision()` marker. Godoc names the steer/pause/resume FEATURE and the
      descendant-scope + human-supremacy invariant in feature terms (no
      godoc-visible phase/decision/wave numbers — CLAUDE.md §13).
- [ ] **AC-2** `internal/planner/react/react.go`: `SteerTaskToolName =
      "_steer_task"`, `PauseTaskToolName = "_pause_task"`, `ResumeTaskToolName =
      "_resume_task"` reserved-name constants, godoc mirroring
      `SpawnTaskToolName` / `CancelTaskToolName`.
- [ ] **AC-3** `internal/planner/react/discovered_tools.go`:
      `reservedPlannerControlDeclarations()` returns three more entries.
      `jsonSchemaRawSteerTask` — `{task_id: string (required), directive:
      string (required)}`; `jsonSchemaRawPauseTask` — `{task_id: string
      (required), reason?: string}`; `jsonSchemaRawResumeTask` — `{task_id:
      string (required), directive?: string}`. All pin
      `additionalProperties: false` at every object level (OpenAI strict-mode
      parity). Tool descriptions state plainly that these verbs only reach tasks
      this run itself spawned (directly or transitively), that the operator can
      always override, and that a paused descendant resumes only via
      `_resume_task` or an operator resume.
- [ ] **AC-4** `internal/planner/react/projector.go`: `projectResponse`'s head
      switch gains a `case` per new tool name → `translateNativeSteerTask` /
      `translateNativePauseTask` / `translateNativeResumeTask`. Each parses its
      JSON args; an empty `task_id` (or empty `directive` where required) fails
      loud with wrapped `planner.ErrInvalidDecision` (mirrors 187's
      `translateNativeCancelTask` / `translateNativeAwait` empty-id guard
      verbatim); malformed JSON fails loud the same way.
- [ ] **AC-5** `internal/planner/react/projector.go`: the standalone-name guard
      (187 extended it with `_task_status` / `_cancel_task`) gains all three new
      tool names. Any response where a steer/pause/resume control co-occurs with
      ANY other tool-call is rejected loud with `planner.ErrInvalidDecision`
      naming the offending control. `_spawn_task` remains batchable, unaffected.
- [ ] **AC-6** `internal/runtime/dispatch/dispatch.go`: `ExecuteDecision`'s
      switch gains `case planner.SteerTask`, `case planner.PauseTask`, `case
      planner.ResumeTask` → new executor methods `steerTask` / `pauseTask` /
      `resumeTask`. Each attaches the run's identity via `identity.With` exactly
      like `spawnTask` / `cancelTask` (never a global context, CLAUDE.md §6).
- [ ] **AC-7** All three executor methods call 187's EXISTING
      `isOwnDescendant(ctx, d.TaskID, rc.Quadruple.RunID)` BEFORE touching the
      inbox / pause primitive; a negative result produces 187's existing
      `dispatch.ErrTaskNotOwnDescendant` wrapped with the offending id — no new
      scope sentinel is minted (the taxonomy stays one guard, one sentinel).
- [ ] **AC-8** `steerTask` enqueues the directive onto the target sub-run's
      EXISTING per-sub-run steering inbox (`internal/runtime/steering/inbox.go`)
      — the SAME inbox the operator's steering targets — resolved from the
      descendant task's run handle; it never opens a second steering channel.
      Returns `{task_id, steered: bool}` (idempotent-on-terminal: steering a
      terminal descendant returns `steered: false`, not an error, mirroring
      187's `_cancel_task` contract).
- [ ] **AC-9** `pauseTask` / `resumeTask` drive the descendant through the
      EXISTING unified pause/resume primitive (RFC §3.3) — pause parks the
      descendant's run through the existing pause path; resume releases it
      (optionally carrying a `directive` injected on resume). No new pause
      reason is invented at the coordination layer. **Implementation note
      (§4.3, D-330):** because routing goes through the descendant's inbox (not
      a parent-side query of the descendant's pause token — which would be the
      §13-forbidden second coordination path), the returned bool means "control
      ENQUEUED onto a live descendant" and is `false` only when the descendant
      has already finished (inbox retired). There is no parent-observable
      "no transition" signal: a redundant `_pause_task` on an already-paused
      descendant reports `paused: true` (harmless — the RunLoop parks once), and
      a `_resume_task` on a not-paused descendant reports `resumed: true` but
      ends that descendant's run loud with `ErrNoOutstandingPause` downstream —
      inherited operator-RESUME semantics, stated truthfully in the tool
      description. The finer already-paused / not-paused idempotency is
      delegated downstream to the primitive rather than resolved at the edge.
- [ ] **AC-10** Serialization fails loud (§5, D-025, §11). **Implementation
      note (§4.3, D-330):** the parent dispatch edge honors the fail-loud
      contract on the AGENT-supplied pause/resume payload (`validatePausePayload`
      → `trajectory.ErrUnserializable`, never a silent drop). The
      descendant-run-state serialization contract AC-10 names lives DOWNSTREAM in
      the descendant's own RunLoop (`Coordinator.Request`), enforced unchanged
      there and covered by the pauseresume package's contract tests — it is not
      re-surfaced through the parent verb (the async design cannot deliver an
      end-to-end parent-observable `ErrUnserializable`). The mandatory test is
      scoped honestly to the agent-payload guard.
- [ ] **AC-11** `internal/planner/react/prompt.go`: `renderNativeControlStep`
      gains a `case` per new decision shape with matching replay-arg builders
      (mirroring `cancelTaskReplayArgs`), so a trajectory containing a
      steer/pause/resume step replays as a native `tool_call` + `RoleTool` pair
      on the next prompt build.
- [ ] **AC-12** Human-supremacy + descendant-scope isolation test (mandatory
      per CLAUDE.md §6 / §11): two sibling runs in the SAME
      `(tenant, user, session)` — run A spawns a background descendant; run B
      calls `_steer_task` / `_pause_task` / `_resume_task` naming run A's
      descendant. All three fail loud with `ErrTaskNotOwnDescendant`; run A's
      descendant is untouched. A companion case proves the operator's EXISTING
      control surface CAN pause/steer/resume that same descendant (human
      supremacy), and that run A's own `_pause_task` / `_resume_task` on its own
      descendant succeeds.
- [ ] **AC-13** D-025 concurrent-reuse test: N≥100 concurrent `_steer_task` /
      `_pause_task` / `_resume_task` dispatches against the SAME shared
      `toolExecutor` instance (mirrors 187's
      `TestExecutor_SpawnAwait_ConcurrentReuse` / the AC-16 pattern), under
      `-race`, asserting no data races, no context bleed across runs, no
      cancellation cross-talk (pausing run A's descendant never affects run B),
      no goroutine leak.
- [ ] **AC-14** `docs/skills/drive-the-playground/SKILL.md` §3 (the same skill
      187 updated for `_task_status` / `_cancel_task`) is updated in the same PR
      (§18): the agent can now steer, pause, and resume its own spawned
      background tasks mid-run, and the operator can always override. A grep of
      `docs/skills/` confirms no other `surface: llm|agent-yaml|protocol` skill
      documents `_spawn_task` today, so no second skill needs the update.
- [ ] **AC-15** `docs/decisions.md` gains the pre-assigned D-330 entry: the
      three new sealed `Decision` shapes, why they are NOT a widening of `Batch`
      (structural), the descendant-scope reuse of 187's `isOwnDescendant` guard,
      the routing onto the EXISTING steering inbox + pause/resume primitive
      (no new mechanism, §13/§3.3), and the human-supremacy ordering. If
      `RFC-001-Harbor.md` §6.2's standalone-control sentence enumerates the
      reserved controls, it is extended to name the three new tools in the same
      PR (keeping RFC ↔ projector guard in lockstep, mirroring 187's AC-19).

## Files added or changed

```text
internal/planner/decision.go                    # SteerTask, PauseTask, ResumeTask shapes
internal/planner/react/react.go                 # three reserved-name constants
internal/planner/react/discovered_tools.go      # three reserved declarations + JSON schemas
internal/planner/react/discovered_tools_test.go
internal/planner/react/projector.go             # three translate funcs + standalone-guard extension
internal/planner/react/projector_test.go
internal/planner/react/prompt.go                # renderNativeControlStep cases + replay-arg builders
internal/planner/react/prompt_test.go
internal/runtime/dispatch/dispatch.go           # ExecuteDecision cases; steerTask/pauseTask/resumeTask; reuse isOwnDescendant
internal/runtime/dispatch/dispatch_steer_pause_test.go   # AC-6..10, AC-13
internal/runtime/steering/inbox.go              # (read-only reuse; enqueue path for an agent-issued directive if a thin accessor is needed)
internal/runtime/steering/inbox_test.go
internal/runtime/pauseresume/...                # (reuse of the existing primitive; test-only touches if a descendant-resume path needs coverage)
internal/planner/conformance/...                # decision serialization coverage for the three new shapes
docs/skills/drive-the-playground/SKILL.md
RFC-001-Harbor.md                               # §6.2 standalone-control sentence (if it enumerates controls)
docs/decisions.md                               # D-330
docs/glossary.md                                # new terms
test/integration/                               # AC-12 human-supremacy + cross-run isolation (if not adequately in-package)
scripts/smoke/phase-193.sh
```

## Public API surface

```go
// internal/planner/decision.go
type SteerTask struct {
    TaskID    tasks.TaskID
    Directive string // enqueued onto the descendant's existing per-sub-run steering inbox
}
func (SteerTask) isDecision() {}

type PauseTask struct {
    TaskID tasks.TaskID
    Reason string
}
func (PauseTask) isDecision() {}

type ResumeTask struct {
    TaskID    tasks.TaskID
    Directive string // optional; injected on resume via the unified pause/resume primitive
}
func (ResumeTask) isDecision() {}
```

Reserved tool names (LLM-facing, `internal/planner/react`): `_steer_task`
(args `{task_id, directive}`), `_pause_task` (args `{task_id, reason?}`),
`_resume_task` (args `{task_id, directive?}`).

No new `dispatch` sentinel (reuses 187's `ErrTaskNotOwnDescendant`). No new
pause/resume coordination type (reuses the existing primitive). No new Protocol
method. `ProtocolVersion` unchanged.

## Test plan

- **Unit:** AC-3..AC-5 (declaration presence, schema shape, translation,
  standalone-co-occurrence rejection, malformed/empty args) in
  `internal/planner/react/projector_test.go` + `discovered_tools_test.go`;
  AC-11 replay in `prompt_test.go`; AC-6..AC-10 in
  `internal/runtime/dispatch/dispatch_steer_pause_test.go` (nil-registry
  unsupported, out-of-scope target rejection, idempotent-terminal steer,
  idempotent already-paused/non-paused, and the mandatory `ErrUnserializable`
  loud-fail on an unserializable descendant pause); decision serialization for
  the three new shapes in `internal/planner/conformance`.
- **Integration:** AC-12 (human-supremacy + cross-run isolation under the SAME
  session) — an in-package test in `internal/runtime/dispatch` is the natural
  home (the dispatch package IS the planner→steering/pause wiring boundary, per
  §17.2's in-package carve-out), using the REAL `inprocess` TaskRegistry driver,
  the REAL steering inbox, the REAL pause/resume primitive, real identity: it
  asserts (a) run B's steer/pause/resume of run A's descendant is rejected loud,
  (b) run A's descendant is untouched, (c) the operator's existing control
  surface DOES reach that descendant, and (d) run A's own pause→resume of its
  descendant round-trips. ≥1 failure mode = the `ErrUnserializable` pause.
- **Conformance:** the three new `Decision` shapes join the planner decision
  serialization conformance coverage (round-trip stable).
- **Concurrency / leak:** AC-13, N≥100 concurrent steer/pause/resume dispatches
  against one shared `toolExecutor`, `-race`, asserting no cancellation
  cross-talk between runs and goroutine-baseline restored.

## Smoke script additions

`scripts/smoke/phase-193.sh` (`# PREFLIGHT_REQUIRES: unit-tests`):

- Static greps: `_steer_task` / `_pause_task` / `_resume_task` reserved
  declarations present in `discovered_tools.go`; the standalone guard names all
  three in `projector.go`; the dispatch executor reuses `isOwnDescendant` (the
  guard name appears in the new executor methods, proving the scope check is
  wired, not just described); the steer path references the existing
  `steering` inbox and the pause path the existing pause/resume primitive (no
  new coordination symbol minted).
- `go test ./internal/planner/react/... -run
  'TestProjector.*(Steer|Pause|Resume)Task|TestProjector.*Standalone' -race` —
  translation-table + standalone-rejection coverage.
- `go test ./internal/runtime/dispatch/... -run
  'TestExecutor_(Steer|Pause|Resume)Task.*|TestExecutor.*NotOwnDescendant|TestExecutor.*Unserializable|TestExecutor.*ConcurrentReuse'
  -race` — descendant-scope rejection, the loud pause-serialization fail, and
  concurrent-reuse.
- 404/405/501 → SKIP is N/A (unit-tests class, no live server surface); each
  block SKIPs with a named reason when its target symbol/test name is absent, so
  the script coexists with pre-193 builds.

## Coverage target

- `internal/planner/react` (touched paths): 85%
- `internal/runtime/dispatch` (touched paths): 85%
- `internal/runtime/steering` (touched paths): 85%
- `internal/planner/conformance` (touched — three new shapes): maintained

## Dependencies

- 187

(187 is the sole dependency and is already Shipped: this phase reuses 187's
`isOwnDescendant` guard, `ErrTaskNotOwnDescendant` sentinel, the reserved-control
projector/dispatch seam, and the standalone-guard set. The steering inbox
(Phase 30-band) and the unified pause/resume primitive (RFC §3.3) are long
shipped. No unshipped dependency — this phase parallelises in Stage 1 alongside
192/194.)

## Risks / open questions

- **Resolving a descendant's run handle for steering/pause.** Steering targets a
  per-sub-run inbox and pause targets a run, but the planner names a *task* id.
  The executor must resolve the descendant task's run handle from the registry
  before enqueuing/pausing. 187's dispatch already walks the task→run
  relationship for descendant scoping; verify the run handle is reachable from
  the task record (or via the sub-run registry) without a new engine accessor —
  if a thin accessor is genuinely needed it is additive and named in the plan,
  not smuggled in.
- **Pause/resume of a descendant vs the parent turn.** Pausing a descendant must
  NOT pause the parent run issuing the verb (the parent is mid-turn dispatching
  a non-terminal control). The executor pauses ONLY the resolved descendant's
  run through the primitive; a test asserts the parent run continues. This is
  the pause/resume analogue of 187's "self is not a descendant" rule.
- **Steering-inbox backpressure.** The existing inbox has a bounded channel with
  a drop policy (CLAUDE.md §5 concurrency). An agent-issued directive rides the
  same policy as an operator directive — no new backpressure surface. Confirm the
  drop-oldest + `dropped` event behavior is unchanged for agent-sourced
  directives during implementation.
- **Load-bearing shared code.** The dispatch and steering paths are used by every
  existing steering/pause flow. The change is additive (new executor methods, new
  producers of the existing primitive); the existing operator steering/pause
  regression tests must stay green unmodified.

## Glossary additions

- **Planner-facing task control** (one combined entry covering `_steer_task`,
  `_pause_task`, `_resume_task`) — the agent-side verbs letting a run steer,
  pause, and resume the background tasks it spawned, descendant-scoped, routed
  through the existing steering inbox + unified pause/resume primitive.
- **Human supremacy (control hierarchy)** — the invariant that the operator can
  steer/pause/resume/cancel ANY task while the agent reaches only its own
  descendants; the steer/pause/resume analogue of the cancel hierarchy.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test passes (AC-12)
- [ ] **If this phase builds a reusable artifact:** N/A — this phase does not
      construct a NEW long-lived reusable artifact; it extends the existing
      `toolExecutor` (already concurrent-reuse-covered) with new dispatch
      methods and adds new producers of the existing steering inbox +
      pause/resume primitive. AC-13 extends the existing D-025 coverage rather
      than opening a new surface.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam:** yes — the integration test (AC-12) wires the real
      `inprocess` TaskRegistry, the real steering inbox, and the real
      pause/resume primitive end-to-end, asserts identity propagation +
      human-supremacy ordering, and covers a failure mode
      (`ErrUnserializable`), under `-race`.
- [ ] Pause/resume serialization test passes (AC-10 — `ErrUnserializable`
      raised loudly, no silent nil)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry filed (N/A — no departure; D-330 filed as the sanctioned extension
      of D-324's control taxonomy)
