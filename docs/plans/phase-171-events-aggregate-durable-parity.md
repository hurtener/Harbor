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
  change WHAT a method returns (retention depth, an observed horizon) but NEVER WHETHER it
  works; differences are DATA, never a 500 and never a status fork. Concretely: a window
  too wide to count within the aggregation bound returns partial buckets with an additive
  `truncated: true` UNIFORMLY on both drivers — NOT a 400 on durable (unbounded log) while
  inmem (physically-bounded ring) returns 200 for the same request. This is the ONE
  additive wire field 171 introduces (see D-223 lockstep below); it is what makes the
  thesis implementable — a `truncated` flag is DATA, an `ErrAggregateWindowTooLarge → 400`
  would re-introduce the exact whether-it-works fork HA-20 exists to kill.

## Non-goals

- No change to redaction, retention horizons, bucket identity axes, or scope rules. HA-18
  is fundamentally "return on the durable driver."
- **Bucket contents change in exactly ONE honest way (own it, do not bury it):** the new
  `HistoryReplayer` substrate EXCLUDES bus-internal notice types
  (`IsBusInternalNotice` — `admin_scope_used`, `bus.dropped`, `audit.redaction_failed`,
  `bus.subscription_idle_closed`; `filter.go:301`, `durable.go:1129`), whereas today's
  `Replay(Filter{Admin:true})` + `MatchWire` path does NOT — so today's inmem aggregate
  COUNTS those notices into type buckets and self-pollutes (every admin Replay emits an
  `admin_scope_used` into the ring it then counts). Real-event-type counts are
  byte-identical old-vs-new; the ONLY delta is those four notice types. This is a
  latent-bug FIX, adopted intentionally (recorded in D-305), not silent normalization.
- No change to `Replayer.Replay`'s per-session reconnect contract (its session-less admin
  refusal on durable stays — it is correct for the SSE path).
- No epoch/origin bucket grid (that is Phase 172 / D-306) and no per-tenant attribution
  (that is Phase 173 / D-307). 171's only wire change is the additive
  `EventAggregateResponse.Truncated` field.
- No making inmem durable, and no normalizing retention depth across drivers — the
  divergence stays honest DATA (surfaced via `truncated`).

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
- [ ] The handler computes `widened` on the RAW, PRE-FOLD wire filter and passes it
      verbatim — byte-mirroring `events_list_handler.go:167-206`: `widened :=
      conv.RequiresAdminScope` (from `FilterFromWire` on the pre-fold filter), then the
      caller's triple is folded into elided axes, then the aggregator is called with
      `Admin: widened`. It MUST NOT re-derive widening from the POST-fold filter (a
      genuine cross-tenant read `{TenantIDs:[T2]}` with user/session folded to the caller
      has a complete folded triple → would evaluate `widened=false` → run un-audited AND
      un-fanned, returning the wrong intersection — worse than the over-audit this fixes).
      (WARN-2.)
- [ ] The aggregator threads the effective window onto the substrate query:
      `q.Filter.Since = effectiveSince`, `q.Filter.Until = effectiveUntil` on the
      `ListWindow`/fan-in call — NOT only the post-loop `windowStart/now` guard. Pinned by
      a test with an `Until`-clamped sub-window ("2h–1h ago") whose newest matches at/above
      the bound would otherwise be dropped as newer-than-`Until`. (W2.)
- [ ] A non-admin own-session aggregate emits ZERO `audit.admin_scope_used` events; a
      widened aggregate emits EXACTLY ONE per request. NOTE: the spurious emit fires TODAY
      on inmem for every aggregate (`inmem.go:640` — the hardcoded `Filter{Admin:true}`),
      so this regression test guards a REAL current defect. (NIT-3.)
- [ ] A window too wide to count within the aggregation bound returns the partial buckets
      WITH `EventAggregateResponse.Truncated = true`, UNIFORMLY on both drivers (inmem sets
      it when its ring evicted below the requested window; durable sets it when the scan hit
      the bound) — NEVER a 400, never a silent undercount (§13). There is NO
      `ErrAggregateWindowTooLarge → 400` path. (F1.)
- [ ] Bucket contents: real-event-type counts are byte-identical old-vs-new; a published
      bus-internal notice-TYPE event (e.g. `admin_scope_used`) is EXCLUDED from the new
      aggregate — pinned by a guarded old-vs-new-inmem unit test. (W1.)
- [ ] New conformance scenario `Aggregate_Admin_SessionLess_FansInAcrossSessions` over a
      MULTI-TENANT fixture (≥2 tenants × ≥2 users × ≥2 sessions — the harness already
      seeds tenant A/B/C, `conformancetest.go:190-210`): an `events.Aggregator` built over
      each registered driver's bus fans in across sessions for an empty-triple + admin
      request; counts match across drivers AND the leg carries an explicit ISOLATION
      assertion — a widened admin read attributes to the CORRECT tenants and a non-admin
      read scopes to its own tuple (parity ≠ isolation; two drivers sharing a leak agree
      on the leaky answer). (WARN-1.)
