# Phase 57 — Durable event log driver (StateStore-backed)

## Summary

Ships `internal/events/drivers/durable` — an `EventBus` + `Replayer` driver
that persists every published `Event` through the Phase 07/15/16 `StateStore`,
keyed by `(SessionID, Sequence)`, so replay-from-cursor is exact and gap-free
across Runtime restarts. When no `StateStore` is configured the driver
auto-degrades to a best-effort in-memory ring buffer and emits a loud
`runtime.warning` so the lossy posture is never silent.

## RFC anchor

- RFC §6.13
- RFC §9
- RFC §6.11

## Briefs informing this phase

- brief 06
- brief 05

## Brief findings incorporated

- brief 06 §"Roadmap" item 4: "Durable event log driver. Backed by the
  StateStore subsystem (cross-fork dependency on brief 05's seam); persists
  `Event` records keyed by `(SessionID, Sequence)`." — adopted verbatim as the
  keying scheme.
- brief 06 §"Replay semantics" (line 122): "With a durable log driver attached
  (which uses the StateStore from research brief 05), replay is exact." — the
  durable driver's `Replay` reads from the `StateStore`, not a ring, so it is
  exact and survives restart.
- brief 06 §"Persistence" (line 183): "Without it, replay degrades to
  ring-buffer-only. The runtime ships a usable in-memory experience without
  StateStore." — adopted as the degradation behaviour, but per CLAUDE.md §13
  "no silent degradation" the degradation is made LOUD: a `runtime.warning`
  event is published on the bus at construction AND `slog.Warn` is emitted, so
  an operator running the driver without a `StateStore` is told their replay is
  best-effort.
- brief 05 §"StateStore" (the keyed-slot model): `StateStore.Save` overwrites at
  `(Identity, Kind)`; there is no list/scan method. The durable driver works
  within that contract by writing one record per event under a
  `Sequence`-suffixed `Kind` plus one mutable head record, and replays by
  loading the contiguous `[cursor+1, head]` range.

## Findings I'm departing from (if any)

None. The brief's "degrades to ring-buffer-only" is honoured but tightened to a
loud warning per CLAUDE.md §13 — that is a strengthening, not a departure, and
is recorded in D-074.

## Goals

- A `durable` `EventBus` driver that persists every published event through a
  `StateStore` and offers exact, gap-free replay-from-cursor that survives a
  Runtime restart.
- Replay across all three `StateStore` drivers (in-memory, SQLite, Postgres)
  with conformance parity.
- Loud auto-degradation to a best-effort ring buffer when no `StateStore` is
  configured.
- Full multi-isolation: every persisted record carries the identity triple;
  replay filters by identity exactly as `Subscribe` does.

## Non-goals

- No new `StateStore` driver — Phase 57 consumes the Phase 07/15/16 drivers
  as-is.
- No Protocol / HTTP surface — that is Phase 60. Correctness is gated by
  `go test -race` (the Phase 06 precedent).
- No change to the `events.EventBus` / `events.Replayer` interfaces — the
  durable driver implements the existing surface.
- No retention / compaction policy for the durable log — V1 keeps every event;
  a compaction policy is a post-V1 concern.
- No change to the inmem driver — it stays the ring-buffer reference.

## Acceptance criteria

- [ ] `internal/events/drivers/durable` registers a `durable` factory via
  `init()`; `events.Open` with `EventsConfig.Driver == "durable"` returns it.
- [ ] A late subscriber that calls `Replay` after the Runtime is torn down and
  rebuilt against the same `StateStore` sees every event with no gaps.
- [ ] `Replay` returns events strictly newer than the cursor, in `Sequence`
  order, filtered by identity — the same contract as the inmem `Replayer`.
- [ ] When no `StateStore` is configured the driver degrades to a best-effort
  ring buffer AND publishes a `runtime.warning` event AND logs `slog.Warn`.
- [ ] Integration test exercises the durable driver against all three
  `StateStore` drivers (in-mem / SQLite / Postgres).
- [ ] Every persisted record carries the identity triple; a cross-session
  replay attempt without admin scope is rejected with
  `ErrIdentityScopeRequired`; cross-tenant events never leak across a replay.
- [ ] Concurrent-reuse test: N≥100 concurrent publishers against one shared
  durable bus under `-race` — no races, no context bleed, no goroutine leak.
- [ ] `scripts/smoke/phase-57.sh` shows `OK ≥ 1` and `FAIL = 0`; prior phase
  smoke scripts still pass.

## Files added or changed

- `internal/events/drivers/durable/durable.go` — the driver (new)
- `internal/events/drivers/durable/record.go` — the persisted-record codec (new)
- `internal/events/drivers/durable/durable_test.go` — unit tests (new)
- `internal/events/drivers/durable/concurrent_test.go` — D-025 reuse test (new)
- `internal/config/config.go` — `EventsConfig.StateDriver` / `StateDSN` fields
- `internal/config/loader.go` — `validateEvents` accepts the new fields
- `cmd/harbor/main.go` — blank-import the durable driver
- `test/integration/durable_eventlog_test.go` — cross-driver integration (new)
- `scripts/smoke/phase-57.sh` — smoke assertions
- `examples/*.yaml` — document the new optional config keys
- `docs/decisions.md` — D-074
- `docs/glossary.md` — "Durable event log" term
- `README.md`, `docs/plans/README.md` — status flips

## Public API surface

```go
// Package internal/events/drivers/durable.
//
// Registered as the "durable" events driver. Implements
// events.EventBus + events.Replayer.
//
// New(cfg config.EventsConfig, r audit.Redactor, store state.StateStore,
//     opts ...Option) (events.EventBus, error)
//   store == nil  -> best-effort ring-buffer mode + loud warning.
//   store != nil  -> StateStore-backed exact replay.
//
// Option:
//   WithLogger(*slog.Logger) Option   // inject the warn sink (test seam)
//   WithClock(Clock) Option           // deterministic time (test seam)
```

The driver adds no new exported type to `internal/events` — it implements the
existing `EventBus` and `Replayer` interfaces.

## Test plan

- **Unit:** publish then persist round-trip; replay-from-cursor exactness;
  cursor at head returns nil; cursor older than the persisted tail in
  best-effort mode returns `ErrCursorTooOld`; identity-scope rejection on
  `Subscribe` and `Replay`; loud-degradation path (no store -> warning event +
  `slog.Warn`); a `StateStore.Save` failure surfaces loudly from `Publish` (no
  silent drop).
- **Integration:** `test/integration/durable_eventlog_test.go` —
  `TestE2E_Phase57_DurableReplay_AllStateDrivers` runs the
  publish then teardown then rebuild then replay-no-gaps scenario against
  in-mem, SQLite, and Postgres `StateStore` drivers (Postgres `t.Skip`s with a
  reason when no DSN is exported, mirroring the Phase 16 driver-test
  convention); identity propagation through every layer; at least one failure
  mode (a closed store mid-publish); cross-tenant isolation across replay.
- **Conformance:** the durable driver is exercised through the same scenario
  matrix across all three `StateStore` drivers — the cross-driver integration
  test IS the conformance gate for "durable replay behaves identically
  regardless of StateStore backend."
- **Concurrency / leak:** `concurrent_test.go` — N=120 concurrent publishers
  against one shared durable bus under `-race`; distinct per-goroutine identity
  quadruples (a context bleed surfaces as a foreign triple in the persisted
  record); baseline `runtime.NumGoroutine` restored after `Close`.

## Smoke script additions

- `scripts/smoke/phase-57.sh` runs `go test -race -run TestDurable` against
  `internal/events/drivers/durable/...` and `go test -race
  -run TestE2E_Phase57_` against `test/integration/...`, asserting both pass;
  it `skip`s the HTTP/Protocol surface (Phase 60+), matching the Phase 06
  smoke pattern.

## Coverage target

- `internal/events/drivers/durable`: 85%

## Dependencies

- Phase 05 (the `events.EventBus` / `Replayer` interfaces + registry)
- Phase 07 (`StateStore` interface + in-memory driver)
- Phase 15 (SQLite `StateStore` driver)
- Phase 16 (Postgres `StateStore` driver)

## Risks / open questions

- **Payload fidelity on replay.** `StateStore` stores opaque `[]byte`; the
  durable driver JSON-encodes the event and, on replay, reconstructs the
  payload as `events.RedactedMap` (the same generic post-redaction shape the
  inmem bus already produces for non-`SafePayload` types). Concrete typed
  payloads are NOT round-tripped — replay consumers read fields via
  `RedactedMap.Data`. This matches the existing redaction-boundary contract and
  is documented in D-074; revisit if a future consumer needs typed replay.
- **`StateStore` has no scan/list method.** The durable driver works within the
  keyed-slot contract by writing a per-event record under a `Sequence`-suffixed
  `Kind` plus one mutable head record; replay loads the contiguous
  `[cursor+1, head]` range. A torn write (event record persisted, head not
  advanced) is recoverable on the next publish and never produces a *gap* in a
  served replay — replay never reads past `head`.

## Glossary additions

- **Durable event log** — added to `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact: concurrent-reuse test passes**
  — `concurrent_test.go`, N=120 publishers against one shared bus under `-race`.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a
  cross-subsystem seam: an integration test exists** —
  `test/integration/durable_eventlog_test.go` wires the real `StateStore`
  drivers end-to-end.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry
  filed — none departed; D-074 records the loud-degradation strengthening.
