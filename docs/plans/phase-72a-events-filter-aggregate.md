# Phase 72a — `events.subscribe` filter extensions + `events.aggregate`

## Summary

Extends the Protocol's `events.subscribe` surface with a filter struct (event-type, tenant, user, session, run, time-window) and adds a new `events.aggregate` Protocol method returning time-bucketed count series per event type. Together these unlock the Events page's faceted filters and the per-event-type stacked-area sparkline (`docs/design/console/page-events.md` §12). First consumer is an in-package integration test in `internal/events/` that subscribes with a filter and asserts only matching events are delivered; a Stage-2 Events page (73g) is the UI consumer.

## RFC anchor

- RFC §5.2 (streaming events row)
- RFC §6.13 (typed event bus)
- RFC §7 (Console layer)

## Briefs informing this phase

- brief 11 (Console feature surface — Events view, §LR-5 Event Stream shared component, §CC-4 search)
- brief 12 (deployment + two-surface model)

## Brief findings incorporated

- brief 11 §"Events view": "operators open Events when investigating across sessions (every `tool.failed` in the last hour), debugging a regression (every `planner.repair_exhausted` after deploy), or sampling for anomalies (rate-over-time of `governance.budget_exceeded`)". The filter shape this phase ships is built around exactly those three use cases — event-type + identity + time-window are the primary axes.
- brief 11 §CC-4: events are **high-cardinality runtime-side** — the runtime owns the index and exposes a search method, not Console-side substring matching. `events.aggregate` time-bucketing supports the sparkline without round-tripping every event.
- brief 12 §"the two-surface model": the filter shape MUST be the same one third-party Console implementations consume — so the wire shape is `internal/protocol/types/`, not a Console-private struct.

## Findings I'm departing from (if any)

None.

## Goals

- Extend `events.subscribe` request shape with a filter struct that is identity-scope-aware (cross-tenant filters require the `auth.ScopeAdmin` or `auth.ScopeConsoleFleet` scope claim per D-079 — the closed two-scope set, not a new `events.crosstenant` scope).
- Add a new Protocol method `events.aggregate` that returns time-bucketed counts per event type over a window.
- Both methods MUST honor identity rejection (D-033 shape — fail loudly on missing tuple components; never silently filter to empty).
- Heavy payload bypass: events whose payload exceeds the heavy-content threshold (D-026) ship an `ArtifactRef` rather than inlined bytes; the filter MUST accept event-shape predicates without forcing the runtime to materialize heavy payloads.

## Non-goals

- Cross-runtime aggregation. D-091 — post-V1.
- Saved-view persistence on the runtime side. Saved views are Console-local per D-061.
- Anomaly detection / alert rules. Post-V1 per page-events.md §10.
- Per-event traceparent rendering as an OTel deep-link. D-073 traceparent is already on events; surfacing it in a UI deep-link is post-V1.

## Acceptance criteria

- [ ] `internal/protocol/types/events.go` defines an `EventFilter` struct (event-type set, tenant set, user set, session set, run set, time-window) consumable by `events.subscribe` and `events.aggregate`.
- [ ] `internal/protocol/methods/methods.go` declares `events.aggregate` alongside the existing `events.subscribe` method name.
- [ ] `internal/events/aggregate.go` implements the aggregator: given a filter + window + bucket-size, returns `[]EventBucket` where each bucket has `start`, `end`, and `counts map[string]int64` (counts keyed by event type).
- [ ] `events.subscribe` extends to accept the filter struct on subscription request — when the filter is empty, behavior is identical to today's surface (backward compatible).
- [ ] Identity-scope check: a subscriber without the `auth.ScopeAdmin` scope claim cannot pass multiple tenants in the filter; violation rejected loudly with `ErrIdentityScopeRequired` (never silently downgraded).
- [ ] Identity-rejection check: missing `tenant_id` / `user_id` / `session_id` in the subscriber's identity triple causes the runtime to emit `memory.identity_rejected`-shaped audit event (D-033 pattern, here applied to events surface) and fail the request loudly.
- [ ] Heavy-payload handling: when an event's payload exceeds the heavy-content threshold (D-026), the delivered event carries an `ArtifactRef` not bytes; the filter MUST evaluate against the event header fields without dereferencing the artifact.
- [ ] Filter conformance test runs N≥100 concurrent subscribers with overlapping filters against a single shared `EventBus` instance under `-race` (D-025 concurrent-reuse contract).
- [ ] `events.aggregate` bucket arithmetic is verified: deliberately emit N events across two types over a known window, query aggregate, assert bucket sums match.
- [ ] `scripts/smoke/phase-72a.sh` invokes both methods and asserts response shape.

## Files added or changed

```text
internal/protocol/methods/methods.go               # +events.aggregate const
internal/protocol/types/events.go                  # +EventFilter struct, +EventBucket struct, +EventAggregateRequest, +EventAggregateResponse
internal/protocol/errors/errors.go                 # +ErrIdentityScopeRequired (if not already shipped)
internal/events/aggregate.go                       # +Aggregate(ctx, store, filter, window, bucket) ([]EventBucket, error)
internal/events/aggregate_test.go                  # unit + concurrent-reuse + identity-rejection
internal/events/filter.go                          # +Match(ev, filter) bool — pure predicate over event header fields
internal/events/filter_test.go                     # conformance: filter combinations
internal/events/subscribe_test.go                  # update for filtered subscription
internal/protocol/transports/stream/handlers.go    # +events.aggregate handler wiring
test/integration/events_filter_aggregate_test.go   # cross-tenant scope-claim failure mode + N≥10 concurrent stress
scripts/smoke/phase-72a.sh                         # protocol_call assertions
docs/glossary.md                                   # +"events.aggregate", +"EventFilter", +"EventBucket"
```

