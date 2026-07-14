# Phase 171 — `events.aggregate` durable-driver parity + the events-driver conformance matrix closure

## Summary

Two things land together, exactly the D-283 pattern (fix the instance AND make its
class mechanically impossible to reintroduce, same PR). **HA-18:** `events.aggregate`
returns HTTP 500 on the durable driver on every call, because the aggregator replays
through the per-session `Replayer.Replay` path with a session-less `Filter{Admin:true}`,
which the durable driver correctly refuses (`ErrIdentityScopeRequired`), while the inmem
driver honours it — so the method works in dev and 500s in prod. **HA-20:** the
events-driver conformance matrix never exercises the session-less admin read the
aggregator depends on, and never runs the `events.aggregate` Protocol method against
every registered driver at all — the hole that let HA-18 ship green.

## RFC anchor

- RFC §6.13
- RFC §5.2
- RFC §6.5
- RFC §4
- RFC §7

<!-- Note: the downstream asks referenced a high-numbered RFC section that does not
     exist in RFC-001 (the RFC tops out in the §6.x/§7 range). The events subsystem is
     RFC §6.13; the Protocol read surface is §5.2; the identity/elevated-scope rules are
     §4/§6.5; the Console lens is §7. Those are the real anchors and the drift-audit
     resolves them. See the coordination note's open-questions section. -->

## Briefs informing this phase

- brief 06
- brief 07

## Brief findings incorporated

- **brief 06 §5 (the one-bus lesson):** "the replay/read channel must be the SAME
  records the live channel fans out, or every consumer fuses two paths forever." HA-18
  is precisely a two-paths divergence: `events.aggregate` reads through one substrate
  (`Replayer.Replay`) while `events.list` reads through another
  (`HistoryReplayer.ListWindow`), and only the latter works session-less on durable. The
  fix moves the aggregator onto the SAME cross-session windowed substrate `events.list`
  uses, so aggregate and list agree by construction on "what a session-less admin read
  means."
- **brief 06 (fail-loudly runtime principle):** a driver difference may change WHAT a
  method returns (retention depth, an observed horizon) but must NEVER change WHETHER it
  works. HA-18 is the anti-pattern — an internal 500 that is really "this driver refuses
  the filter the aggregator hands it." Legitimate differences stay named sentinels
  (`ErrReplayUnavailable`) or data flags (`truncated`), never a 500 and never normalized
  away.
- **brief 07 (runtime owns the index; the LLM/consumer is not the runner):** the
  aggregator's cross-session count is a runtime-side index read; it must not push
  aggregation policy (bucket grid, tenant dimension) down into the driver. The driver
  returns matching events; the aggregator buckets. This keeps 172/173 additive over the
  same substrate.

## Findings I'm departing from (if any)

- **The downstream ask named the new conformance scenario `Replay_Admin_SessionLess_FansInAcrossSessions`.**
  I depart from that literal name. After the HA-18 fix the aggregator no longer depends
  on `Replayer.Replay` for the session-less admin read — it depends on the
  `HistoryReplayer` windowed fan-in (`ListWindow`), whose session-less admin fan-in is
  ALREADY conformance-pinned (`ListWindow_Admin_FansInAcrossSessions`). Making
  `Replayer.Replay` itself fan in session-less on durable would add a code path with no
  live consumer (Replay is the per-session SSE-reconnect path; no reconnect is
  session-less) — a §13 "primitive without a consumer" smell. So the real matrix hole is
  one level up: the `events.aggregate` Protocol surface is not parametrized by driver at
  all. I close it with a driver-parametrized aggregate scenario
  (`Aggregate_Admin_SessionLess_FansInAcrossSessions`) plus method-level parity across
  the four event-read methods, and I additionally pin the `Replay` vs `ListWindow`
  session-less-admin divergence as a UNIFORM named contract so it is DATA, not a silent
  "whether it works" fork. This is a name change and a granularity change, not a scope
  change — it closes the same class the ask named. Recorded in D-305.

## Goals

- `events.aggregate` returns a correct time-bucketed response on the durable driver for
  every call it already served on inmem — same request, same answer, never a 500.
- `events.aggregate` and `events.list` agree, by construction, on what a session-less
  admin (fleet) read means: both source the same cross-session `HistoryReplayer`
  windowed fan-in over the persisted global-sequence log.
- Authority stays server-derived: the handler's verified `widened` decision (D-299 —
  cross-principal OR multi-value fan-in) is threaded into the aggregator; it is never
  read from the request body. This also fixes a latent audit-integrity bug: today the
  aggregator hardcodes `Filter{Admin:true}`, so a non-admin own-session aggregate would
  emit `audit.admin_scope_used` if it reached the durable fan-in.
- Exactly ONE `audit.admin_scope_used` per widened aggregate request (parity with
  `events.list`).
- The events-driver conformance matrix makes HA-18's class impossible to reintroduce: a
  driver that self-registers but cannot serve the session-less admin aggregate — or that
  serves a DIFFERENT answer than a sibling driver — fails the build.