- [ ] New method-parity conformance: `events.aggregate`, `events.list`,
      `events.subscribe`, and `state.history` run against every registered driver for the
      same request and return the same answer OR the same named-sentinel difference
      (never a 500, never a status fork).
- [ ] Registry gate: a test that BLANK-IMPORTS `internal/drivers/prod` (so
      `events.RegisteredDrivers()` is actually complete, not just what the test binary
      happened to import) enumerates the registered drivers and fails the build if any has
      no conformance run wired — a new events driver cannot ship without proving parity.
      (W3.)
- [ ] Legitimate driver differences stay DATA: replay-disabled → `ErrReplayUnavailable`;
      retention depth / ring eviction → `truncated: true`; the `Replay` vs `ListWindow`
      session-less-admin divergence pinned as a uniform named contract. No difference
      surfaces as a 500 or a status-code fork.
- [ ] Wire: the ONLY wire change is the additive `EventAggregateResponse.Truncated bool`;
      no method/error/event, no request-shape change, `ProtocolVersion` stays 0.1.0. Full
      D-223 lockstep + D-209 regen committed (enumerated below). No change under
      `internal/protocol/methods/`.

## Files added or changed

```text
internal/protocol/types/events.go                    # + Truncated bool on EventAggregateResponse (the one additive field)
internal/events/aggregate.go                          # source snapshot from HistoryReplayer fan-in; thread widened + effective window; set Truncated at the bound
internal/events/aggregate_test.go                     # durable-parity + widened-audit-once + truncated-uniformity + notice-exclusion + Until-clamp unit tests
internal/protocol/transports/stream/handlers.go       # AggregateHandler computes widened on the raw pre-fold filter; threads it into Aggregate
internal/protocol/transports/stream/handlers_test.go  # non-admin no-audit / widened one-audit / pre-fold-widening handler tests
internal/events/conformancetest/conformancetest.go    # + Aggregate_Admin_SessionLess (multi-tenant + isolation); + method-parity legs; Harness gains an Aggregator+ArtifactStore seam
internal/events/registryconformance_test.go           # NEW: registry-parity gate, blank-imports internal/drivers/prod
internal/events/drivers/inmem/conformance_test.go     # wire the new harness members
internal/events/drivers/durable/conformance_test.go   # wire the new harness members (real StateStore at the seam)
web/console/src/lib/protocol/events.ts (or protocol.ts) # mirror the additive Truncated field by hand (D-223)
web/console/src/lib/protocol/wire-manifest.gen.json     # regenerated via `make protocol-ts-gen` (never hand-edited)
docs/site/protocol/types.md                             # regenerated via `make protocol-docs-gen` (D-209)
docs/skills/use-the-harbor-protocol/SKILL.md            # §18 — the aggregate RESPONSE wire shape gained a field
test/integration/events_aggregate_durable_test.go     # NEW: aggregate 200-not-500 on durable + identity propagation + failure mode + truncated-at-bound
scripts/smoke/phase-171.sh                            # live-server aggregate regression guard + truncated-uniformity assertion
docs/decisions.md                                     # D-305
docs/glossary.md                                      # events-driver conformance parity
docs/plans/README.md                                  # row + detail block (Pending)
```

## Public API surface

- **Wire (additive):** `EventAggregateResponse.Truncated bool` (`json:"truncated,omitempty"`) —
  the honest "the counts are partial" signal, uniform across drivers (ring eviction below
  the window, or the scan hitting the aggregation bound). `ProtocolVersion` stays 0.1.0.
  No request-shape change, no new method/error/event.
- Go-internal: `events.Aggregator.Aggregate` gains a server-derived widening input
  (a Go parameter or an internal, non-wire query field — NEVER a wire field, per D-299).
  Signature shape (implementor finalizes): `Aggregate(ctx, req prototypes.EventAggregateRequest, widened bool) (prototypes.EventAggregateResponse, error)`.
- Go-internal (conformance): `conformancetest.Harness` gains the seam needed to build an
  `events.Aggregator` (and, for the method-parity legs, the `state.history`/`events.list`
  handler cores) over each driver's bus. `conformancetest.Run` gains the new scenarios.

## D-223 lockstep touch points (the one additive response field)

- No new method → `internal/protocol/methods/methods.go`'s three maps are NOT touched.
- Register the additive `Truncated` field on the canonical `EventAggregateResponse` in the
  `internal/protocol/types` single source.
- Hand-mirror the field into the Console per-page wire module
  (`web/console/src/lib/protocol/events.ts` + siblings) — no hand-rolled `fetch`.
- Run `make protocol-ts-gen` → regenerate `wire-manifest.gen.json` (committed, never
  hand-edited); `npm run lint` (the TS-source scan) passes.
