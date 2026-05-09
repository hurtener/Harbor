# Phase 06 — Bus replay + ring buffer + cursor

## Summary

Extend `internal/events` with the replay-from-cursor capability deferred from Phase 05. Land the `Cursor`, `Replayer` capability interface, an in-memory ring buffer (default 10k events, configurable) on the `inmem` driver, and the gap-free / no-duplicate guarantees that let a late or disconnected subscriber resume cleanly. Replay degrades to documented loss when a cursor is older than the ring's tail; the durable event log driver (Phase 57) is the long-term answer for unbounded retention.

## RFC anchor

- RFC §6.13

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- **brief 06 §4 — replay semantics.** "Without a durable log, replay is best-effort within an in-memory ring buffer (default 10k events, configurable). With a durable log driver attached (which uses the StateStore from research brief 05), replay is exact. The `events.Cursor` is just `(SessionID, Sequence)`; clients that resume after a disconnect pass the last acknowledged cursor and pick up cleanly." Phase 06 is the in-memory ring side of that statement; Phase 57 is the durable side.
- **brief 06 §3 — gap-free guarantee within a `RunID`.** "The bus guarantees no duplicates and no gaps within a `RunID`." This is the load-bearing replay invariant: a subscriber who replays from cursor `(S, N)` and then transitions to live receives events `N+1, N+2, ...` with no duplicates of anything already at-or-before `N`, and no holes within any single `RunID` even when sessions interleave. Phase 06's replay implementation must enforce both.
- **brief 06 §4 — drop-oldest is already wired** (Phase 05). Replay uses the SAME drop policy: when the ring overflows, the oldest event is evicted; replay-from-an-evicted-cursor returns `ErrCursorTooOld`. This is symmetric with the per-subscriber drop policy and surfaces the loss explicitly rather than silently truncating history.
- **brief 06 §5 — sharp edges.** The predecessor's two-channel split (telemetry vs. chunked output) made replay impossible to reason about at the consumer end. Harbor's single-bus model means replay returns ALL event types within scope; subscribers filter at consumption time. Phase 06 must NOT introduce any per-channel ring carving.
- **brief 06 §6 — late-subscriber replay tests are required.** "Subscribe-from-cursor with both ring-buffer and durable backends" — Phase 06 ships the ring-buffer-side test; Phase 57 will ship the durable-backend side using the same test shape.

## Findings I'm departing from (if any)

- **brief 06 §2 sketch — `Replay` returns a `Subscription`, not a slice.** The brief sketch has `Replay(ctx, Cursor, Filter) (Subscription, error)` returning a fresh Subscription whose stream interleaves historical-then-live. That coupling makes the historical and live boundary fuzzy and complicates the no-duplicate guarantee (the bus would have to dedupe at the seam between snapshot and live tail). Phase 06 instead ships `Replay(ctx, Cursor, Filter) ([]Event, error)` returning a snapshot of historical events strictly between the cursor and the bus's current sequence, and **the caller is responsible** for combining the snapshot with a fresh `Subscribe` if it wants to continue live. The snapshot's last sequence becomes the cursor input to a follow-up `Subscribe` (which the caller then live-tails). The split is what gives the no-duplicate / no-gap guarantee a clean home: the bus stamps every event with `Sequence` at `Publish`, and a subscriber's cursor is just "the last sequence I have." If we later need a one-shot `ReplayAndSubscribe`, it's a thin convenience wrapper composed on top of these two primitives — no surface change to drivers. Documented in `docs/decisions.md` as **D-029** in this PR.

## Goals

- Land the `Cursor` type + `Replayer` capability interface in `internal/events` so callers can type-assert (`if r, ok := bus.(events.Replayer); ok { ... }`) without polluting the core `EventBus` surface.
- Add an in-memory ring buffer to the `inmem` driver with the configured retention; `Publish` appends to the ring under the same lock that assigns `Sequence`, so the ring is always consistent with the live broadcast.
- Implement `Replay` against that ring with: (a) cross-tenant filter applied identically to `Subscribe`, (b) gap-free guarantee within `RunID` enforced (no event is ever skipped between two events sharing the same `RunID`), (c) `ErrCursorTooOld` when the cursor's `Sequence` is older than the ring tail, (d) `nil, nil` when the cursor is at-or-after the bus head (no events newer to replay).
- Make replay reusable: `Replay` is safe to call concurrently with `Publish` and `Subscribe`; the ring snapshot is consistent (no torn reads) under `-race` with N≥100 producers and ≥10 replayers.
- Add the configuration knob (`EventsConfig.ReplayBufferSize`) and validator entry; default 10000, zero means "no replay" (drives `Replayer.Replay` to return `ErrReplayUnavailable`).