## Public API surface

```go
// internal/protocol/types/events.go
type EventFilter struct {
    EventTypes []string  // e.g. ["tool.failed", "planner.repair_exhausted"]
    TenantIDs  []string  // empty = subscriber's own tenant; >1 requires events.crosstenant claim
    UserIDs    []string
    SessionIDs []string
    RunIDs     []string
    Since      time.Time // optional lower bound
    Until      time.Time // optional upper bound
}

type EventBucket struct {
    Start  time.Time
    End    time.Time
    Counts map[string]int64 // event type → count
}

type EventAggregateRequest struct {
    Filter EventFilter
    Window time.Duration // e.g. 1h
    Bucket time.Duration // e.g. 1m — must divide Window
}

type EventAggregateResponse struct {
    Buckets []EventBucket
}

// internal/events/aggregate.go
func Aggregate(ctx context.Context, store Store, req EventAggregateRequest) (EventAggregateResponse, error)

// internal/events/filter.go
func Match(ev Event, filter EventFilter) bool
```

## Test plan

- **Unit:**
  - `filter_test.go` — table-driven matrix: filter combinations vs events; assert `Match` correctness for every combination including empty filter (matches everything).
  - `aggregate_test.go` — deterministic emission of N events across known types over a known window; assert bucket counts and bucket boundary timestamps.
- **Integration:**
  - `test/integration/events_filter_aggregate_test.go` — real `events/drivers/inmem` driver, real Protocol transport. Subscribes with filter; emits a mix of in-scope + out-of-scope events; asserts only in-scope delivered. Cross-tenant subscription without scope claim → `ErrIdentityScopeRequired`. Missing identity component → fails loudly per D-033 pattern.
- **Conformance:**
  - `events.aggregate` runs against the existing event-driver conformance suite (in-mem, sqlite, postgres). All three drivers must produce identical bucket counts for the same input series.
- **Concurrency / leak:**
  - `aggregate_test.go::TestAggregate_ConcurrentReuse` — N=100 concurrent `Aggregate` calls against a single shared store, each with a different filter, under `-race`. Asserts no data races, no context bleed (each goroutine's filter is preserved), baseline goroutine count restored after all calls return (D-025).

## Smoke script additions

`scripts/smoke/phase-72a.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'events/subscribe' '{"filter": {"event_types": ["tool.failed"]}}'` → assert 200; assert response is a subscription cursor.
- `protocol_call 'events/aggregate' '{"filter": {"event_types": ["tool.failed"]}, "window": "1h", "bucket": "1m"}'` → assert 200; `assert_json_path '.buckets | length' 60` (60 one-minute buckets in a one-hour window).
- `protocol_call 'events/subscribe' '{"filter": {"tenant_ids": ["t1", "t2"]}}'` (without the `auth.ScopeAdmin` claim) → `assert_status 403`.
- `protocol_call 'events/aggregate' '{}'` (missing identity in context) → `assert_status 401` or `403`.

## Coverage target

- `internal/events`: 85% (was 80% — this phase tightens because the filter + aggregate logic is testable in isolation).
- `internal/protocol/types`: 90% (struct serialization).
- `internal/protocol/transports/stream`: 80%.

## Dependencies

- Phase 72 (events.subscribe scope foundation).
- Phase 06 (Bus replay + cursor — `Shipped`).
- Phase 60 (Protocol wire transport — `Shipped`).
- Phase 61 (Protocol auth — `Shipped`; supplies the scope-claim primitive).

## Risks / open questions

- **Bucket arithmetic correctness across DST / leap-second boundaries.** `events.aggregate` uses UTC throughout per RFC §6.13; the test plan asserts UTC boundaries explicitly.
- **Postgres driver aggregation cost.** A naive impl scans all events in window then buckets in Go. For high-cardinality tenants this is O(N) per call. Acceptable for V1 (Brief 11 §CC-4 expects runtime-side high-cardinality scan); post-V1 may add a continuous-aggregate materialized view. **Operator may flag this in review** if a different V1 perf posture is required.
- **Heavy-payload payload-shape predicates.** The filter operates on event headers only — predicates over payload contents are deliberately out of scope (would force materialization). Brief 11 §CC-4 acknowledges this (substring search on payload is post-V1).

## Glossary additions

- **EventFilter** — Protocol-level struct for narrowing `events.subscribe` and `events.aggregate` deliveries by event type, identity scope, and time window.
- **EventBucket** — single time-bucketed count series in an `events.aggregate` response.
- **events.aggregate** — Protocol method returning time-bucketed event-type counts. Added in Phase 72a.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (binding for this phase — the filter shape touches the isolation tuple; the integration test asserts cross-tenant rejection without scope claim)
- [ ] **Concurrent-reuse test for the aggregator passes** — N≥100 concurrent `Aggregate` calls against a single shared store under `-race`, asserting no data races, no context bleed, baseline goroutine count restored. (D-025)
- [ ] **Integration test exists** for the cross-package seam — `events/drivers/inmem` ↔ Protocol transport ↔ subscriber identity. Real drivers, ≥1 failure mode, under `-race`. (§17)
- [ ] If new vocabulary: glossary updated (events.aggregate, EventFilter, EventBucket)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase.)
