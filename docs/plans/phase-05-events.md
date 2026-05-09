# Phase 05 — Event taxonomy + InMem `EventBus` + isolation

## Summary

Land `internal/events`: Harbor's typed event subsystem and the in-memory driver every other subsystem will publish to. Ships the exhaustive `EventType` enum (with the V1 starter set Phase 03 / Phase 04 / Phase 36b will populate), the sealed `EventPayload` interface, the `EventBus` interface, server-enforced isolation `Filter`s, per-subscriber bounded fan-out with drop-oldest backpressure, an idle-subscription reaper, and the §4.4 driver-registry seam. Replay-from-cursor is deliberately deferred to Phase 06.

## RFC anchor

- RFC §6.13
- RFC §6.4
- RFC §6.10
- RFC §6.14
- RFC §6.15
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 06
- brief 05

## Brief findings incorporated

- **brief 06 §1 — one bus, not two.** The predecessor split telemetry (`FlowEvent`) from chunked output (`StreamChunk`); every consumer had to fuse two streams to reconstruct a run. Harbor unifies them on a single typed bus from t=0. Phase 05 is where that unification becomes mechanical: ALL subsystems Publish to this one `EventBus`; no parallel observability channel is permitted to land in any later phase.
- **brief 06 §3 — "What subscribers consume."** Filters are server-enforced for isolation; Subscribe rejects any filter that elides the identity triple unless the caller has explicit `admin` scope. Cross-tenant subscriptions are an explicit, audited operation. Phase 05 ships the rejection + audit-emit; the wire-level scope claim is wired in Phase 61 (Protocol auth).
- **brief 06 §4 — bounded channels with explicit drop-oldest + `bus.dropped` event.** When a subscriber's buffer is full, the OLDEST event is dropped and a `bus.dropped` event describing the dropped sequence range is published into the same subscription stream. This converts silent loss into a visible, replayable signal — the predecessor's queue-depth telemetry could not name "I dropped these specific sequences," and that became hard to debug under saturation.
- **brief 06 §4 — idle-subscription reaper.** A subscription whose channel hasn't been drained within the idle window is reaped; the reaping emits `bus.subscription_idle_closed`. This prevents a misbehaving client from pinning runtime state forever.
- **brief 06 §5 — sharp edges to NOT repeat.** The predecessor wires telemetry as middleware (per-node observability) which couples observability lifetime to node execution. Harbor's bus-first model lets observability subscribe at any granularity (planner step, tool, run, session) without inserting middleware.
- **brief 05 (sessions / state interplay).** The bus's identity-scoped subscribe semantics rely on the `(tenant, user, session)` triple being present in `ctx` from Phase 01; the future durable-event-log driver (Phase 57) will key persisted events by `(SessionID, Sequence)` against the StateStore landing in Phase 07. Phase 05 does NOT depend on Phase 07 directly — the in-memory driver is StateStore-free — but the surface is shaped to make Phase 57's wiring mechanical.
- **decisions D-020 — Audit owns redaction; the bus consumes the redactor.** Every `Publish` runs the payload through `audit.Redactor.Redact` before the event hits the in-memory queue. A redaction error means the event is NOT published; the bus instead publishes an `audit.redaction_failed` event using a stripped (payload-less) form AND returns the error to the caller. This makes redaction failure observable without leaking unredacted bytes.

## Findings I'm departing from (if any)

- None.

## Goals

- Ship the canonical `EventBus` interface every other subsystem will Publish to and Subscribe from. Locking the surface here unblocks Phases 04 (logger emits `runtime.error`), 03 (already calling sites wire to this bus), 36a/36b (Governance emits `governance.budget_exceeded` / `governance.rate_limited`), and Phase 06 (replay).
- Make identity-mandatory subscription mechanically obvious: no `Filter` can elide the triple without an `Admin` opt-in, and admin opt-in is audited.
- Deliver the in-memory MPSC ingress + per-subscriber bounded fan-out + drop-oldest backpressure as the V1 reference driver, behind the §4.4 driver-registry seam, so Phase 06's replay-equipped driver and Phase 57's durable-log driver plug in without changing callers.
- Bake redaction-before-emit into `Publish` so D-020 holds at the bus boundary, not just at log boundaries.
- Establish the `EventPayload` sealed-interface pattern + the exhaustive `EventType` registry idiom so future subsystems extend the taxonomy without breaking the "exhaustive enum" property.

## Non-goals