## Non-goals

- No durable event log driver (Phase 57; depends on Phase 07 + 15 + 16). Phase 06's ring is RAM-only and lost on process exit.
- No replay-then-live splice on the bus surface. The caller composes that from `Replay` + `Subscribe`.
- No partial-cursor matching (e.g. "give me events for `RunID=R-7` regardless of Sequence"). Cursor is `(SessionID, Sequence)` exactly.
- No cross-bus replay (the cursor is meaningful only against the bus that assigned the sequence numbers).
- No Protocol-wire encoding of the replay surface. Phase 60 (Protocol wire transport) will expose this.
- No metric labels derived from replay (Phase 56 territory).
- No re-numbering of existing tests or refactor of Phase 05's broadcast path. Phase 06 is strictly additive.

## Acceptance criteria

- [ ] `internal/events/events.go` defines `Cursor{SessionID, Sequence}`, the `Replayer` capability interface (`Replay(ctx context.Context, from Cursor, f Filter) ([]Event, error)`), and the new sentinels `ErrCursorTooOld` and `ErrReplayUnavailable`. `EventBus` is **unchanged** — `Replayer` is a separate optional interface callers type-assert.
- [ ] `internal/events/drivers/inmem/inmem.go` implements `Replayer` on the existing `*Bus` type (no new struct). The ring buffer is appended under the same critical section that assigns `Sequence` so ring contents and broadcast are atomically consistent.
- [ ] `Replay(ctx, from, f)` rejects empty-triple non-admin filters with `ErrIdentityScopeRequired` (same rule as `Subscribe`); `Admin: true` with cross-tenant fan-in emits `audit.admin_scope_used` exactly as `Subscribe` does. The two surfaces share the filter-validation helper.
- [ ] `Replay` returns events with `ev.Sequence > from.Sequence` AND matching the filter (Tenant/User/Session/Types). Events are returned in `Sequence` order (gap-free within any single `RunID` present). The returned slice is owned by the caller; the bus doesn't retain a reference.
- [ ] When `from.Sequence` is older than the ring's oldest live entry, `Replay` returns `(nil, ErrCursorTooOld)` AND surfaces enough information for the caller to fall back to the durable log: the error wraps `fmt.Errorf("oldest=%d requested=%d", oldestSeq, from.Sequence)`. The caller (Phase 57+) interprets the gap and reads from durable storage.
- [ ] When `from.Sequence == 0`, `Replay` returns the entire ring's contents matching the filter (the "subscribe from beginning" case). When `from.Sequence >= bus.head`, `Replay` returns `(nil, nil)` (no error, empty slice — nothing newer).
- [ ] When `EventsConfig.ReplayBufferSize == 0`, the driver disables the ring entirely; `Replay` returns `(nil, ErrReplayUnavailable)` immediately. Documented as the "no replay capacity configured" mode; the type assertion `bus.(events.Replayer)` still succeeds, the caller learns at call time.
- [ ] Ring eviction policy: when the ring is full, the oldest entry is evicted on each `Publish` to make room. No `bus.dropped` event is emitted for ring eviction (drop-oldest applies to subscriber-side fan-out only; ring eviction is a documented retention policy, not a delivery failure).
- [ ] `internal/config/config.go` adds `EventsConfig.ReplayBufferSize int` (`yaml:"replay_buffer_size"`); default 10000.
- [ ] `internal/config/validate.go` rejects negative values with the `config.events.replay_buffer_size` error path. Zero is accepted (means "replay disabled").
- [ ] No package-level mutable state added by Phase 06 (the ring lives on the existing `*Bus` instance, behind the existing mutex). Compiled bus remains reusable across N goroutines (D-025).
- [ ] Coverage on `internal/events/drivers/inmem` ≥ 85% (matches phase 05 target). New replay-only paths covered by direct unit tests; the existing Phase 05 tests must continue to pass unchanged.
- [ ] **Concurrent-reuse test (D-025):** `TestReplay_ConcurrentReuse_ReuseContract` runs ≥100 goroutines concurrently `Publish`ing AND `Replay`ing AND `Subscribe`ing on a single shared bus; under `-race`, asserts no data races, no goroutine leak after `Close`, and that every `Replay` snapshot is internally consistent (sequences strictly increasing, no torn entries).
- [ ] **Cross-tenant isolation on Replay:** `TestReplay_CrossTenant_Isolation` publishes events for tenants A and B interleaved; a tenant-A `Replay` returns ZERO tenant-B events. Run under `-race`. Pins AGENTS.md §13 forbidden practice.
- [ ] **Gap-free within RunID:** `TestReplay_GapFreeWithinRunID` publishes interleaved events with two distinct `RunID`s on the same session; replay extracts each `RunID`'s events and asserts no holes (every consecutive `Sequence` for that `RunID` appears).
- [ ] **No duplicates after subscribe-then-replay:** `TestReplay_NoDuplicatesWithLiveSubscribe` opens a `Subscribe`, drains N events, captures the last cursor, opens a fresh `Subscribe`, and calls `Replay(cursor, ...)` — the union of (already-drained, replay snapshot, new live tail) contains every published event exactly once. The test models the "client reconnects after a disconnect" case the brief calls out.
- [ ] **Ring overrun → ErrCursorTooOld:** `TestReplay_RingOverrun_ErrCursorTooOld` publishes more than `ReplayBufferSize` events, then asks for a cursor older than the ring tail; asserts the error and the wrapped (`oldest, requested`) numbers.
- [ ] **Replay disabled:** `TestReplay_DisabledByConfig_ErrReplayUnavailable` opens a bus with `ReplayBufferSize=0`; asserts `ErrReplayUnavailable` is returned immediately and that nothing was retained.
- [ ] **Integration test:** `test/integration/replay_test.go` wires real config + audit + events + telemetry/eventbus and runs `Logger.Error` → `Replay` (after Subscribe-cancel) → `Subscribe` → live tail; asserts the full sequence appears with no duplicates, no gaps within `RunID`, and identity-mandatory rejection on a malformed cursor. Real drivers everywhere; no mocks at the seam (per AGENTS.md §17).
- [ ] **Goroutine leak test:** `TestReplay_NoGoroutineLeak_AfterClose` asserts `runtime.NumGoroutine` returns to baseline within 2s of `Close(ctx)` for: idle bus with replay configured, bus with active replay-and-publish, bus with `Replay` mid-call when `Close` is invoked.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `phase-06.sh` smoke script runs `go test -race -run "TestReplay" ./internal/events/...` against the built tree (Go package only; HTTP surface check still SKIPs).

