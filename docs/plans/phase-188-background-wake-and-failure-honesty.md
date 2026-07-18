# Phase 188 — Background wake notifications + turn-failure honesty on the conversation surfaces

## Summary

Group resolution and background-task completion already reach the planner as
typed data (`GroupCompletion`/`MemberOutcome` via `WatchGroup`), but nothing
tells the *operator* — conversationally, on the surfaces they're actually
looking at — that background work finished, and a foreground turn that FAILS
goes idle with zero on-chat indication. This phase closes both honesty gaps by
extending the already-shipped `internal/runtime/notifications` topic with two
new classes (`notification.task_group_resolved`, `notification.task_completed`)
mirroring group/background resolution, and by rendering a dedicated
turn-failure line on the TUI's conversation surface. The typed `MemberOutcome`
path to the planner is unchanged — this is a conversational mirror layered on
top, per brief 16 §2c/§5's "wake-with-a-message" pattern grounded against
Harbor's actual substrate.

## RFC anchor

- RFC §6.13
- RFC §6.8
- RFC §5.2
- RFC §7

## Briefs informing this phase

- brief 16
- brief 06
- brief 05
- brief 11

## Brief findings incorporated

- brief 16 §2c: "Only opencode has a true non-blocking mode... and later
  wakes the parent by INJECTING A MESSAGE into the parent session's stream
  (`task.ts:202-240`) — a side effect, not a typed completion... Do not
  regress toward the weaker fused model; DO adopt opencode's insight that a
  conversational wake signal composes well on top of the typed one." This
  phase adopts the insight (a conversational mirror) and explicitly rejects
  the mechanism (no message injection into the planner's context — the
  mirror rides the existing `notification.*` event topic, never the
  planner-visible trajectory).
- brief 16 §5: "Wake-with-a-message (from opencode, adapted to Harbor's
  substrate): group resolution keeps the typed `MemberOutcome` path for the
  planner AND emits a notification-class event (the existing
  `internal/runtime/notifications` subsystem — `notification.task_failed` et
  al.) that the TUI/Console render conversationally. The model gets
  structure; the operator gets narrative." This is this phase's design in
  one sentence — extend the existing subsystem, don't build a parallel one.
- brief 06 §5 "Sharp edges to design out": "Visualization coupled to private
  state. Visualization that reaches into private runtime fields breaks on
  every refactor. Harbor's visualization derives from the canonical
  event/topology surface published over the protocol — no private fields."
  The new notification classes ride the same canonical `events.subscribe`
  surface every other class already uses; no new Protocol method, no
  TUI-private hook into the task registry.
- brief 05 §5 "Sharp edges": "A task interface that mixes orchestration and
  group governance... Harbor keeps the surface but groups it into named
  method sets." `MemberOutcome`'s additive `Description` field is populated
  at the exact call site (`collectMemberOutcomesLocked`) that already reads
  the member `Task` record for `Status` — no new registry method, no new
  cross-package read.
- brief 11 §"Notification center": "Each notification is a Protocol-emitted
  event of type `notification.*` carrying severity, identity scope, summary,
  deep-link." The two new classes conform to the existing shape exactly;
  this phase does not touch the Console's notification center (out of
  scope — see Non-goals).

## Findings I'm departing from (if any)