- No replay-from-cursor or ring buffer (Phase 06 owns that — the `Cursor` type is OUT of scope here; Phase 06 introduces it).
- No durable event log driver (Phase 57; depends on Phase 07 StateStore + Phase 15/16 SQLite/Postgres state drivers).
- No OpenTelemetry trace / metric derivation (Phase 55 / 56). The bus carries the data; OTel derives.
- No `harbor dev`-side default subscription filter (CLI ergonomics — Phase 64).
- No metrics emission. The `Extra map[string]string` field is RESERVED on `Event` for future Phase 56 metric labels but no derivation runs in Phase 05. The cardinality lint check ships as a STUB script that becomes binding when Phase 56 lands real metric emit paths.
- No actual `admin`-scope verification logic — Phase 05 enforces "filter must have triple OR `Admin: true`" but trusts the caller's `Admin` boolean. The cryptographic scope claim wiring is Phase 61 (Protocol auth). Documented as a deliberate seam.
- No protocol-wire encoding of events. The Protocol exposes the bus to remote consumers in Phase 60; Phase 05 ships only the in-process interface.

## Acceptance criteria

- [ ] `internal/events/events.go` defines `EventType`, `EventPayload`, `Event`, `EventBus`, `Filter`, `Subscription`, the sentinel errors enumerated under "Public API surface", and the `WithBus` / `MustFrom` / `From` ctx helpers.
- [ ] `EventType` is a string-typed value backed by an internal exhaustive registry (`var allEventTypes = map[EventType]struct{}{}`). Each canonical type is registered via an exported constant + an `init()` registration in its declaring file. `IsValidEventType(EventType) bool` reports membership; `EventTypes() []EventType` returns the deterministic full set. Unknown types passed to `Publish` are rejected with `ErrUnknownEventType`.
- [ ] V1 starter set of `EventType`s is registered: `runtime.error`, `runtime.warning`, `bus.dropped`, `bus.subscription_idle_closed`, `audit.redaction_failed`, `governance.budget_exceeded`, `governance.rate_limited`. The first three are emitted BY the bus / runtime in this phase; the last four are reserved for owning subsystems (Phase 04 emits `runtime.*`, Phase 03 already exists and gains an emit hook in this phase or a later wiring PR, Phase 36b emits the `governance.*` set). Adding new types in later phases is at-the-seam: registering a new exported constant + `init()` line.
- [ ] `EventPayload` is a sealed interface (`isEventPayload()` unexported method); concrete payload types live alongside their owning subsystems but implement the seal. The events package ships the bus-internal payload types (`BusDroppedPayload`, `SubscriptionIdleClosedPayload`, `AuditRedactionFailedPayload`).
- [ ] `Event` carries `Type EventType`, `Identity identity.Quadruple`, `OccurredAt time.Time`, `Sequence uint64`, `Payload EventPayload`, plus `Extra map[string]string` reserved for Phase 56's bounded low-cardinality metric labels. `Sequence` is per-bus monotonic, gap-free; assigned by `Publish` (callers must NOT pre-fill it).
- [ ] `EventBus.Publish(ctx, Event) error` runs `Event.Payload` through `audit.Redactor.Redact` BEFORE enqueueing. On redaction error: returns the wrapped error AND publishes a payload-less `audit.redaction_failed` event so the failure is observable; the original event with its raw payload is NOT enqueued.
- [ ] `EventBus.Subscribe(ctx, Filter) (Subscription, error)` rejects with `ErrIdentityScopeRequired` when `Filter.Tenant`, `Filter.User`, or `Filter.Session` is empty AND `Filter.Admin == false`. When `Filter.Admin == true` AND the triple is partially specified (cross-tenant fan-in), the bus emits an `audit.admin_scope_used` event before returning the subscription.
- [ ] Per-session subscriber cap defaults to 16; configurable via `EventsConfig.MaxSubscribersPerSession`. Exceeding it returns `ErrSubscriberLimitReached`.
- [ ] Per-subscriber buffer defaults to 256; configurable via `EventsConfig.SubscriberBufferSize`. When full, `Publish` to that subscriber drops the OLDEST queued event, advances a per-subscriber drop window, and at most once per `EventsConfig.DropWindow` (default 1s) emits `bus.dropped` with the dropped sequence range to that subscriber's stream.
- [ ] Idle-subscription reaper runs on a `time.Ticker` (interval `EventsConfig.IdleTimeout / 4`); a subscription whose `Events()` channel has not been drained within `IdleTimeout` (default 60s) is `Cancel`led and emits `bus.subscription_idle_closed` to whatever drain remains in its buffer before close.
- [ ] `internal/events/drivers/inmem/inmem.go` implements the interface; `init()` registers the driver under name `"inmem"`. `cmd/harbor/main.go` blank-imports the driver (extending the existing audit-driver blank-import block).
- [ ] `internal/events/registry.go` mirrors the §4.4 pattern from `internal/audit/registry.go`: `Register`, `Open(ctx, cfg, audit.Redactor)`, `OpenDriver(name, cfg, audit.Redactor)`, `RegisteredDrivers() []string`. Default driver is `"inmem"` until later phases register replay-equipped or durable-log drivers.
- [ ] `internal/config/config.go` updates `EventsConfig` from its zero-valued reservation to: `Driver string` (default `"inmem"`), `MaxSubscribersPerSession int` (default 16), `SubscriberBufferSize int` (default 256), `IdleTimeout time.Duration` (default 60s), `DropWindow time.Duration` (default 1s). All fields default per the values above when zero-valued; none are `reload:"live"` in V1 (default `restart`-required, per AGENTS.md §10).
- [ ] `internal/config/validate.go` adds an `EventsConfig` validator that rejects out-of-range values (negative caps, zero buffer, etc.) with the standard `config.events.<field>` error path.
- [ ] No package-level mutable state on `EventBus` instances or driver structs. Per-run / per-subscription state lives on the `Subscription` struct and is bounded by `Cancel`. Compiled bus is reusable across N goroutines (D-025).
- [ ] Coverage on `internal/events` ≥ 85%; on `internal/events/drivers/inmem` ≥ 85%.
- [ ] Concurrent-reuse test: `TestBus_ConcurrentReuse_ReuseContract` runs ≥100 goroutines `Publish`ing independent events to a single shared bus while ≥10 subscribers consume; assert no cross-tenant events leak across subscriptions, no race detector hits, no goroutine leak after `Close(ctx)` (baseline `runtime.NumGoroutine()` restored within 2s of close).
- [ ] Cross-tenant isolation test: tenant A's subscriber receives ZERO events emitted with tenant B's identity, even when tenant B publishes ≥1k events. Asserted under `-race`. Pins AGENTS.md §13 forbidden practice "Cross-session queries without an explicit elevated scope claim."
- [ ] Drop-oldest test: a slow subscriber + a fast producer triggers ≥1 `bus.dropped` event, with the reported sequence range matching the dropped events. Drop-window throttling asserted: bursts of saturation produce at most one `bus.dropped` per `DropWindow`.
- [ ] `TestEventTypes_Exhaustiveness` enumerates `EventTypes()` and asserts each canonical V1 starter type is present. The test fails if any starter type is removed without replacement.
- [ ] Goroutine leak test: `TestBus_GoroutineLeak_AfterClose` asserts `runtime.NumGoroutine` returns to baseline within 2s of `Close(ctx)` for: idle bus, bus with active subscribers, bus mid-publish saturation, bus with reaper-cancelled subscriptions.
- [ ] `scripts/check-event-cardinality.sh` (new) lints `internal/events` AND `internal/metrics` (when it lands) for `slog.String("run_id", ...)` / `slog.String("trace_id", ...)` patterns inside metric-emit code paths. Phase 05's stub form `grep`s and exits 0 (no metric code exists yet); Phase 56 will harden it to a binding check. Listed as a Phase 05 deliverable so the slot exists when Phase 56 lands.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-05.sh` smoke script exercises `go test -run TestEventTypes_Exhaustiveness ./internal/events/...` against the built tree; the protocol surface check still SKIPs (no Protocol layer until Phase 58+).