- The driver-parity thesis is encoded as an executable contract: a driver difference may
  change WHAT a method returns (depth via `truncated`, observed horizon) but NEVER
  WHETHER it works; differences are DATA / named sentinels, never a 500.

## Non-goals

- No change to bucket CONTENTS, redaction, retention horizons, bucket identity axes, or
  scope rules. HA-18 is ONLY "return on the durable driver."
- No change to `Replayer.Replay`'s per-session reconnect contract (its session-less admin
  refusal on durable stays — it is correct for the SSE path).
- No epoch/origin bucket grid (that is Phase 172 / D-306) and no per-tenant attribution
  (that is Phase 173 / D-307). This phase is zero-wire.
- No making inmem durable, and no normalizing retention depth across drivers — the
  divergence stays honest DATA.

## Acceptance criteria

- [ ] `events.aggregate` on the durable driver returns HTTP 200 with a well-formed
      `buckets` series for a request that today 500s; an integration test proves it
      against a REAL StateStore (`§17.1`, `§17.8`).
- [ ] For the SAME session-less admin request, `events.aggregate` returns bucket totals
      that agree between the inmem and durable drivers (the method-parity conformance
      leg).
- [ ] The aggregator sources events via the `HistoryReplayer` windowed fan-in (the same
      substrate `events.list` uses), NOT via `Replayer.Replay`. A bus that does not
      implement `HistoryReplayer` yields `ErrReplayUnavailable` (loud; classified 500 as
      today — "no historical substrate"), never a silent empty series.
- [ ] The handler threads its server-derived `widened` decision into the aggregator; a
      non-admin own-session aggregate emits ZERO `audit.admin_scope_used` events; a
      widened aggregate emits EXACTLY ONE per request.
- [ ] A window whose matching-event count exceeds the aggregation cap fails loud with a
      named sentinel (`ErrAggregateWindowTooLarge` → `CodeInvalidRequest` / 400,
      "narrow the window"), NEVER a silent undercount (§13).
- [ ] New conformance scenario `Aggregate_Admin_SessionLess_FansInAcrossSessions`: an
      `events.Aggregator` built over each registered driver's bus fans in across sessions
      for an empty-triple + admin request; counts match across drivers.
- [ ] New method-parity conformance: `events.aggregate`, `events.list`,
      `events.subscribe`, and `state.history` run against every registered driver for the
      same request and return the same answer OR the same named-sentinel difference
      (never a 500).
- [ ] Registry gate: a test enumerates `events.RegisteredDrivers()` and fails the build
      if any registered driver has no conformance run wired — a new events driver cannot
      ship without proving parity.
- [ ] Legitimate driver differences stay DATA: replay-disabled → `ErrReplayUnavailable`;
      ring eviction → `truncated: true`; the `Replay` vs `ListWindow` session-less-admin
      divergence is pinned as a uniform named contract. No difference surfaces as a 500.
- [ ] `git diff` shows NO change under `internal/protocol/types/`,
      `internal/protocol/methods/`, or `web/console/src/lib/protocol/` — this phase is
      zero-wire (the manifest diff is a red flag).

## Files added or changed

```text
internal/events/aggregate.go                         # source snapshot from HistoryReplayer fan-in; thread widened; cap sentinel
internal/events/aggregate_test.go                    # durable-parity + widened-audit-once + cap unit tests
internal/protocol/transports/stream/handlers.go      # AggregateHandler threads server-derived `widened` into Aggregate
internal/protocol/transports/stream/handlers_test.go # non-admin no-audit / widened one-audit handler tests
internal/events/conformancetest/conformancetest.go   # + Aggregate_Admin_SessionLess_FansInAcrossSessions; + method-parity legs; Harness gains an Aggregator+ArtifactStore seam
internal/events/registryconformance_test.go          # NEW (or test/integration/): registry-parity gate over RegisteredDrivers()
internal/events/drivers/inmem/conformance_test.go    # wire the new harness members
internal/events/drivers/durable/conformance_test.go  # wire the new harness members (real StateStore at the seam)
test/integration/events_aggregate_durable_test.go    # NEW: aggregate 200-not-500 on durable + identity propagation + failure mode
scripts/smoke/phase-171.sh                           # live-server aggregate regression guard + zero-wire static guards
docs/decisions.md                                    # D-305
docs/glossary.md                                     # events-driver conformance parity
docs/plans/README.md                                 # row + detail block (Pending)
```

## Public API surface

- No Protocol/wire surface change.
- Go-internal: `events.Aggregator.Aggregate` gains a server-derived widening input
  (a Go parameter or an internal, non-wire query field — NEVER a wire field, per D-299).
  Signature shape (implementor finalizes): `Aggregate(ctx, req prototypes.EventAggregateRequest, widened bool) (prototypes.EventAggregateResponse, error)`.