- brief 16 §2c's mined precedent (opencode) wakes the parent by injecting a
  synthetic message into the SAME session/context stream the model reads.
  Harbor deliberately does NOT do this: the notification mirror rides the
  `notification.*` bus topic (operator-facing), never the planner's
  trajectory or a synthesized `RoleUser`/`RoleTool` turn. brief 16 §5 itself
  flags this as the adaptation Harbor should make ("adapted to Harbor's
  substrate"), so this is not a silent departure — it is the brief's own
  recommendation, restated as a binding constraint here so a future
  implementor does not "helpfully" wire the notification into `RunContext`.
- D-325's context paragraph cites `task.failed` as the sole illustrative
  existing trigger for the "background terminal transition" half. This plan
  ALSO gates `task.completed` behind the pre-existing `NotifyOnComplete`
  flag as a NEW trigger (§"Runtime" below) — `notification.task_failed`
  already fires unconditionally today (any task, foreground or background),
  so the only true wake-message gap for background work is the SUCCESS
  path, which had no notification class at all before this phase. This is
  an extension of D-325's scope, not a contradiction: the group-resolution
  and background-success gaps are the two concrete instances the decision's
  own title names ("background-task resolution wakes the conversation").
- `task.group_cancelled` (the `GroupCompletion` cancel-path sibling of
  `task.group_resolved`) is deliberately NOT given a notification mirror in
  this phase, even though it shares the same `GroupCompletion` payload
  shape. D-325 scopes the wake to "GroupCompletion... on group resolution"
  (the success path); a cancelled-group mirror is a reasonable follow-up but
  is left out here to keep the phase's blast radius matched to what D-325
  actually authorizes. Filed as a non-goal, not silently dropped.

## Goals

- Close the "silent background success" gap: a solo background task
  (`NotifyOnComplete=true`, no group) that completes successfully produces an
  operator-visible conversational line, not just a typed `MemberOutcome` the
  planner already had.
- Close the "silent group resolution" gap: a resolved `TaskGroup` produces
  ONE muted, human-readable rollup line on the conversation surface — never a
  per-member flood, never raw Runtime internals leaking into chat.
- Close the "silent foreground failure" gap: a FAILED foreground turn renders
  an explicit, bounded failure line (error code, never raw error text) on the
  TUI's conversation surface instead of the composer quietly returning to
  idle with no signal.
- Keep the planner-facing `WatchGroup`/`GroupCompletion` typed path completely
  unchanged — this phase adds a mirror, not a new decision point.
- Extend the Console's session/run views (Sessions `BottomDockTabs`, Tasks
  `TaskBottomDock`) to render the same notification family the TUI renders,
  so an operator watching either surface sees the same narrative.

## Non-goals

- No change to `WatchGroup`, `GroupCompletion`, or any planner-visible
  decision/prompt surface. The model's view of task management is exactly
  what phases 185–187 shipped.