## Files added or changed

- `internal/events/events.go` (new) — `EventType` registry, `EventPayload` seal, `Event`, `EventBus` interface, `Filter`, `Subscription` interface, sentinel errors, ctx helpers.
- `internal/events/payloads.go` (new) — bus-internal payload types (`BusDroppedPayload`, `SubscriptionIdleClosedPayload`, `AuditRedactionFailedPayload`, `AdminScopeUsedPayload`); each implements the sealed interface.
- `internal/events/registry.go` (new) — driver registry mirroring `internal/audit/registry.go` (§4.4 seam).
- `internal/events/events_test.go` (new) — interface, registry, and exhaustiveness tests.
- `internal/events/drivers/inmem/inmem.go` (new) — InMem driver: MPSC ingress, fan-out goroutine, per-subscriber buffered channels, reaper, `init()`-time registration.
- `internal/events/drivers/inmem/inmem_test.go` (new) — driver-level tests: publish→subscribe round-trip, drop-oldest, idle reaper, concurrent-reuse, cross-tenant isolation, goroutine-leak.
- `internal/config/config.go` (modified) — `EventsConfig` gains the five fields enumerated under "Acceptance criteria"; the reserved zero-valued struct from Phase 02 is replaced.
- `internal/config/loader.go` (modified) — `defaults()` populates the new `EventsConfig` defaults.
- `internal/config/validate.go` (modified) — `validateEvents` per-section validator added; root `Validate()` orchestrator includes it.
- `internal/config/validate_test.go` (modified) — table-driven cases extended with `EventsConfig` failure modes.
- `examples/harbor.yaml` (modified) — gains an `events:` block at the documented defaults.
- `cmd/harbor/main.go` (modified) — blank-imports `_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"` alongside the existing audit driver import.
- `scripts/check-event-cardinality.sh` (new) — stub linter that exits 0 in Phase 05; tightens in Phase 56.
- `scripts/smoke/phase-05.sh` (new) — exercises the exhaustiveness test; SKIPs the protocol surface.
- `docs/plans/phase-05-events.md` (this file).