## Files added or changed

```text
internal/events/events.go               # Cursor, Replayer iface, ErrCursorTooOld, ErrReplayUnavailable
internal/events/drivers/inmem/inmem.go  # ring buffer, Replay implementation
internal/events/drivers/inmem/inmem_test.go  # new TestReplay_* tests
internal/config/config.go               # EventsConfig.ReplayBufferSize
internal/config/validate.go             # validator entry
test/integration/replay_test.go         # cross-subsystem integration test
scripts/smoke/phase-06.sh               # smoke skeleton (Go-package SKIP + go test invocation)
docs/decisions.md                       # D-029 — Replay returns []Event, not Subscription (departure from brief 06 §2)
docs/plans/README.md                    # Status: 06 → Shipped (in the implementation PR, not this plan PR)
```

## Public API surface

```go
package events

// Cursor identifies the last event a subscriber has consumed for a session.
// Sequence is the per-bus monotonic value assigned by Publish; SessionID
// scopes the cursor so two subscribers on different sessions can use the
// same numeric Sequence without collision.
type Cursor struct {
    SessionID string
    Sequence  uint64
}

// Replayer is the optional capability interface drivers may implement to
// support replay-from-cursor. EventBus.Subscribe + Replayer.Replay together
// give the caller a "resume cleanly after disconnect" pattern: drain
// Replay's snapshot, then live-tail a fresh Subscribe.
//
// A driver that does not implement Replayer (or for which the caller passed
// EventsConfig.ReplayBufferSize=0) returns ErrReplayUnavailable.
type Replayer interface {
    // Replay returns events whose Sequence > from.Sequence and that match
    // the filter, in Sequence order. The returned slice is owned by the
    // caller. See ErrCursorTooOld and ErrReplayUnavailable for failure
    // modes; (nil, nil) is the "nothing newer to replay" case.
    Replay(ctx context.Context, from Cursor, f Filter) ([]Event, error)
}

var (
    // ErrCursorTooOld — the requested cursor's Sequence is older than the
    // ring's oldest retained entry. Wraps a "(oldest, requested)" detail
    // for callers that fall through to a durable log.
    ErrCursorTooOld = errors.New("events: cursor older than ring tail")
    // ErrReplayUnavailable — replay is disabled on this driver
    // (EventsConfig.ReplayBufferSize=0) or the driver does not implement
    // Replayer at all.
    ErrReplayUnavailable = errors.New("events: replay not available on this driver")
)
```