- No `task.group_cancelled` notification mirror (see "Findings I'm departing
  from").
- No Console notification center, toast system, or bell-icon badge (brief
  11's "Notification center" UI is a separate, larger, not-yet-planned
  surface). This phase renders the family only on the session/run views
  already listed in Goals.
- No change to notification routing (email/Slack/web-push) or the
  `notifications_routing` Console DB table (Phase 72h territory, unaffected).
- No severity-escalation policy, snooze/dismiss/mute actions, or anomaly
  detection — all still explicitly out of scope per the notifications
  package's own "what is out of scope" contract.
- No retroactive backfill: a group that resolved or a background task that
  completed before this phase's Subscriber wiring landed does not get a
  synthesized notification after the fact.

## Acceptance criteria

- [ ] `internal/runtime/notifications` gains two new V1 classes —
      `notification.task_group_resolved` (from `task.group_resolved`,
      `tasks.TaskGroupResolvedPayload`) and `notification.task_completed`
      (from `task.completed`, gated on the payload's `NotifyOnComplete`) —
      registered in `V1NotificationClasses()`/`V1TriggerEventTypes()`,
      each with its own `mapXxx` function following the `mapTaskFailed`
      pattern (typed payload assertion, `ErrUnmappable` on mismatch, no I/O).
- [ ] `NotificationPayload` gains additive fields (`TaskID`, `GroupID`,
      `Members []MemberOutcomeSummary`, `MembersTruncated`,
      `MemberSucceeded`/`MemberFailed`/`MemberCancelled`) so a single shared
      payload type still serves every class; `Members` is capped (bounded
      length, `MembersTruncated=true` when the cap is hit — never a silent
      drop) and carries only `TaskID`/`Status`/`Description` per member
      (ref-shaped — `MemberOutcome.Result`/`Error` bytes never cross onto
      the notification payload).
- [ ] `tasks.MemberOutcome` gains an additive `Description string` field,
      populated at `collectMemberOutcomesLocked` (`internal/tasks/engine/groups.go`)
      from the member `Task.Description` already read at that call site (no
      new cross-package read); `tasks.TaskCompletedPayload` gains an
      additive `NotifyOnComplete bool`, populated from the `Task` record at
      `MarkComplete`.
- [ ] `make protocol-docs-gen` regenerates `docs/site/protocol/events.md` /
      `types.md` for the two new event types and the changed payload shapes;
      the generator's lockstep test fails on a missed regen.
- [ ] `internal/tui/projection`: a new `"notification"` `Block.Kind` renders
      `notification.task_completed` and `notification.task_group_resolved`
      unconditionally, and `notification.task_failed` ONLY when its `TaskID`
      does not match a foreground turn already tracked on the projection
      (no `"user:"+TaskID` block) — never duplicating the dedicated
      turn-failure line below. `ClassifyEventType` marks all three as
      `EventTyped` (never the generic event fallback).
- [ ] `internal/tui/projection.Block` gains an additive `ErrorCode` field,
      populated from `task.failed`'s existing `ErrorCode` payload field (no
      wire change — the TUI simply starts reading a field it already
      received and discarded).
- [ ] `internal/tui/app`: `conversational()` includes `"notification"`;
      `projectionActive()` treats it like `"event"`/`"user"` (never
      in-flight work); the transcript renders it as one muted lifecycle
      line (glyph + bounded text, reusing `lifecycleGlyph`'s vocabulary) —
      never a card, never per-member fan-out.
- [ ] A FAILED foreground turn (the newest terminal `"task"` block whose
      `RunID` has a matching `"user:"+RunID` block, `Status=="failed"`)
      renders `"×  Turn failed · <ErrorCode>"` on the status-strip chrome
      (`statusStrip()`), distinct from and never simultaneous with the muted
      background-notification line; the line clears the moment a new turn
      is submitted. Full detail (message, trajectory) stays reachable via
      the existing Tasks/diagnostics route (Phase 183) — the strip shows
      only the bounded error code.
- [ ] TUI golden matrix (`docs/design/tui/CONVENTIONS.md` §10) gains
      fixtures for: a background-completion line, a group-resolution rollup
      line, and the turn-failure status-strip line (present, then cleared on
      next submit) — ANSI-stripped geometry plus a style assertion.
- [ ] `web/console/src/lib/events/taxonomy.ts` registers `'notification'` as
      an `EventCategory` and the three consumed classes
      (`notification.task_failed`, `notification.task_completed`,
      `notification.task_group_resolved`) in `EVENT_TYPES`.
- [ ] `web/console/src/lib/tasks/run-events.ts` gains `filterNotificationEvents`;
      `web/console/src/lib/tasks/run-stream.svelte.ts`'s `RUN_DOCK_TYPES`
      gains the `notification.` prefix and a `notificationEvents` getter.
- [ ] Both `web/console/src/lib/components/sessions/BottomDockTabs.svelte`
      (session view) and `web/console/src/lib/components/tasks/TaskBottomDock.svelte`
      (run view) subscribe to `notification.*` (their `DOCK_TYPES`/dock
      subscription gains the prefix) and render the notification's `Summary`
      text (falling back to the bare event type) on their existing Events
      row — no new tab, no notification-center UI.
- [ ] `docs/skills/drive-the-harbor-tui/SKILL.md` and
      `docs/skills/observe-with-the-console/SKILL.md` document the new
      conversational wake lines and the turn-failure status-strip line in
      the same PR (§18).

## Files added or changed

- `internal/runtime/notifications/notifications.go` (two new event-type
  constants + V1 class/trigger registration)
- `internal/runtime/notifications/payloads.go` (`NotificationPayload`
  additive fields, new `MemberOutcomeSummary`)
- `internal/runtime/notifications/mapper.go` (`mapTaskGroupResolved`,
  `mapTaskCompleted`, `mapTaskFailed` doc update for the new `TaskID` field)
- `internal/runtime/notifications/mapper_test.go`, `subscriber_test.go`
- `internal/tasks/events.go` (`TaskCompletedPayload.NotifyOnComplete`)
- `internal/tasks/groups.go` (`MemberOutcome.Description`)
- `internal/tasks/engine/groups.go` (`collectMemberOutcomesLocked` populates
  `Description`; `MarkComplete` populates `NotifyOnComplete`)
- `internal/tui/projection/projection.go` (new `Block.ErrorCode`, new
  `"notification"` Kind, three new reducer cases, `ClassifyEventType`
  entries, foreground-dedup logic)
- `internal/tui/projection/projection_test.go`
- `internal/tui/app/live.go` (`conversational()`, `projectionActive()`,
  turn-failure state derivation, new `State` fields)
- `internal/tui/app/model.go` (`statusStrip()` new case, `State.TurnFailed`
  / `State.TurnFailedCode`)
- `internal/tui/app/transcript_render.go` (`layoutNotification`)
- `internal/tui/app/conversation_surface_test.go`,
  `transcript_render_test.go`, golden fixtures under `internal/tui/app/testdata/`
- `docs/design/tui/CONVENTIONS.md` (§10 matrix additions — new fixture rows
  only, no convention change)
- `test/integration/notifications_topic_test.go` (extended, not replaced)
- `web/console/src/lib/events/taxonomy.ts`
- `web/console/src/lib/tasks/run-events.ts`, `run-events.test.ts`
- `web/console/src/lib/tasks/run-stream.svelte.ts`
- `web/console/src/lib/components/sessions/BottomDockTabs.svelte`
- `web/console/src/lib/components/tasks/TaskBottomDock.svelte`
- `docs/site/protocol/events.md`, `docs/site/protocol/types.md` (regenerated)
- `docs/skills/drive-the-harbor-tui/SKILL.md`,
  `docs/skills/observe-with-the-console/SKILL.md`
- `docs/glossary.md`
- `scripts/smoke/phase-188.sh`

## Public API surface

```go
package notifications

const (
    EventTypeNotificationTaskGroupResolved events.EventType = "notification.task_group_resolved"
    EventTypeNotificationTaskCompleted     events.EventType = "notification.task_completed"
)

// NotificationPayload additive fields (existing fields unchanged):
type NotificationPayload struct {
    events.Sealed
    Class, Severity, Summary, DeepLink string
    OriginEventType                    events.EventType
    OriginEventSequence                uint64

    // New, all additive and omitted when the class doesn't populate them.
    TaskID          string                 // single-task classes
    GroupID         string                 // group-resolution class
    Members         []MemberOutcomeSummary // ref-shaped, bounded
    MembersTruncated bool
    MemberSucceeded, MemberFailed, MemberCancelled int
}

type MemberOutcomeSummary struct {
    TaskID      string
    Status      string
    Description string // bounded, echoes Task.Description
}
```

```go
package tasks

type MemberOutcome struct {
    TaskID      TaskID
    Status      TaskStatus
    Result      *TaskResult
    Error       *TaskError
    Description string // NEW, additive
}

type TaskCompletedPayload struct {
    events.SafeSealed
    TaskID           TaskID
    NotifyOnComplete bool // NEW, additive
}
```

```go
package projection

type Block struct {
    // ... existing fields unchanged ...
    ErrorCode string `json:"error_code,omitempty"` // NEW, from task.failed
}
```

No new Protocol method. No `ProtocolVersion` bump (additive event-payload
fields + two new per-class topics, same posture as Phase 72d).

## Test plan

- **Unit:**
  - `internal/runtime/notifications`: `mapTaskGroupResolved` (Completed
    members / mixed success-failure severity / `ErrUnmappable` on wrong
    payload type / `Members` cap + `MembersTruncated`), `mapTaskCompleted`
    (`NotifyOnComplete=false` → `(nil, nil)`; `=true` → synthesized event),
    concurrent-reuse N=100 under `-race` (trivially satisfied — `Map` stays
    pure — but the mandatory test still runs per the package's existing
    convention).
  - `internal/tasks`: `collectMemberOutcomesLocked` populates `Description`;
    `MarkComplete` populates `NotifyOnComplete` from the `Task` record.
  - `internal/tui/projection`: the three new reducer cases (typed decode +
    generic-fallback guard on malformed payload); the foreground-dedup rule
    (`notification.task_failed` suppressed when a `"user:"+TaskID` block
    exists, rendered when it does not); `ErrorCode` threading from
    `task.failed`; `ClassifyEventType` exhaustiveness.
  - `internal/tui/app`: `conversational("notification")==true`;
    `projectionActive()` unaffected by a notification block;
    `statusStrip()`'s new case takes precedence correctly and clears on a
    fresh `"user:"` block; golden fixtures for the muted line and the
    turn-failure line.
  - `web/console`: `filterNotificationEvents` (vitest, mirrors
    `run-events.test.ts`'s captured-frame style); taxonomy registration.
- **Integration:**
  - Extends `test/integration/notifications_topic_test.go`
    (`TestE2E_NotificationsTopic`): real bus, real `Subscriber`, a
    synthetic `task.group_resolved` with mixed member outcomes round-trips
    to `notification.task_group_resolved` with correct counts; a synthetic
    `task.completed` with `NotifyOnComplete=true` round-trips to
    `notification.task_completed`; a synthetic `task.completed` with
    `NotifyOnComplete=false` produces no notification event (asserted via a
    bounded-wait negative check, not a sleep); identity propagation
    end-to-end; N≥20 concurrency stress per §17.3 (extends the existing
    stress, does not duplicate it).
  - A new in-package adapter test in `internal/tui/projection` hydrating a
    `SnapshotBundle` + live events through the Reducer end-to-end: a
    background task's `notification.task_completed` renders conversationally
    while its `task.completed` sibling stays off-chat; a foreground turn's
    `task.failed` suppresses the notification mirror and drives the
    turn-failure status-strip state instead.
- **Conformance:** N/A — no new persistence driver.
- **Concurrency / leak:** the extended `TestE2E_NotificationsTopic` N≥20
  stress; `Map`'s existing N=100 concurrent-reuse test extended to the two
  new classes; no new long-lived goroutine is introduced (both new classes
  ride the existing `Subscriber`).

## Smoke script additions

`scripts/smoke/phase-188.sh`:

- Unit-tests class (per `AGENTS.md` §4.2 `PREFLIGHT_REQUIRES: unit-tests`):
  runs the new `internal/runtime/notifications` mapper tests, the
  `internal/tui/projection` reducer tests, and the extended
  `TestE2E_NotificationsTopic` integration test, mirroring Phase 72d's
  smoke shape (`ok`/`fail` per suite, no live-server dependency for these).
- A live-server events assertion (the new event TYPE half of the
  instruction): boots against the preflight dev server, drives a background
  spawn + group resolution through the shipped Phase 185–187 surfaces (or,
  if those surfaces are not yet reachable via a scripted flow, a direct
  `events.subscribe` filter probe asserting the two new event-type
  constants are accepted by the filter — never a 400/`unknown event type`),
  and asserts at least one `notification.task_group_resolved` or
  `notification.task_completed` frame is observable on the stream within a
  bounded wait.
- `grep`-based static assertions (matching the Phase 184/72d house style)
  that `conversational()` includes `"notification"` and that
  `statusStrip()` carries the turn-failure case, so a regression that
  silently reverts the honesty gate fails preflight immediately even before
  the golden suite runs.

## Coverage target

- `internal/runtime/notifications`: 85%
- `internal/tui/projection`: 85%
- touched `internal/tui/app` (conversational/status-strip/render paths): 85%
- `internal/tasks` touched functions (`collectMemberOutcomesLocked`,
  `MarkComplete`): 85% (matches the package's existing target)
- `web/console/src/lib/tasks/` (touched): 85%
- `web/console/src/lib/events/taxonomy.ts`: existing target maintained

## Dependencies

- 186

(187 is deliberately NOT a dependency: the notification mirror and the
turn-failure line are orthogonal to the task-management meta-tools, so the
wave stages 187 and 188 in parallel — see
`docs/plans/wave-v116-parallel-intent-coordination.md` §3. The suppression
rule for `notification.task_failed` keys on the tracked foreground turn,
not on any meta-tool surface.)

## Risks / open questions

- **Cross-wave field coordination.** This phase is drafted in parallel with
  Phases 185–187 (the `Batch` decision, its execution, and the
  task-management meta-tools) inside the same v1.16 wave (§17.7 staged
  dispatch). `MemberOutcome.Description` is a small additive field this
  phase owns; if Phase 186 lands first and already populates a similar
  field under a different name, reconcile onto ONE field in the merge —
  additive fields don't conflict at the type-system level, but a duplicate
  concept would. Verify against Phase 186's landed shape before implementing.
- **Foreground/background dedup correctness.** The "does a `"user:"+TaskID`
  block exist" check is a heuristic over the projection's own accumulated
  state, not an explicit `Task.Kind=="foreground"` signal on the wire (the
  event payloads carry `TaskID`, not `Kind`). It is correct because only a
  composer-submitted turn ever creates a `"user:"` block, but a future
  session-hydration path that omits `"user:"` blocks (e.g. a truncated
  history window) could misclassify a foreground failure as background.
  Mitigated by history-truncation already being an honestly-marked partial
  state (`HistoryTruncated`) the operator sees independently.
- **`Members` cap value.** The exact cap (proposed: 20) is a judgment call
  balancing "the operator can see what happened" against payload size /
  audit-redactor walk cost on large batches. Confirm against
  `planner.max_batch_spawns` (Phase 186/D-323) once that config key's
  default is known — the cap should comfortably cover the common case
  without being so large it defeats the "one muted line" design goal.

## Glossary additions

- **Notification wake events** (one combined glossary entry covering
  `notification.task_group_resolved` and `notification.task_completed`) —
  added.
- **Turn-failure status-strip line** — added.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (extended `TestE2E_NotificationsTopic` covers identity propagation for
      the two new classes; no new identity-scoped storage method added)
- [ ] **If this phase builds a reusable artifact:** N/A — `Map` remains the
      existing pure-function reusable artifact from Phase 72d; no new
      compiled artifact is introduced. The existing N=100 concurrent-reuse
      test is extended to the two new classes, not newly created.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam:** yes — an integration test extends
      `test/integration/notifications_topic_test.go` with real bus, real
      `Subscriber`, identity propagation, and a failure mode
      (`NotifyOnComplete=false` producing no notification), under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry filed (D-325, pre-assigned)
