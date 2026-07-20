# Phase 192 — `task.group_cancelled` conversational mirror

## Summary

Phase 188 (D-325) shipped the background-wake notification family —
`notification.task_group_resolved` / `notification.task_completed` mirror a
group's *successful* resolution onto the conversation surface while the typed
`WatchGroup` planner path stays untouched. But a **cascade- or
fail-fast-cancelled** batch-spawned group is silent while its *successful*
siblings wake: `spawnOne` marks every batch spawn `NotifyOnComplete=true`, so
an operator watching the conversation sees the winners announce themselves and
the cancelled losers vanish without a word. This phase closes that asymmetry by
adding a `notification.task_group_cancelled` conversational-mirror class,
reusing 188's member-outcome summarisation, and settles the suppression rule
(mirror an *unprompted* cascade/fail-fast cancel; suppress a
*directly-operator-initiated* cancel the operator already knows about —
analogous to 188's foreground-failure suppression).

## RFC anchor

- RFC §6.13
- RFC §6.8
- RFC §5.2
- RFC §7

## Briefs informing this phase

- brief 16
- brief 06

## Brief findings incorporated

- brief 16 §5 "Wake-with-a-message (from opencode, adapted to Harbor's
  substrate): group resolution keeps the typed `MemberOutcome` path for the
  planner AND emits a notification-class event (the existing
  `internal/runtime/notifications` subsystem) that the TUI/Console render
  conversationally. The model gets structure; the operator gets narrative."
  This phase extends the SAME subsystem 188 extended — it does not build a
  parallel one — adding one class for the cancel path the resolution path
  already established.
- brief 16 §3 "the cancel hierarchy" (human > agent > cascade defaults):
  a cascade/fail-fast cancel is a runtime-initiated, *unprompted* transition
  from the operator's point of view — exactly the "narrative the operator
  didn't ask for" case a conversational wake exists to surface; a
  directly-operator-initiated cancel is the operator's OWN action and is not
  news. This is the settled suppression rule (D-329), mirroring how 188
  suppresses `notification.task_failed` when the failing task IS the tracked
  foreground turn.
- brief 06 §5 "Sharp edges to design out": "Visualization coupled to private
  state ... breaks on every refactor. Harbor's visualization derives from the
  canonical event/topology surface ... no private fields." The new class rides
  the same canonical `events.subscribe` surface every other notification class
  already uses — no new Protocol method, no TUI-private hook into the task
  registry.
- brief 16 §5 (member-outcome shape): the cancelled-group summary reports the
  SAME `MemberOutcomeSummary` shape (`TaskID`/`Status`/`Description`, capped at
  `MaxMemberSummaries`, `MembersTruncated` on overflow, true totals) 188's
  resolved-group summary reports — ref-shaped, member `Result`/`Error` bytes
  never cross onto the notification payload.

## Findings I'm departing from (if any)

- Phase 188's plan filed `task.group_cancelled` as an explicit non-goal
  ("D-325 scopes the wake to the success path; a cancelled-group mirror is a
  reasonable follow-up ... left out here to keep the phase's blast radius
  matched to what D-325 actually authorizes. Filed as a non-goal, not silently
  dropped."). This phase is that authorized follow-up (GitHub issue #532, v1.16
  checkpoint audit WARN W2). It does NOT contradict D-325 — it extends the same
  design to the sibling transition D-325 deliberately parked. No brief finding
  is departed from.
- brief 16's mined precedent (opencode) wakes the parent by INJECTING a
  synthetic message into the model's context stream. Harbor deliberately does
  NOT do this — the cancel mirror rides the `notification.*` bus topic
  (operator-facing), never the planner's trajectory or a synthesized turn.
  This is 188's binding constraint restated here so a future implementor does
  not wire the cancel notification into `RunContext`.

## Goals

- Close the "silent cancelled group" asymmetry: a batch-spawned group cancelled
  by fail-fast or by an ancestor cascade produces ONE muted, human-readable
  rollup line on the conversation surface with member outcomes — never a
  per-member flood, never raw Runtime internals leaking into chat — matching
  the shape a *resolved* group already produces via 188.
- Settle and encode the suppression rule (D-329): mirror a cascade/fail-fast
  cancel (unprompted); suppress a directly-operator-initiated cancel of that
  same group (the operator already knows). The rule keys on the cancel's
  *origin*, carried as a typed field on the trigger payload — never on
  guesswork over projection state.
