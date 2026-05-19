# Phase 72d — `notification.*` event topic + rules-engine-lite mapper

## Summary

Introduces the `notification.*` event family as a NEW event topic on the typed event bus, plus a runtime-internal rules-engine-lite mapper that translates a small, enumerated subset of the existing event taxonomy (`tool.failed`, `task.failed`, `tool.auth_required`, `tool.approval_requested`, `governance.budget_exceeded`, `pause.requested`) into per-class `notification.*` events for the Console's notification center (Brief 11 §CC-3). The wire shape is **per-class topic naming** (`notification.task_failed`, `notification.tool_approval_requested`, `notification.governance_budget_exceeded`, `notification.auth_required`, `notification.pause_requested`) — not a single `notification.emit` with a payload class field — chosen to compose naturally with the existing per-class event taxonomy and the `events.subscribe` topic-filter shape. The §13 primitive-with-consumer rule is satisfied in this phase by a Stage-1 test consumer at `internal/runtime/notifications/notifications_test.go` that fires a deliberate `tool.failed` and asserts a subscriber receives the synthesised `notification.task_failed` via the shared bus; the UI consumers (73a Overview alert ribbon, 73m Settings notification-routing matrix) land in Stage 2 and cannot substitute for the Stage-1 test consumer per the operator amendment locked in `docs/plans/wave-13-decomposition.md` §12 item 5.

## RFC anchor

- RFC §5.2 (streaming events row — the Protocol surfaces the typed event bus, and `notification.*` extends that taxonomy)
- RFC §6.13 (typed event bus — the registry pattern this phase extends)
- RFC §7 (Console layer — Console subscribes to `notification.*` through the Protocol)

## Briefs informing this phase

- brief 11 (Console feature surface — §CC-3 Notifications + §"Notification triggers" starter list)
- brief 06 (events-observability-devx — bus-first model, unified-bus rule, §5 sharp edges)

## Brief findings incorporated