No top-level directory additions; `internal/events/` is enumerated in AGENTS.md §3.

## Public API surface

```go
package events

import (
    "context"
    "errors"
    "time"

    "github.com/hurtener/Harbor/internal/audit"
    "github.com/hurtener/Harbor/internal/config"
    "github.com/hurtener/Harbor/internal/identity"
)

// EventType is a string-typed exhaustive enum. Each canonical type is
// declared as an exported constant + registered in init() so the
// registry stays the single source of truth.
type EventType string

const (
    EventTypeRuntimeError              EventType = "runtime.error"
    EventTypeRuntimeWarning            EventType = "runtime.warning"
    EventTypeBusDropped                EventType = "bus.dropped"
    EventTypeBusSubscriptionIdleClosed EventType = "bus.subscription_idle_closed"
    EventTypeAuditRedactionFailed      EventType = "audit.redaction_failed"
    EventTypeAdminScopeUsed            EventType = "audit.admin_scope_used"
    EventTypeGovernanceBudgetExceeded  EventType = "governance.budget_exceeded"
    EventTypeGovernanceRateLimited     EventType = "governance.rate_limited"
)

// EventPayload is the sealed payload interface. Concrete payload types
// live alongside their owning subsystems and implement isEventPayload().
type EventPayload interface {
    isEventPayload()
}

// Event is the canonical bus record.
type Event struct {
    Type       EventType
    Identity   identity.Quadruple
    OccurredAt time.Time
    Sequence   uint64 // assigned by Publish; callers must not pre-fill
    Payload    EventPayload
    Extra      map[string]string // reserved for Phase 56 metric labels
}

// Filter is the server-enforced subscription predicate. Subscribe
// rejects filters that elide the identity triple unless Admin is set.
type Filter struct {
    Tenant  string
    User    string
    Session string
    Types   []EventType
    Admin   bool
}

// Subscription delivers events to one consumer.
type Subscription interface {
    Events() <-chan Event
    Cancel()
}

// EventBus is the canonical pub/sub surface. Implementations MUST be
// safe for concurrent use by N goroutines against a single shared
// instance (D-025).
type EventBus interface {
    Publish(ctx context.Context, ev Event) error
    Subscribe(ctx context.Context, f Filter) (Subscription, error)
    Close(ctx context.Context) error
}

// Sentinel errors. Callers compare via errors.Is.
var (
    ErrUnknownEventType        = errors.New("events: unknown EventType")
    ErrIdentityScopeRequired   = errors.New("events: filter must specify (tenant, user, session) unless Admin")
    ErrAdminScopeRequired      = errors.New("events: admin scope required for cross-session/cross-tenant subscription")
    ErrSubscriberLimitReached  = errors.New("events: per-session subscriber limit reached")
    ErrBusClosed               = errors.New("events: bus is closed")
)

// Open returns the configured EventBus driver (default: "inmem").
// The audit.Redactor is mandatory: every Publish runs payloads through
// it before enqueueing.
func Open(ctx context.Context, cfg config.EventsConfig, r audit.Redactor) (EventBus, error)

// OpenDriver opens a specific driver by name; useful for tests.
func OpenDriver(name string, cfg config.EventsConfig, r audit.Redactor) (EventBus, error)

// Register installs a driver factory (called from driver init()).
type Factory func(config.EventsConfig, audit.Redactor) (EventBus, error)

func Register(name string, factory Factory)
func RegisteredDrivers() []string

// IsValidEventType reports whether t is in the canonical registry.
func IsValidEventType(t EventType) bool

// EventTypes returns a deterministic snapshot of every registered type.
func EventTypes() []EventType

// WithBus / MustFrom / From propagate an EventBus on a context. Mirror
// of the audit and identity ctx-helper pattern; unexported context key.
func WithBus(ctx context.Context, b EventBus) context.Context
func MustFrom(ctx context.Context) EventBus
func From(ctx context.Context) (EventBus, bool)
```