- Keep the planner-facing `WatchGroup` / `GroupCompletion` typed path and the
  cancel hierarchy (187/D-324) completely unchanged — this phase adds a mirror,
  not a new decision point or a new cancel mechanism.
- Render the new class on the SAME surfaces 188 wired: the TUI's muted
  `notification` block kind, and the Console Sessions (`BottomDockTabs`) + Tasks
  (`TaskBottomDock`) docks.

## Non-goals

- No change to `WatchGroup`, `GroupCompletion`, the cancel hierarchy
  (187/D-324), or any planner-visible decision/prompt surface. The model's view
  of task management is exactly what 185–187 shipped.
- No `notification.task_cancelled` for a SOLO (non-group) cancelled task in
  this phase. The named gap in #532 is the *group* asymmetry (successful
  siblings wake, cancelled siblings are silent); a solo-task cancel mirror has
  no sibling-asymmetry driver and is left as a possible additive follow-up, not
  silently folded in. (The `task.group_cancelled` trigger is the load-bearing
  one; a solo `task.cancelled` class would be a second additive class with its
  own suppression question — out of scope, filed here.)
- No Console notification center, toast, or bell-icon badge (188's non-goal,
  unchanged). This phase renders the family only on the session/run docks 188
  already touches.
- No `ProtocolVersion` bump — additive event class + additive payload fields
  only, same posture as 188.
- No retroactive backfill: a group cancelled before this phase's Subscriber
  wiring landed does not get a synthesized notification after the fact.

## Acceptance criteria

- [ ] **AC-1** `internal/runtime/notifications` gains one new V1 class —
      `notification.task_group_cancelled` (from `task.group_cancelled`,
      `tasks.TaskGroupCancelledPayload`) — registered in
      `V1NotificationClasses()` / `V1TriggerEventTypes()`, with its own
      `mapTaskGroupCancelled` function following the `mapTaskGroupResolved`
      pattern exactly (typed payload assertion, `ErrUnmappable` on mismatch,
      no I/O, pure).
- [ ] **AC-2** `mapTaskGroupCancelled` reuses 188's member-outcome
      summarisation: `Members []MemberOutcomeSummary` capped at
      `MaxMemberSummaries` (the constant 188 introduced), `MembersTruncated`
      set on overflow (never a silent drop), true `MemberSucceeded` /
      `MemberFailed` / `MemberCancelled` totals over the full member set, and
      each summary carrying only `TaskID`/`Status`/`Description` (ref-shaped —
      no `Result`/`Error` bytes). `NotificationPayload` needs NO new fields
      (188's additive `TaskID`/`GroupID`/`Members`/… set already serves this
      class); if a `CancelReason` / `Origin` display string is surfaced it is
      an additive field, documented, `omitempty`.
- [ ] **AC-3** Suppression rule (D-329): `task.group_cancelled` carries a typed
      `Origin` (e.g. `tasks.CancelOriginOperator` / `CancelOriginCascade` /
      `CancelOriginFailFast`) on `TaskGroupCancelledPayload`, populated at the
      group-cancel call site in `internal/tasks/engine` from the cancel's own
      provenance (never inferred downstream). `mapTaskGroupCancelled` returns
      `(nil, nil)` — no notification synthesized — when
      `Origin == CancelOriginOperator` (the operator already knows); it
      synthesizes the notification for `CancelOriginCascade` /
      `CancelOriginFailFast`. The rule is unit-tested per origin value; a
      missing/unknown origin fails LOUD (defaults to synthesize, never a silent
      swallow — an unclassified cancel is surfaced, not hidden).
- [ ] **AC-4** `tasks.TaskGroupCancelledPayload` is the additive trigger
      payload (mirroring `TaskGroupResolvedPayload`): `GroupID`, the member
      outcome slice (reusing the same `MemberOutcome`+`Description` shape 188
      populates at `collectMemberOutcomesLocked`), and the `Origin` field
      (AC-3). It is emitted at the existing group-cancel site
      (`internal/tasks/engine/groups.go`'s `cancelTaskLocked` / the group
      cascade path) — same call site that already knows the members and the
      cancel reason; no new registry method, no new cross-package read.