- Run `make protocol-docs-gen` → regenerate `docs/site/protocol/types.md` (D-209).
- `make protocol-ts-gen-check` + `make protocol-docs-gen-check` gate before final push.
- §18: `docs/skills/use-the-harbor-protocol/SKILL.md` — the aggregate response wire shape
  gained a field.

## Test plan

- **Unit:** aggregate.go — session-less admin fan-in returns non-empty over the durable
  substrate; non-admin scopes to own triple; the widened path emits one
  `audit.admin_scope_used`, the non-admin path emits none (guarding the real inmem defect,
  NIT-3); a window past the bound returns partial buckets with `Truncated=true` (never a
  400), UNIFORMLY on inmem and durable (F1); a bus-internal notice-TYPE event is EXCLUDED
  old-vs-new (W1); an `Until`-clamped sub-window at/above the bound is counted correctly
  because `Since/Until` are threaded onto the substrate query (W2); `ErrReplayUnavailable`
  on a bus without `HistoryReplayer`.
- **Integration:** `test/integration/events_aggregate_durable_test.go` — real durable
  driver + REAL inmem StateStore at the seam (`§17.8`, mirroring the durable conformance
  harness). Assert: (1) `events.aggregate` returns 200 with a correct series where it
  previously 500'd; (2) identity propagates — a non-admin caller sees only its own tuple's
  counts, a widened caller (verified scope) fans in across the correct tenants' sessions;
  (3) ≥1 failure mode — a cross-tenant filter WITHOUT the elevated scope is rejected (not
  counted); (4) a window past the bound returns `truncated=true` on durable. Under `-race`.
- **Conformance:** `Aggregate_Admin_SessionLess_FansInAcrossSessions` over the multi-tenant
  fixture (≥2×2×2) with an explicit isolation assertion (WARN-1); method-parity for the
  four event-read methods across every registered driver; the `Replay` vs `ListWindow`
  session-less-admin divergence pinned as a uniform named contract; the retention-depth /
  `truncated` differences pinned as DATA. All run for inmem AND durable (durable with a
  real StateStore). Registry gate blank-imports `internal/drivers/prod` (W3).
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
  4. Static (always-run) guards: the additive-field invariant — grep asserts the aggregate
     surface added ONLY `EventAggregateResponse.Truncated` (no method/error/event, no
     request-shape field) in `internal/protocol/types/events.go`; single-source
     method-string check; no Console import in the stream package.

## Coverage target

- `internal/events`: 85% (raise-only from current).
- `internal/protocol/transports/stream` (aggregate handler paths touched): 85%.

## Dependencies

- 72a (`events.aggregate` + the aggregator), 162 (`events.list` / the `HistoryReplayer`
  windowed fan-in this reuses; D-294), 124 (gap-free durable log), 125 (`state.history`
  substrate; D-254), 118 (D-223 lockstep machinery, for the one additive `Truncated`
  field). All shipped.

## Risks / open questions

- **The aggregation bound → `truncated`, not a 400 (F1, resolved).** Sourcing the whole
  window in one `ListWindow` call (one call ⇒ one audit ⇒ full-window correctness) requires
  a generous match bound; today's aggregator already materializes the whole ring, so the
  memory profile is unchanged. A window exceeding the bound returns the partial buckets
  with the additive `EventAggregateResponse.Truncated = true` — UNIFORMLY on both drivers,
  never a 400. The earlier draft's `ErrAggregateWindowTooLarge → 400` was WRONG: because
  `EventAggregateResponse` had no partial signal and 171 was declared zero-wire, the
  over-bound case was forced to a 400 that fires on durable (unbounded log) but never on
  inmem (physically-bounded ring) for the SAME request — the exact "a driver difference
  changes WHETHER it works" fork HA-20 exists to kill, and the four-method parity contract
  (same answer OR same named sentinel) cannot reconcile 200-vs-400. The fix is one additive
  field; 171 is therefore NOT zero-wire (it runs the D-223/D-209 lockstep for the single
  `Truncated` field). **Rejected alternatives:** (a) page `ListWindow` to completion —
  correct counts but emits N `audit.admin_scope_used` events per aggregate (a §6 audit
  amplification); (b) a dedicated unbounded scan member on `HistoryReplayer` — heavier
  interface surface, same memory profile; (c) record the bound as an accepted divergence in
  D-305 — rejected because a status-code divergence is precisely the thesis violation, and
  the additive `truncated` flag is the DATA-not-500 discipline the whole track rests on.
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
- [ ] Wire change (the single additive `EventAggregateResponse.Truncated`): `make
      protocol-ts-gen` + `make protocol-docs-gen` run, regenerated artifacts committed; the
      two lockstep-check gates pass; `use-the-harbor-protocol` SKILL.md updated (§18); no
      diff under `internal/protocol/methods` (no new method)