`Event`, `EventType`, `EventPayload`, `Filter`, `Subscription`, `EventBus`, `Open`, the sentinel errors, and the ctx helpers are the entire public surface. The driver registry's internal `factories` map and the InMem driver's per-subscriber state stay unexported.

## Test plan

- **Unit:**
  - `EventType` registry: `IsValidEventType` matches the V1 starter set; unknown types fail; `EventTypes()` returns deterministic order.
  - `Event` zero-value safety: `Sequence` MUST be assigned by Publish (caller-set values rejected with a wrapped error).
  - `EventPayload` seal: a payload type that doesn't implement `isEventPayload()` will not compile (compile-time enforcement; test asserts via a type-check helper).
  - `Filter` validation: empty triple + Admin=false → `ErrIdentityScopeRequired`; partially-empty triple + Admin=true emits `audit.admin_scope_used` and returns subscription.
  - Sentinel error wrapping: every error from `Publish` / `Subscribe` is `errors.Is`-compatible with the listed sentinels.
  - Driver registry: `Register` panics on duplicate name, empty name, nil factory (mirror of audit registry tests).
- **Integration:**
  - Publish → Subscribe round-trip: tenant-A bus, single subscriber, asserts every published event arrives in `Sequence` order.
  - Audit-before-emit: Publish with a payload that triggers a Redactor error → return wrapped error AND emit `audit.redaction_failed` (asserted via a parallel admin subscriber).
  - Per-session subscriber cap: 17th Subscribe in same `(tenant, user, session)` returns `ErrSubscriberLimitReached`.
  - Idle reaper: Subscribe but never `<-Events()`; after `IdleTimeout` (using a controllable clock; never `time.Sleep`), assert reaper cancels the subscription and emits `bus.subscription_idle_closed`.
  - Drop-oldest: producer at 10×subscriber drain rate fills buffer; oldest events dropped; `bus.dropped` emitted ≤ once per `DropWindow`.
  - Cross-tenant isolation: tenant A publishes 1k events; tenant B subscriber receives 0; asserted under `-race`.
- **Conformance:** N/A this phase (single driver in V1; future drivers — Phase 06 ring-buffer-equipped, Phase 57 durable-log — will share the same correctness suite at that time. The seam is in place; the suite materialises when ≥2 drivers exist).
- **Concurrency / leak (D-025 concurrent-reuse contract):** `TestBus_ConcurrentReuse_ReuseContract` runs ≥100 goroutines `Publish`ing independent events with mixed `(tenant, user, session)` identities to a single shared bus; ≥10 subscribers consume; asserts (a) no race detector hits under `-race`, (b) no cross-tenant event leaks across subscriptions, (c) cancelling one subscriber's ctx does not affect siblings, (d) goroutine count returns to baseline within 2s of `bus.Close(ctx)`. AGENTS.md §5 + §11 + RFC §3.5.
- **Goroutine leak:** `TestBus_GoroutineLeak_AfterClose` covers idle, active, saturated, and reaper-active bus states; baseline-restored after Close.
- **Cardinality lint (stub):** `scripts/check-event-cardinality.sh` exits 0 in Phase 05; Phase 56 hardens it. The phase plan documents the slot's existence so the binding check is not lost.

## Smoke script additions