- Go-internal: a new sentinel `events.ErrAggregateWindowTooLarge`.
- Go-internal (conformance): `conformancetest.Harness` gains the seam needed to build an
  `events.Aggregator` (and, for the method-parity legs, the `state.history`/`events.list`
  handler cores) over each driver's bus. `conformancetest.Run` gains the new scenarios.

## Test plan

- **Unit:** aggregate.go — session-less admin fan-in returns non-empty over the durable
  substrate; non-admin scopes to own triple; the widened path emits one
  `audit.admin_scope_used`; the non-admin path emits none; a window past the cap returns
  `ErrAggregateWindowTooLarge`; `ErrReplayUnavailable` on a bus without `HistoryReplayer`.
- **Integration:** `test/integration/events_aggregate_durable_test.go` — real durable
  driver + REAL inmem StateStore at the seam (`§17.8`, mirroring the durable conformance
  harness). Assert: (1) `events.aggregate` returns 200 with a correct series where it
  previously 500'd; (2) identity propagates — a non-admin caller sees only its own tuple's
  counts, a widened caller (verified scope) fans in across sessions; (3) ≥1 failure mode —
  a cross-tenant filter WITHOUT the elevated scope is rejected (not counted). Under `-race`.
- **Conformance:** `Aggregate_Admin_SessionLess_FansInAcrossSessions`; method-parity for
  the four event-read methods across every registered driver; the `Replay` vs `ListWindow`
  session-less-admin divergence pinned as a uniform named contract; the retention-depth /
  `truncated` differences pinned as DATA. All run for inmem AND durable (durable with a
  real StateStore).
- **Concurrency / leak:** the aggregator remains a compiled reusable artifact (D-025); the
  existing `concurrent_test.go` N≥100 shared-instance `-race` run extends to cover the
  durable-substrate path.

## Smoke script additions

- `scripts/smoke/phase-171.sh` (`PREFLIGHT_REQUIRES: live-server`):
  1. Route probe: `POST /v1/events/aggregate` no-bearer → 401 (route mounted + identity
     gate); 404/405/501 → SKIP.
  2. With a dev token: a structurally-valid windowed aggregate returns 200 with a
     `buckets` array of the expected length — the regression guard that the method still
     works (preflight boots the inmem events driver; the durable-parity proof is the
     integration test, noted in-script).
  3. A cross-tenant aggregate body WITHOUT an elevated scope → 403
     `CodeIdentityScopeRequired`.
  4. Static (always-run) guards: the zero-wire invariant — grep asserts no new
     `events.aggregate` wire field appeared in `internal/protocol/types/events.go`
     beyond the current shape; single-source method-string check; no Console import in the
     stream package.

## Coverage target

- `internal/events`: 85% (raise-only from current).
- `internal/protocol/transports/stream` (aggregate handler paths touched): 85%.

## Dependencies

- 72a (`events.aggregate` + the aggregator), 162 (`events.list` / the `HistoryReplayer`
  windowed fan-in this reuses; D-294), 124 (gap-free durable log), 125 (`state.history`
  substrate; D-254). All shipped.

## Risks / open questions

- **The aggregation cap.** Sourcing the whole window in one `ListWindow` call (one call ⇒
  one audit ⇒ full-window correctness) requires a generous match cap; today's aggregator
  already materializes the whole ring, so the memory profile is unchanged. A window
  exceeding the cap fails loud (`ErrAggregateWindowTooLarge`) rather than silently
  undercounting. **Rejected alternatives:** (a) page `ListWindow` to completion —
  correct counts but emits N `audit.admin_scope_used` events per aggregate (a §6 audit
  amplification); (b) add a dedicated unbounded scan member to `HistoryReplayer` — heavier
  interface surface with the same memory profile as the cap approach. The single-call +
  loud-cap design is the most faithful to the ask's "reuse the `ListWindow` path."
- **Durable fleet candidate-gather is `O(events-below-cursor)`** (the HA-13 note recorded
  in D-294). Unchanged by this phase; the merged global-sequence index remains tracked
  separately.
- **The registry gate's harness construction.** Durable needs a real StateStore injected
  (`§17.8`), so the gate cannot be a fully-generic `OpenDriver`-only sweep. The gate lives
  in one test package that can construct each registered driver's harness (durable wired
  with a real inmem StateStore, exactly as `durable/conformance_test.go` does) and asserts
  the covered-driver set equals `events.RegisteredDrivers()`.

## Glossary additions

- **events-driver conformance parity** — see `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (the
      integration test's identity-propagation + cross-tenant-refusal legs)
- [ ] Reusable-artifact concurrent-reuse test passes (the aggregator's N≥100 `-race` run,
      extended to the durable substrate) — D-025
- [ ] Integration test exists (`test/integration/events_aggregate_durable_test.go`), wires
      real drivers + a real StateStore, asserts identity propagation, covers ≥1 failure
      mode, runs under `-race` — §17.1
- [ ] New vocabulary added to `docs/glossary.md`
- [ ] Zero-wire verified: no diff under `internal/protocol/types|methods` or
      `web/console/src/lib/protocol`