## Test plan

- **Unit:** `TestReplay_HappyPath_ReturnsRequestedRange`, `TestReplay_ZeroCursor_ReturnsEntireRing`, `TestReplay_HeadCursor_ReturnsNilNil`, `TestReplay_FilterApplied`, `TestReplay_DisabledByConfig_ErrReplayUnavailable`, `TestReplay_RingOverrun_ErrCursorTooOld`, `TestReplay_RejectsEmptyFilter`, `TestCursor_OrderingInvariant`.
- **Integration:** `test/integration/replay_test.go` per AGENTS.md §17 — real audit + events + telemetry + eventbus drivers, identity propagation, ≥1 failure mode (`ErrCursorTooOld` after forced overrun), under `-race`. Wires Logger.Error → Subscribe → Cancel → Replay → fresh Subscribe → live-tail; asserts the full event set appears exactly once.
- **Conformance:** N/A — no driver pluralism for replay at Phase 06 (the durable-log driver in Phase 57 will share a separate replay conformance suite).
- **Concurrency / leak:** `TestReplay_ConcurrentReuse_ReuseContract` (≥100 goroutines, mixed Publish/Replay/Subscribe, under `-race`, baseline goroutines restored after Close — D-025); `TestReplay_NoGoroutineLeak_AfterClose` per the standard leak pattern.

## Smoke script additions

- `phase-06.sh`: Go-package-only phase. Runs `go test -race -run "TestReplay" ./internal/events/...` against the built tree, asserts the Replay tests pass; the HTTP/Protocol surface check still SKIPs (no Protocol layer until Phase 58+). Mirrors phase-05.sh's style — same shape, different test prefix.

## Coverage target

- `internal/events`: 85%
- `internal/events/drivers/inmem`: 85% (new replay paths covered; existing Phase 05 paths unchanged)
- `internal/config`: not regressed by the new field

## Dependencies

- 05 (Phase 05 — Event taxonomy + InMem `EventBus` + isolation)

## Risks / open questions

- **Ring sizing default.** 10k matches the brief; production workloads may want higher. The knob is operator-tunable, but a single-tenant Console under burst load could quickly exhaust it. Phase 57 (durable log) is the long-term answer; until then, operators monitor `bus.dropped` (per-subscriber) and the (post-Phase-56) replay-too-old metric. Not a blocker for V1; documented in the phase plan and surfaces back if Phase 57 schedule slips.
- **Memory pressure.** A ring of 10k events × ~1 KB per redacted payload = ~10 MB per bus. Acceptable for V1 (one bus per Runtime process); becomes interesting if Phase 86 (durable distributed bus driver, post-V1) scales fan-out. Documented in the plan but not gated.
- **No `RunID` indexing in the ring.** Replay's gap-free-within-RunID guarantee comes from `Sequence` ordering, not from a per-`RunID` index. If a future phase needs `Replay(filterByRunID)`, the ring will need a secondary index. Out of scope here; Phase 60 (Protocol wire transport) is where Console-side per-run debugging would surface the need.

## Glossary additions

- **Replayer capability interface.** Optional capability a driver may implement to support replay-from-cursor; callers type-assert `bus.(events.Replayer)`.
- **Cursor.** `(SessionID, Sequence)` pair identifying the last event a subscriber has consumed. Used by `Replayer.Replay` to compute "events strictly newer than this."
- **Ring buffer (events).** In-memory bounded retention queue inside the `inmem` driver; default 10k events. Eviction is drop-oldest. Distinct from per-subscriber buffers (which use drop-oldest with a `bus.dropped` notice).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (`TestReplay_CrossTenant_Isolation`)
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. — `TestReplay_ConcurrentReuse_ReuseContract`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. — `test/integration/replay_test.go`.
- [ ] If new vocabulary: glossary updated (Replayer, Cursor, ring buffer)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-029)