- `scripts/smoke/phase-05.sh` runs `go test -run TestEventTypes_Exhaustiveness ./internal/events/...` to verify the exhaustive-enum invariant. If the test passes, increments `OK`; on failure, increments `FAIL`. The protocol-side surface is `skip`ped with `"phase 05: events bus has no HTTP/Protocol surface yet"` so preflight stays green until Phase 58+.
- Uses the existing `common.sh` helpers — no new helpers introduced. Reference shape: `phase-01.sh` (Go-test-only smoke).

## Coverage target

- `internal/events`: 85%.
- `internal/events/drivers/inmem`: 85%.

## Dependencies

- Phase 01 (identity) — `Quadruple` carried on `Event`; identity validators back the `Filter` triple gating.
- Phase 03 (audit redactor) — `audit.Redactor` is a mandatory parameter to `Open`; redaction-before-emit is contract.
- (Phase 02 — config — already shipped; this phase fills the reserved `EventsConfig` slot.)

## Risks / open questions

- **`Filter.Admin` is trust-based in Phase 05.** The scope claim is not cryptographically verified until Phase 61 wires Protocol auth. Documented as a deliberate seam: a malicious caller passing `Admin: true` against an in-process bus would bypass triple-scoped subscribe, but the bus runs only in-process in V1 (Phase 60+ adds the Protocol-exposed wire surface, at which point Phase 61 enforces the scope claim). The audit emit on `Admin: true` makes any abuse retroactively detectable.
- **Cardinality lint is a stub in Phase 05.** Phase 56 (metrics derivation) is where the binding check fires. Risk: a future contributor adds a metric label keyed by `RunID`/`TraceID` between Phases 05 and 56. Mitigation: AGENTS.md §13 already lists "metric labels keyed by `RunID`/`TraceID`" implicitly via the cardinality discipline; the script is a slot to point CI at when Phase 56 lands.
- **Sequence numbering under high contention.** Per-bus monotonic `Sequence` is implemented with `atomic.Uint64.Add(1)`; ordering across producers is best-effort (the order a goroutine wins the atomic Add is the order subscribers see). This is acceptable per RFC §6.13's "monotonic per-bus, gap-free" wording — gap-freedom holds even under heavy concurrency because Add is atomic; strict producer-order does not.
- **Reaper interval vs idle timeout.** Default reaper ticks every `IdleTimeout / 4` (15s for the 60s default). Tests use a controllable clock to validate reaping deterministically; production uses real time. Documented in the InMem driver's godoc.
- **EventPayload sealed interface and external subsystem extension.** Subsystems outside `internal/events` need to declare their own payload types (e.g., `internal/governance/payloads.go` will declare `BudgetExceededPayload`). The `isEventPayload()` method is package-private; cross-package implementation requires an exported helper (`func PayloadSeal()`). Documented in the events package godoc; the seal is enforced at compile time; the helper sits at `events.PayloadSeal` and is called from each foreign payload type via an unexported method body that calls it. Standard Go pattern; no real risk.
- **Open RFC §11 questions:** the bus wire format question (Q-1 / brief 06 §9-1) is OUT of scope for Phase 05 — the in-process interface is wire-agnostic; Protocol transports land in Phase 60.

## Glossary additions

- **Filter (events)** — the server-enforced subscription predicate carried on `EventBus.Subscribe`. Mandates the identity triple (`Tenant`, `User`, `Session`) unless `Admin` is set; the bus rejects empty-triple non-admin filters with `ErrIdentityScopeRequired` and audit-emits `audit.admin_scope_used` when admin scope is exercised. RFC §6.13, brief 06 §3-§4.
- **Subscription (events)** — the typed handle returned from `EventBus.Subscribe`. Owns one bounded buffer per subscriber, drops oldest on saturation (emitting `bus.dropped`), and is reaped after `IdleTimeout` of un-drained backlog (emitting `bus.subscription_idle_closed`). `Cancel()` is idempotent. RFC §6.13, brief 06 §4.

(`Event bus` already has a glossary entry; this phase does not redefine it.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (85% / 85%)
- [ ] If multi-isolation paths changed: cross-session isolation test passes (this phase introduces the bus-side enforcement; cross-tenant + cross-session isolation tests are in scope and listed under "Test plan")
- [ ] **Concurrent-reuse test passes** — N≥100 publishes against a single shared bus with M≥10 subscribers, under `-race`, asserting no data races, no context bleed (cross-tenant), no cancellation cross-talk, no goroutine leaks. AGENTS.md §5 + §11 + D-025.
- [ ] If new vocabulary: glossary updated (yes — `Filter (events)`, `Subscription (events)` added)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed from)