- brief 11 §CC-3: "Each notification is a Protocol-emitted event of type `notification.*` carrying severity, identity scope, summary, deep-link." Harbor lands this as a per-class topic taxonomy (`notification.task_failed`, `notification.tool_approval_requested`, `notification.governance_budget_exceeded`, `notification.auth_required`, `notification.pause_requested`), with the deep-link encoded on the payload — exactly the shape brief 11 anchors against.
- brief 11 §CC-3: the "starter list" of notification triggers (`governance.budget_exceeded`, `tool.auth_required`, `tool.approval_required`, `task.failed`, `agent.credentials_expired`, `runtime.health_degraded`) names the input event types this phase's mapper consumes. V1 maps the five that already exist as shipped event types (`governance.budget_exceeded`, `tool.auth_required`, `tool.approval_requested`, `task.failed`, `pause.requested`); `agent.credentials_expired` and `runtime.health_degraded` are not yet shipped event types, so V1 leaves them unmapped (additive — a later phase adds a new mapping rule when the input event type lands).
- brief 11 §"Open architectural questions" item 2: "Notification taxonomy. Events have severity but no 'notify the user' classification. … Recommend: separate topic, populated by a runtime-internal 'event → notification' mapper for the small subset of event types that surface to users." Harbor adopts the separate-topic recommendation verbatim. The mapper lives in the runtime (not the Console) so a third-party Console implementation gets the same taxonomy out of the box (D-061 + D-091).
- brief 06 §1 + §5: "One bus, not two" — Harbor unifies telemetry, audit-projection, and notifications on the **single** typed event bus the predecessor regretted splitting. `notification.*` events ride the same `EventBus.Publish` path every other event ships through; the mapper does not introduce a parallel channel, it republishes onto the same bus with a different event-type prefix.
- brief 06 §"Metrics cardinality footgun": the mapper does NOT propagate `traceparent`, `RunID`-shaped identifiers, or any free-form caller-controlled string onto the `Event.Extra` label map — the Extra map on a `notification.*` event is empty (Phase 56's metric derivation must remain cardinality-safe).

## Findings I'm departing from (if any)

None. The wire-shape choice (per-class topic vs per-instance `notification.emit`) is settled by `docs/plans/wave-13-decomposition.md` §12's "Wire-shape decision left to me" note. This phase plan does not re-litigate; it implements.

## Goals

- Register a NEW event-type family `notification.*` on the canonical `events.EventType` registry, with five V1 classes (`notification.task_failed`, `notification.tool_approval_requested`, `notification.governance_budget_exceeded`, `notification.auth_required`, `notification.pause_requested`).
- Ship a pure-function rules-engine-lite mapper `Map(Event) []NotificationEvent` that consumes a triggering bus event and synthesises zero or more `notification.*` events. Concurrent calls against a single shared mapper are safe by construction (no internal mutable state).
- Ship a long-lived `Subscriber` that wires the mapper into the bus: subscribes (Admin scope) to the V1 trigger types, runs `Map` on each delivered event, and republishes each resulting `notification.*` event through `EventBus.Publish` with the originating event's `identity.Quadruple` preserved on the synthesised event so identity-scope filtering on the downstream subscriber works without elevation.
- Define the `NotificationPayload` shape on `internal/runtime/notifications/payloads.go` (severity, summary, deep-link reference, originating event ID for correlation) — registered against the canonical `EventPayload` seal pattern (D-028).
- The mapper MUST be a **pure function** with no I/O, no `time.Now()` dependency (the synthesised event's `OccurredAt` is filled by `Publish`, matching the bus convention), no global state, no side effects. Concurrent reuse contract per D-025 is then trivial — the test still runs N≥100 concurrent calls under `-race` to validate construction.
- The Stage-1 test consumer fires a deliberate `tool.failed`, asserts the mapper synthesises a `notification.task_failed` (V1 mapping: `tool.failed` does not directly notify — but `task.failed` does; the test uses `task.failed` to keep the path real-shaped), and asserts a separately-registered subscriber receives the synthesised event via the bus. This satisfies the §13 primitive-with-consumer rule per `docs/plans/wave-13-decomposition.md` §12 lock-in #5.

## Non-goals

- Notification routing fan-out (email / Slack / web-push). Brief 11 §CC-3 says routing is per-user via Settings — that surface lands in 73m Settings + Console DB schema (Phase 72h's `notifications_routing` table), not here.
- Severity escalation policy ("notify only when `task.failed.severity >= warning`"). V1 emits one `notification.*` per matching trigger event; downstream filtering happens on the subscriber side using the existing `events.subscribe` filter shape extended in 72a.
- Snooze / dismiss / mute-this-trigger user actions. Those are Console DB state changes (D-061), not runtime entities; 73a Overview + 73m Settings surface them.
- Anomaly detection ("notify when error-rate exceeds X over Y window"). Post-V1; would consume `events.aggregate` (72a) and re-emit synthetic notifications.
- Persistence of notification history. Notifications ride the same `events` bus replay surface (Phase 06's ring + Phase 57's durable log per D-074); no separate notification store.
- New Protocol methods. `notification.*` is an event topic on the bus, not a method cluster — consumed via the existing `events.subscribe` surface (extended in 72a).
- Mapping `agent.credentials_expired` and `runtime.health_degraded`: those event types are not shipped at V1; the mapper accepts them as future input via a small input-type registry but emits nothing for V1 unmapped inputs. Adding the mappings is a one-line change in a future phase.

## Acceptance criteria

- [ ] `internal/runtime/notifications/notifications.go` declares the five V1 `notification.*` `EventType` constants (`EventTypeNotificationTaskFailed`, `EventTypeNotificationToolApprovalRequested`, `EventTypeNotificationGovernanceBudgetExceeded`, `EventTypeNotificationAuthRequired`, `EventTypeNotificationPauseRequested`) and registers them via `events.RegisterEventType` from package `init()`.
- [ ] `internal/runtime/notifications/payloads.go` declares `NotificationPayload` embedding `events.Sealed` (NOT `events.SafeSealed` — the payload includes a free-form `Summary` string that could surface caller-controlled text; the audit redactor walks it on publish).
- [ ] `internal/runtime/notifications/mapper.go` exports `Map(ctx context.Context, ev events.Event) ([]events.Event, error)` — pure function, no I/O, no global state. Returns synthesised `notification.*` events with the originating event's `identity.Quadruple` and `Type` preserved on the payload's `OriginEventType` / `OriginEventSequence` fields for correlation.
- [ ] `Map` returns `(nil, nil)` for unmapped input event types (silent at the mapper layer; the Subscriber that wires it logs at debug). Unmapped is the expected case for the overwhelming majority of bus events.
- [ ] `Map` returns `(nil, ErrUnmappable)` when a triggering event is structurally invalid (missing identity component, missing payload, payload type does not match the input event type). Fail-loudly per §13 — never silently downgrade to "no notifications."
- [ ] `internal/runtime/notifications/subscriber.go` exports `NewSubscriber(bus events.EventBus, log *slog.Logger) *Subscriber` and `(*Subscriber).Run(ctx context.Context) error` (long-lived goroutine, cancellable via `ctx`, joins on `Run` return). The Subscriber opens an `Admin: true` subscription scoped to the V1 trigger types and republishes synthesised notifications onto the same bus.
- [ ] **Stage-1 §13-compliant test consumer (BINDING per `docs/plans/wave-13-decomposition.md` §12 item 5):** `internal/runtime/notifications/notifications_test.go` includes `TestSubscriber_TaskFailedSynthesisesNotificationTaskFailed` — boots a fresh in-mem `EventBus`, registers a separately-scoped subscriber on `notification.task_failed`, fires a deliberate `task.failed` event through `Bus.Publish`, asserts the `notification.task_failed` arrives at the subscriber with the originating event's identity preserved on the synthesised event.
- [ ] All five V1 mappings have a matching unit test that emits the input event and asserts the synthesised `notification.*` event has the expected severity + originating-event correlation fields.
- [ ] Identity-mandatory: the Subscriber MUST NOT synthesise a `notification.*` event when the triggering event has a missing identity component — emit `notification.identity_rejected` (mirroring D-033 shape) and fail-loudly to the slog Error channel + emit `runtime.error`. (The trigger event itself cannot have a missing component — `events.ValidateEvent` rejects on `Publish` — but if the bus ever delivers an event whose identity is the D-033 `<missing>` sentinel, the mapper skips with this rejection event.)
- [ ] Concurrent-reuse test: `TestMap_ConcurrentReuse` runs N=100 concurrent `Map(ctx, ev)` calls against a single shared mapper instance under `-race`, asserts every call returns the right number of synthesised events per its input, and asserts baseline `runtime.NumGoroutine()` is restored after all calls return (D-025).
- [ ] Integration test `test/integration/notifications_topic_test.go` wires the real `events/drivers/inmem` driver + real `notifications.Subscriber`, deliberately publishes one of each V1 trigger event type, asserts each maps to the correct `notification.*` class on a separately-scoped subscriber, and runs the whole test under `-race` with N≥10 concurrent triggering producers (§17.3).
- [ ] The Subscriber's republish path runs through `audit.Redactor` (via the bus's normal `Publish` path — the bus already redacts non-`SafePayload` payloads per D-028). `NotificationPayload` embeds `events.Sealed` (not `SafeSealed`) precisely so `Summary` text is walked through the redactor.
- [ ] `scripts/smoke/phase-72d.sh` (header: `# PREFLIGHT_REQUIRES: live-server`) exercises both: (a) `events.subscribe` accepts `event_types: ["notification.task_failed"]` as a filter input; (b) emitting a `task.failed` over the Protocol leads to a `notification.task_failed` arriving at a subscriber filtered for that class.
- [ ] `docs/glossary.md` gains entries for: "notification topic", `notification.task_failed`, `notification.tool_approval_requested`, `notification.governance_budget_exceeded`, `notification.auth_required`, `notification.pause_requested`, "rules-engine-lite mapper".

## Files added or changed

```text
internal/runtime/notifications/                          # NEW package
internal/runtime/notifications/notifications.go          # +5 NEW EventType constants + RegisterEventType from init()
internal/runtime/notifications/payloads.go               # +NotificationPayload (Sealed; severity / summary / deep-link / origin-event correlation)
internal/runtime/notifications/mapper.go                 # +Map(ctx, ev) ([]events.Event, error) — pure function
internal/runtime/notifications/mapper_test.go            # unit: every V1 mapping + ErrUnmappable + nil-nil for unmapped + concurrent-reuse N=100
internal/runtime/notifications/subscriber.go             # +Subscriber (long-lived bus consumer that wires Map onto Publish)
internal/runtime/notifications/subscriber_test.go        # §13 Stage-1 test consumer (BINDING) — task.failed → notification.task_failed round-trip
internal/runtime/notifications/errors.go                 # +ErrUnmappable sentinel
test/integration/notifications_topic_test.go             # integration: real EventBus + real Subscriber + every V1 mapping + ≥1 failure mode + concurrency stress
scripts/smoke/phase-72d.sh                               # PREFLIGHT_REQUIRES: live-server
docs/glossary.md                                         # +7 entries (5 classes + "notification topic" + "rules-engine-lite mapper")
```

## Public API surface

```go
// internal/runtime/notifications/notifications.go
package notifications

import "github.com/hurtener/Harbor/internal/events"

const (
    EventTypeNotificationTaskFailed                 events.EventType = "notification.task_failed"
    EventTypeNotificationToolApprovalRequested      events.EventType = "notification.tool_approval_requested"
    EventTypeNotificationGovernanceBudgetExceeded   events.EventType = "notification.governance_budget_exceeded"
    EventTypeNotificationAuthRequired               events.EventType = "notification.auth_required"
    EventTypeNotificationPauseRequested             events.EventType = "notification.pause_requested"
)

// internal/runtime/notifications/payloads.go
type Severity string

const (
    SeverityInfo    Severity = "info"
    SeverityWarning Severity = "warning"
    SeverityError   Severity = "error"
)

type NotificationPayload struct {
    events.Sealed                       // NOT SafeSealed — Summary is caller-controlled, runs through the redactor
    Class               events.EventType // one of the five V1 classes
    Severity            Severity
    Summary             string           // human-readable one-liner
    DeepLink            string           // Protocol-relative path the Console deep-links into
    OriginEventType     events.EventType // the triggering event's Type
    OriginEventSequence uint64           // the triggering event's bus Sequence (correlation key)
}

// internal/runtime/notifications/mapper.go
// Map is a PURE function over (triggering Event) → (synthesised notification events).
// No I/O. No global state. No time.Now(). Concurrent calls against a single
// mapper are safe by construction. Returns (nil, nil) for unmapped event
// types — the expected outcome for the vast majority of bus traffic.
func Map(ctx context.Context, ev events.Event) ([]events.Event, error)

// internal/runtime/notifications/subscriber.go
type Subscriber struct {
    bus events.EventBus
    log *slog.Logger
}

func NewSubscriber(bus events.EventBus, log *slog.Logger) *Subscriber
func (s *Subscriber) Run(ctx context.Context) error  // blocks until ctx cancelled; joins all goroutines on return

// internal/runtime/notifications/errors.go
var ErrUnmappable = errors.New("notifications: triggering event structurally invalid for mapping")
```

## Test plan

- **Unit:**
  - `mapper_test.go::TestMap_TaskFailed_SynthesisesNotificationTaskFailed` — feed a well-formed `task.failed` event; assert exactly one `notification.task_failed` returned with the originating event's identity preserved + correct severity (Error).
  - `mapper_test.go::TestMap_ToolApprovalRequested_SynthesisesNotificationToolApprovalRequested` — feed `tool.approval_requested`; assert `notification.tool_approval_requested` with severity Warning.
  - `mapper_test.go::TestMap_GovernanceBudgetExceeded_SynthesisesNotificationGovernanceBudgetExceeded` — feed `governance.budget_exceeded`; assert `notification.governance_budget_exceeded` with severity Error.
  - `mapper_test.go::TestMap_ToolAuthRequired_SynthesisesNotificationAuthRequired` — feed `tool.auth_required`; assert `notification.auth_required` with severity Warning.
  - `mapper_test.go::TestMap_PauseRequested_SynthesisesNotificationPauseRequested` — feed `pause.requested`; assert `notification.pause_requested` with severity Info.
  - `mapper_test.go::TestMap_UnmappedEventType_ReturnsNilNil` — feed `bus.dropped`; assert `(nil, nil)` (unmapped is the expected case).
  - `mapper_test.go::TestMap_StructurallyInvalidEvent_ReturnsErrUnmappable` — feed `task.failed` with payload of wrong type; assert `ErrUnmappable` wrapped error.
  - `subscriber_test.go::TestSubscriber_TaskFailedSynthesisesNotificationTaskFailed` — **§13 Stage-1 test consumer (BINDING)**: real `events/drivers/inmem` bus; one Subscriber wired in; a separate subscriber filters for `notification.task_failed`; publish `task.failed`; assert the notification arrives with originating identity preserved.
  - `mapper_test.go::TestMap_ConcurrentReuse` — N=100 concurrent `Map` calls against a single mapper under `-race`; each goroutine feeds a different trigger event type; assert all return correctly-shaped output, no data races, baseline `runtime.NumGoroutine()` restored (D-025).
- **Integration:**
  - `test/integration/notifications_topic_test.go::TestE2E_AllV1Mappings_RoundTrip` — real `events/drivers/inmem` bus + real `notifications.Subscriber`. For each V1 trigger event type: publish a well-formed instance, assert the corresponding `notification.*` event arrives at a separately-scoped subscriber within a bounded `eventually` window (no `time.Sleep`). Identity propagation asserted on every notification (§17.3).
  - `test/integration/notifications_topic_test.go::TestE2E_MapperConcurrencyStress` — N≥10 concurrent producers each firing a mix of trigger event types against a single Subscriber + shared bus, under `-race`. Asserts no cross-talk, no dropped notifications, baseline goroutine count restored after teardown (§17.3 long-lived-wiring requirement).
  - `test/integration/notifications_topic_test.go::TestE2E_MissingIdentityFailsLoudly` — deliberately register a custom EventBus that bypasses `ValidateEvent` and publishes an event with the D-033 `<missing>` sentinel identity; assert the Subscriber emits a `notification.identity_rejected`-shaped audit emit + does NOT silently publish a malformed notification (≥1 failure mode requirement, §17.3).
- **Conformance:**
  - N/A — `notifications.*` is a NEW event topic family on the existing bus; there is no driver-conformance suite to extend. The mapper is pure-Go logic with no driver split.
- **Concurrency / leak:**
  - `mapper_test.go::TestMap_ConcurrentReuse` (covered above) — N=100 under `-race`, baseline goroutine restored.
  - `subscriber_test.go::TestSubscriber_RunGoroutineLeak` — start a Subscriber, cancel the ctx, assert `Run` returns within a bounded wait and `runtime.NumGoroutine()` returns to baseline (mandatory per §11 long-lived component rule).

## Smoke script additions

`scripts/smoke/phase-72d.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'events/subscribe' '{"filter": {"event_types": ["notification.task_failed"]}}'` → assert 200 once the Protocol layer ships; SKIP until then (the helper auto-SKIPs).
- A two-step assertion (using the same `protocol_call` helper pattern as `scripts/smoke/phase-72a.sh`):
  1. Subscribe a notification listener: `protocol_call 'events/subscribe' '{"filter": {"event_types": ["notification.task_failed"]}}'`.
  2. Trigger a `task.failed` (test fixture via the Protocol task-control surface OR via a debug-emit helper if one is shipped by Phase 60/61).
  3. Assert the notification arrives on the subscription within the bounded timeout — `assert_json_path '.events[0].type' 'notification.task_failed'` against the SSE buffer endpoint (when shipped).
- `protocol_call 'events/subscribe' '{"filter": {"event_types": ["notification.tool_approval_requested"]}}'` → assert 200 (surface-existence probe for the second V1 class).
- `protocol_call 'events/subscribe' '{"filter": {"event_types": ["notification.governance_budget_exceeded"]}}'` → assert 200.
- `protocol_call 'events/subscribe' '{"filter": {"event_types": ["notification.auth_required"]}}'` → assert 200.
- `protocol_call 'events/subscribe' '{"filter": {"event_types": ["notification.pause_requested"]}}'` → assert 200.
- All five class-name probes use the 72a `EventFilter` shape (which lands in the same Stage-1 batch); the SKIP/OK flip is governed by `events.subscribe`'s liveness, not by 72d's, since 72d ships only the registry constants + mapper + subscriber, not a new HTTP route.

## Coverage target

- `internal/runtime/notifications`: 90% (pure-function mapper + small wiring layer; high coverage is cheap and the §13 + identity-rejection paths need explicit assertions).
- `test/integration/notifications_topic_test.go`: N/A (integration tests don't count toward per-package coverage).

## Dependencies

**Same-wave (Wave 13, Stage 1 — Batch A per `docs/plans/wave-13-decomposition.md` §12 item 2):**

- Phase 72 (events.subscribe scope foundation — the new event family is consumed via `events.subscribe`'s existing surface).
- Phase 72a (events filter + aggregate — Console subscribers will filter for `notification.*` by event-type; the filter shape lands in 72a, not here, but 72d's smoke depends on it for the per-class subscribe assertion).

**Already shipped (pre-Wave 13):**

- Phase 05 (event taxonomy + InMem EventBus — `Shipped`).
- Phase 06 (Bus replay — `Shipped`).
- Phase 03 (audit redactor — `Shipped`; `NotificationPayload` rides the existing non-SafePayload redaction path).
- Phase 20 (TaskService — `Shipped`; supplies `task.failed` event type).
- Phase 30 (tool-side OAuth — `Shipped`; supplies `tool.auth_required`).
- Phase 31 (tool-side approval — `Shipped`; supplies `tool.approval_requested`).
- Phase 36a / 36b (governance — `Shipped`; supplies `governance.budget_exceeded`).
- Phase 50 (unified pause/resume primitive — `Shipped`; supplies `pause.requested`).

## Risks / open questions

- **Wire-shape choice is settled but operator-redirectable.** The decomposition doc §12 leaves a window for the operator to override per-class topic → per-instance `notification.emit` in this phase's review. If overridden, the registry+mapper shape changes (one new event type, payload carries the class enum); the §13 Stage-1 test consumer's existence and shape do not change. Flagged here explicitly per the decomposition doc note.
- **`task.failed` vs `tool.failed` as the §13 demo trigger.** The Wave 13 decomposition doc §9 item 4 says "fires a deliberate `tool.failed` and asserts the rules-engine-lite mapper synthesises a `notification.task_failed`" — but V1's mapper does NOT map `tool.failed` → `notification.task_failed` (a failed tool call doesn't inherently fail the parent task). The Stage-1 test consumer uses `task.failed` → `notification.task_failed` (the direct V1 mapping) to keep the path real-shaped. The smoke script's two-step assertion uses the same shape. Flagged so the coordinator sees the intentional refinement; if the operator wants `tool.failed` to ALSO trigger a notification, that's a one-line addition to the mapper but the V1 mapping table here deliberately keeps it scoped.
- **Severity is heuristic in V1.** The mapper's severity assignment (`task.failed` → Error, `tool.auth_required` → Warning, etc.) is hard-coded per class. Brief 11 §CC-3's "severity above a threshold" hint implies a more nuanced model. V1 ships the heuristic; post-V1 may layer an event-payload-derived severity (e.g. `governance.budget_exceeded.severity` if/when the payload grows one).
- **Deep-link encoding.** `NotificationPayload.DeepLink` is a Protocol-relative path string. V1 hard-codes the shape per class (`/console/tasks/{task_id}`, `/console/tools/{tool_id}/approvals/{approval_id}`, etc.). The shape is informational on the runtime side; the Console renders the deep-link via its router (D-091). If the Console's route shape diverges post-V1, the mapper updates without a Protocol break.
- **Notification de-duplication.** Two identical trigger events in quick succession produce two notifications. Brief 11 §CC-3 mentions "snooze/dismiss/mute-this-trigger" as user actions — those live on Console DB (72h), not here. Runtime-side de-duplication is post-V1 (would require a windowed dedupe key derived from `OriginEventType + identity`).
- **Mapper register-as-pure invariant.** Phase 56's metric derivation may want a counter on `notifications.synthesised{class=...}`. That counter MUST attach at the Subscriber (not the mapper) so the mapper stays pure. Flagged for the 56 phase author; the Subscriber's `Run` is the natural attachment point.
- **Replay interaction with Phase 57's durable log (D-074).** Replayed notification events are byte-identical re-deliveries — a Console reconnecting via replay sees the originally-synthesised notifications, not a re-synthesis. This matches the bus's general replay semantics (Phase 06 + D-029). No special handling needed.

## Glossary additions

- **notification topic** — the `notification.*` event family on Harbor's typed event bus. Per-class topic naming (`notification.task_failed`, `notification.tool_approval_requested`, `notification.governance_budget_exceeded`, `notification.auth_required`, `notification.pause_requested`). Populated by a runtime-internal mapper consuming a small subset of the wider event taxonomy. Phase 72d.
- **`notification.task_failed`** — V1 notification class synthesised from `task.failed` events. Severity Error. Phase 72d.
- **`notification.tool_approval_requested`** — V1 notification class synthesised from `tool.approval_requested` events (Phase 31). Severity Warning. Phase 72d.
- **`notification.governance_budget_exceeded`** — V1 notification class synthesised from `governance.budget_exceeded` events (Phase 36a). Severity Error. Phase 72d.
- **`notification.auth_required`** — V1 notification class synthesised from `tool.auth_required` events (Phase 30). Severity Warning. Phase 72d.
- **`notification.pause_requested`** — V1 notification class synthesised from `pause.requested` events (Phase 50). Severity Info. Phase 72d.
- **rules-engine-lite mapper** — Harbor's runtime-internal pure-function translator from a triggering bus event to zero-or-more `notification.*` bus events. Lives at `internal/runtime/notifications/mapper.go`. Pure by construction — no I/O, no global state, no time-of-day dependency — so concurrent calls against a single instance are trivially safe (D-025). Named "rules-engine-lite" to distinguish from a full rules engine: V1 implements a fixed switch over event types, not a configurable rule DSL. Brief 11 §CC-3, Phase 72d.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (`internal/runtime/notifications`: 90%)
- [ ] If multi-isolation paths changed: cross-session isolation test passes (binding — the synthesised event's `identity.Quadruple` MUST equal the trigger's; the integration test asserts this on every V1 mapping)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `Map` calls against a single shared mapper under `-race`, asserting no data races, no context bleed, baseline goroutine count restored (D-025). The mapper is pure-function so the assertion is straightforward; the test is still mandatory.
- [ ] **Integration test exists** — `test/integration/notifications_topic_test.go` wires real `events/drivers/inmem` + real Subscriber + every V1 mapping + ≥1 failure mode + N≥10 concurrent producers under `-race` (§17.3).
- [ ] **§13 Stage-1 test consumer landed in this phase (BINDING per `docs/plans/wave-13-decomposition.md` §12 item 5)** — `internal/runtime/notifications/subscriber_test.go::TestSubscriber_TaskFailedSynthesisesNotificationTaskFailed` fires a deliberate `task.failed`, asserts the synthesised `notification.task_failed` arrives at a separately-scoped subscriber via the bus. NOT deferred to 73a Overview.
- [ ] Goroutine-leak test passes — `Subscriber.Run` returns within bounded wait after ctx cancel; `runtime.NumGoroutine()` returns to baseline (§11 long-lived-component rule).
- [ ] Glossary updated with the 7 new entries (notification topic, 5 classes, rules-engine-lite mapper)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase — wire-shape choice is per the locked-in decomposition doc §12)