- [ ] **AC-5** `make protocol-docs-gen` regenerates `docs/site/protocol/events.md`
      / `types.md` for the new event type and the changed payload shapes; the
      generator's lockstep test fails on a missed regen (D-209).
- [ ] **AC-6** Full D-223 lockstep for the new canonical event class and any
      additive wire field: `protocol.ts` / per-page wire module mirrored,
      `make protocol-ts-gen` manifest regenerated, `ProtocolVersion` stays
      `0.1.0`.
- [ ] **AC-7** `internal/tui/projection`: the new `notification.task_group_cancelled`
      renders as the existing muted `"notification"` `Block.Kind` (188's block
      kind, unchanged — this class joins the family, adds no new kind).
      `ClassifyEventType` marks it `EventTyped` (never the generic fallback).
      No foreground-dedup applies (a group cancel is never a foreground turn),
      but the projection reuses 188's reducer case shape.
- [ ] **AC-8** `internal/tui/app`: `conversational()` already includes
      `"notification"` (188) — this class needs no `conversational()` change;
      the transcript renders it as one muted lifecycle line (glyph + bounded
      rollup text, reusing `lifecycleGlyph`'s vocabulary) — never a card, never
      per-member fan-out. A golden fixture (`docs/design/tui/CONVENTIONS.md`
      §10 matrix) covers the cancelled-group rollup line (ANSI-stripped
      geometry + style assertion) and a suppressed operator-initiated cancel
      producing NO line.
- [ ] **AC-9** `web/console/src/lib/events/taxonomy.ts` registers
      `notification.task_group_cancelled` in `EVENT_TYPES` under the existing
      `'notification'` `EventCategory` (188 added the category).
      `web/console/src/lib/tasks/run-events.ts`'s `filterNotificationEvents`
      and `run-stream.svelte.ts`'s `notificationEvents` getter already cover
      the `notification.` prefix (188) — this class flows through them with no
      new filter, verified by a vitest case.
- [ ] **AC-10** Both `BottomDockTabs.svelte` (session view) and
      `TaskBottomDock.svelte` (run view) render the new class's `Summary` text
      (falling back to the bare event type) on their existing Events row — no
      new tab, no notification-center UI (188's surface, this class rides it).
- [ ] **AC-11** `test/integration/notifications_topic_test.go` is extended (not
      replaced): a synthetic `task.group_cancelled` with `Origin=Cascade` and
      mixed member outcomes round-trips over the real bus + real `Subscriber`
      to `notification.task_group_cancelled` with correct counts and identity
      propagation; a synthetic `task.group_cancelled` with `Origin=Operator`
      produces NO notification event (bounded-wait negative check, not a
      sleep); N≥20 concurrency stress per §17.3 (extends 188's stress, does not
      duplicate it).
- [ ] **AC-12** `docs/skills/use-the-harbor-protocol/SKILL.md` documents the
      new `notification.task_group_cancelled` class + its suppression rule in
      the same PR (§18); `docs/skills/drive-the-harbor-tui/SKILL.md` and
      `docs/skills/observe-with-the-console/SKILL.md` (188's touched skills)
      note the new conversational cancel line. `docs/site/` VitePress stubs +
      nav updated if the protocol reference gains a rendered surface (§18).
- [ ] **AC-13** `docs/decisions.md` gains the pre-assigned D-329 entry: the new
      class, the suppression rule (origin-keyed, unprompted-vs-known), why it
      extends D-325 rather than re-litigating it, and the additive-wire /
      no-version-bump posture.

## Files added or changed

```text
internal/runtime/notifications/notifications.go   # EventTypeNotificationTaskGroupCancelled + V1 class/trigger registration
internal/runtime/notifications/mapper.go          # mapTaskGroupCancelled (origin-gated)
internal/runtime/notifications/payloads.go        # additive Origin/CancelReason display fields (if surfaced)
internal/runtime/notifications/mapper_test.go     # per-origin synthesize/suppress cases
internal/runtime/notifications/subscriber_test.go
internal/tasks/events.go                          # TaskGroupCancelledPayload (+ CancelOrigin enum)
internal/tasks/engine/groups.go                   # emit TaskGroupCancelledPayload at the group-cancel site with Origin
internal/tasks/engine/groups_test.go
internal/tui/projection/projection.go             # reducer case + ClassifyEventType entry (reuses "notification" kind)
internal/tui/projection/projection_test.go
internal/tui/app/transcript_render.go             # cancelled-group rollup line (reuses layoutNotification)
internal/tui/app/transcript_render_test.go
internal/tui/app/testdata/                        # golden fixtures (rollup line; suppressed operator cancel = no line)
docs/design/tui/CONVENTIONS.md                    # §10 matrix rows (new fixtures only)
internal/protocol/singlesource/singlesource.go     # register the canonical event type/class (where TestCanonicalWireTypes gates) + regenerated wire manifest + mirrored TS (D-223/D-209 lockstep)
web/console/src/lib/events/taxonomy.ts
web/console/src/lib/tasks/run-events.test.ts
web/console/src/lib/components/sessions/BottomDockTabs.svelte
web/console/src/lib/components/tasks/TaskBottomDock.svelte
docs/site/protocol/events.md, types.md            # regenerated (make protocol-docs-gen)
docs/skills/use-the-harbor-protocol/SKILL.md
docs/skills/drive-the-harbor-tui/SKILL.md
docs/skills/observe-with-the-console/SKILL.md
docs/site/ (VitePress stubs + nav if a rendered surface changes)
test/integration/notifications_topic_test.go      # extended (Origin=Cascade round-trip + Origin=Operator negative)
docs/decisions.md                                 # D-329
docs/glossary.md                                  # new terms
scripts/smoke/phase-192.sh
```

## Public API surface

```go
package notifications

const EventTypeNotificationTaskGroupCancelled events.EventType = "notification.task_group_cancelled"

// NotificationPayload: no new required fields — 188's additive
// TaskID/GroupID/Members/MembersTruncated/Member{Succeeded,Failed,Cancelled}
// set already serves this class. An additive, omitempty CancelReason display
// string MAY be added if the rollup line surfaces it.
```

```go
package tasks

// CancelOrigin classifies WHY a group/task was cancelled, so the notification
// mapper can suppress operator-initiated cancels (the operator already knows)
// and mirror the unprompted ones. Populated from the cancel's own provenance
// at the engine call site, never inferred downstream.
type CancelOrigin string

const (
    CancelOriginOperator CancelOrigin = "operator" // suppressed from the conversational mirror
    CancelOriginCascade  CancelOrigin = "cascade"  // mirrored (unprompted)
    CancelOriginFailFast CancelOrigin = "failfast" // mirrored (unprompted)
)

type TaskGroupCancelledPayload struct {
    events.SafeSealed
    GroupID string
    Members []MemberOutcome // reuses 188's MemberOutcome (incl. additive Description)
    Origin  CancelOrigin
}
```

No new Protocol method. No `ProtocolVersion` bump (additive event class +
additive payload fields, same posture as Phase 188).

## Test plan

- **Unit:**
  - `internal/runtime/notifications`: `mapTaskGroupCancelled` — cascade origin
    synthesizes with correct member counts + `MembersTruncated` at the cap;
    fail-fast origin synthesizes; operator origin returns `(nil, nil)`;
    unknown/empty origin synthesizes (fail-loud-not-swallow); `ErrUnmappable`
    on wrong payload type; the existing `Map` N=100 concurrent-reuse test
    extended to the new class (`Map` stays pure).
  - `internal/tasks/engine`: the group-cancel site populates
    `TaskGroupCancelledPayload.Origin` correctly for each cancel path (operator
    direct, cascade from an ancestor, fail-fast) and carries the member
    outcomes (incl. `Description`).
  - `internal/tui/projection`: the new reducer case (typed decode +
    generic-fallback guard on malformed payload); `ClassifyEventType`
    exhaustiveness for the new type.
  - `internal/tui/app`: transcript renders one muted rollup line; golden
    fixtures for the rendered line and the suppressed (no-line) operator case.
  - `web/console`: taxonomy registration + `filterNotificationEvents` covers
    the new type (vitest, mirrors 188's captured-frame style).
- **Integration:** extends `test/integration/notifications_topic_test.go`
  (`TestE2E_NotificationsTopic`): real bus, real `Subscriber`, a synthetic
  `task.group_cancelled` (Origin=Cascade, mixed outcomes) → correct
  notification; a synthetic Origin=Operator group cancel → NO notification
  (bounded-wait negative check); identity propagation end-to-end; N≥20
  concurrency stress (extends 188's, does not duplicate).
- **Conformance:** N/A — no new persistence driver.
- **Concurrency / leak:** the extended `TestE2E_NotificationsTopic` N≥20
  stress; `Map`'s N=100 concurrent-reuse test extended to the new class; no new
  long-lived goroutine (the class rides 188's existing `Subscriber`).

## Smoke script additions

`scripts/smoke/phase-192.sh` (`# PREFLIGHT_REQUIRES: unit-tests`):

- Static greps: `EventTypeNotificationTaskGroupCancelled` /
  `notification.task_group_cancelled` present in
  `internal/runtime/notifications`; the `CancelOrigin` enum + operator-suppress
  branch present in `mapper.go`; the new type registered in
  `web/console/src/lib/events/taxonomy.ts`.
- `go test ./internal/runtime/notifications/... -run
  'TestMap.*GroupCancelled|TestSubscriber.*GroupCancelled' -race` — mapper
  synthesize/suppress-per-origin coverage.
- `go test ./test/integration/ -run 'TestE2E_NotificationsTopic' -race` — the
  extended round-trip + negative check.
- A live-server events probe where reachable: an `events.subscribe` filter
  probe asserting the new event type is accepted by the filter (never a
  400/`unknown event type`); skips (404/405/501-style) gracefully when the
  surface isn't present in a given build, per the sacred SKIP convention.

## Coverage target

- `internal/runtime/notifications`: 85%
- `internal/tasks/engine` (touched — group-cancel emit path): 85%
- `internal/tui/projection`: 85%
- touched `internal/tui/app` (render path): 85%
- `web/console/src/lib/tasks/` (touched): 85%

## Dependencies

- 188

(188 is the sole dependency: this phase extends 188's notification family,
member-outcome summarisation, TUI `"notification"` block kind, and Console
dock wiring. 187/D-324's cancel hierarchy supplies the *origin* the suppression
rule keys on, but 187 is already Shipped — no ordering constraint. This phase
therefore parallelises in Stage 1 alongside 193/194, all three depending only
on Shipped phases.)

## Risks / open questions

- **Origin provenance completeness.** The suppression rule is only as good as
  the `Origin` value the engine stamps. Every group-cancel call site must set
  a correct origin; a site that forgets defaults to synthesize (fail-loud-not-
  swallow, AC-3), so the failure mode is an *extra* notification, never a
  swallowed one. The `internal/tasks/engine` cancel paths are enumerated in
  187's AC-11 shared cascade helper — verify each against the origin taxonomy
  during implementation.
- **`MaxMemberSummaries` cap coordination.** This phase reuses 188's cap
  constant verbatim — no new cap is introduced. If a large fail-fast batch
  cancels many siblings, the rollup truncates with `MembersTruncated=true`
  exactly as a large resolved group does; confirm the cap value against
  `planner.max_batch_spawns` (186/D-323) as 188's plan already flagged.
- **Solo-task-cancel scope.** Deliberately out of scope (Non-goals). If an
  operator later asks for a solo `notification.task_cancelled`, it is a second
  additive class with its own suppression question — file it then, don't
  pre-build it.

## Glossary additions

- **`notification.task_group_cancelled`** — the conversational-mirror class for
  an unprompted (cascade / fail-fast) cancellation of a batch-spawned task
  group; suppressed for a directly-operator-initiated cancel.
- **CancelOrigin** — the typed classification of why a task/group was cancelled
  (operator / cascade / fail-fast), stamped at the engine call site, keying the
  notification suppression rule.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (the extended `TestE2E_NotificationsTopic` asserts identity propagation
      for the new class; no new identity-scoped storage method added)
- [ ] **If this phase builds a reusable artifact:** N/A — `Map` remains the
      existing pure-function reusable artifact from Phase 188/72d; no new
      compiled artifact is introduced. The existing N=100 concurrent-reuse test
      is extended to the new class, not newly created.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam:** yes — the integration test extends
      `test/integration/notifications_topic_test.go` (tasks→notifications
      producer chain) with real bus, real `Subscriber`, identity propagation,
      and a failure mode (Origin=Operator producing no notification), under
      `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry filed (D-329, pre-assigned)
