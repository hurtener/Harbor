# Phase 172 — `events.aggregate` origin-anchored (epoch-aligned) bucket grid

## Summary

`events.aggregate` lays its bucket grid from the wall-clock instant at handler entry
(`windowStart := now.Add(-req.Window)`), so two calls at two instants return two
different bucket-boundary sets. A `bucket_start` is therefore not addressable twice and
no consumer can legally cache an aggregate bucket. This phase adds an OPTIONAL
origin/epoch anchor to `EventAggregateRequest`: when present, the aggregator floors
bucket boundaries onto a fixed grid anchored at the given instant (passing the Unix epoch
yields a globally-shared grid); absent, the response keeps today's clock-anchored
behaviour so no existing caller changes. A cold N-bucket fill becomes one call and every
returned bucket is a coordinate the consumer can re-request.

## RFC anchor

- RFC §6.13
- RFC §5.2
- RFC §7

## Briefs informing this phase

- brief 06
- brief 11

## Brief findings incorporated

- **brief 06 §5 (runtime owns the index; expose a search/read method):** the bucket grid
  is aggregation POLICY the runtime owns; making its boundaries deterministic and
  addressable is a read-surface property, not a driver property. The anchor lives on the
  aggregator, over the same substrate the durable-parity fix (171) established — no driver
  change.
- **brief 11 (Console events surface designed around a windowed replay/aggregate read):**
  the Events page renders a per-event-type stacked-area sparkline over `events.aggregate`;
  an addressable grid is what lets the page cache/stitch cold windows and re-request a
  bucket by coordinate instead of re-deriving the whole series on every poll.

## Findings I'm departing from (if any)

None.

## Goals

- Add an optional origin/epoch anchor to `EventAggregateRequest` so bucket boundaries can
  be floored onto a fixed grid (`anchor + k·Bucket`) instead of `now - Window`.
- Absent anchor ⇒ byte-identical to today's clock-anchored behaviour (backward
  compatible; no existing caller changes).
- Every returned `bucket_start` under an anchor is a stable coordinate: two calls at two
  instants with the same anchor + bucket share boundary instants, so a bucket is
  re-requestable and cacheable.
- A cold N-bucket fill (a fresh page loading a wide history window) is ONE call whose
  buckets align to the grid the next poll will also use.

## Non-goals

- No change to bucket CONTENTS, redaction, retention, or identity axes.
- No `ProtocolVersion` bump — the field is additive (stays 0.1.0).
- No per-tenant attribution (Phase 173 / D-307).
- No new method — this is an additive field on the existing request/response types.

## Acceptance criteria

- [ ] `EventAggregateRequest` gains a TRULY-OPTIONAL anchor field. Design: `Anchor *time.Time`
      (`json:"anchor,omitempty"`) — `omitempty` is a no-op on a `time.Time` STRUCT value (it
      would always serialize the zero time and force a non-optional TS mirror), so a pointer
      (or an int64 epoch) is required for real optionality (NIT). `nil` ⇒ today's now-anchored
      grid. Passing the Unix epoch (`1970-01-01T00:00:00Z`) yields a globally-shared grid.
- [ ] When the anchor is set, bucket boundaries are `anchor + k·Bucket` floored so the
      response covers the `Window`'s worth of buckets aligned to that grid; the existing
      `Window > 0`, `Bucket > 0`, `Window % Bucket == 0` validation is unchanged.
- [ ] Two aggregate calls at two different `now` instants, with the same anchor + window +
      bucket, return bucket series whose `bucket_start`/`bucket_end` instants are drawn
      from the same grid (a bucket is addressable twice) — pinned by a controllable-clock
      unit test.
- [ ] An anchored request whose grid does not divide evenly (should be impossible given
      `Window % Bucket == 0`, but defensively) never yields a fractional trailing bucket.
- [ ] Absent anchor: the response is byte-identical to the pre-172 behaviour (a golden
      test over a fixed clock).
- [ ] The epoch anchor never WIDENS the read span beyond the requested `Window` (grid
      boundaries only re-phase; the span extends by at most one bucket width at the edges)
      and never surfaces a fenced/erased session's counts (the substrate's fence check is
      upstream of bucketing) — pinned by a unit test. (NIT-5.)
- [ ] Composes with 171: the anchored grid works on BOTH drivers over the durable-parity
      substrate; the integration test extends 171's to assert an anchored request returns
      an aligned series on durable.
- [ ] Full D-223 lockstep + D-209 regen committed in the same PR (enumerated below).

## Files added or changed

```text
internal/protocol/types/events.go                    # + Anchor *time.Time on EventAggregateRequest (godoc: anchor semantics, epoch note, grid-edge note)
internal/events/aggregate.go                          # floor windowStart/boundaries onto the anchor grid when set
internal/events/aggregate_test.go                     # controllable-clock addressability + absent-anchor golden tests
web/console/src/lib/protocol/events.ts (or protocol.ts) # mirror the new wire field by hand (D-223)
web/console/src/lib/protocol/wire-manifest.gen.json    # regenerated via `make protocol-ts-gen` (never hand-edited)
web/console/src/lib/events/history.svelte.ts (or the aggregate consumer) # pass the anchor from the window picker; cache by coordinate
docs/site/protocol/types.md                            # regenerated via `make protocol-docs-gen` (D-209)
docs/skills/use-the-harbor-protocol/SKILL.md           # §18 — the aggregate request wire-shape gained a field
test/integration/events_aggregate_durable_test.go      # extend: anchored grid aligns on durable
scripts/smoke/phase-172.sh                             # live-server anchored-grid addressability assertion
docs/decisions.md                                      # D-306
docs/glossary.md                                       # origin-anchored bucket grid
docs/plans/README.md                                   # row + detail block (Pending)
```

## Public API surface

- `EventAggregateRequest.Anchor *time.Time` (`json:"anchor,omitempty"`) — additive, truly
  optional (pointer, not a struct value — NIT); `nil` ⇒ unchanged clock-anchored behaviour.
  No response-shape change (the existing `EventBucket.Start/End` become grid coordinates when
  the anchor is set).
- Go-internal: the aggregator's boundary computation floors onto the anchor grid.

## D-223 lockstep touch points (a wire-type field change)

- No new method → `internal/protocol/methods/methods.go`'s three maps are NOT touched (a
  field addition is not a new method).
- The additive field is registered wherever the canonical `EventAggregateRequest` type is
  enumerated for the wire-type registry / single-source (`internal/protocol/types` is the
  single source).
- Hand-mirror the field into the Console per-page wire module
  (`web/console/src/lib/protocol/events.ts` + siblings) — no hand-rolled `fetch`.
- Run `make protocol-ts-gen` → regenerates `web/console/src/lib/protocol/wire-manifest.gen.json`
  (committed, never hand-edited); `npm run lint` (the TS-source scan) must pass.
- Run `make protocol-docs-gen` → regenerates `docs/site/protocol/types.md` (D-209).
- `make protocol-ts-gen-check` + `make protocol-docs-gen-check` are the three gates; run
  them before the final push.

## Test plan

- **Unit:** aggregate.go — anchored addressability across two clock instants; absent-anchor
  golden equivalence; epoch anchor yields the globally-shared grid; validation unchanged; the
  epoch anchor never WIDENS the read span beyond the requested `Window` (the grid only
  re-phases boundaries; it does not extend `[effectiveSince, effectiveUntil)` by more than one
  bucket width at the edges) and never surfaces a fenced/erased session's counts (the
  `HistoryReplayer` substrate's fence check is upstream of bucketing) — NIT-5.
- **Integration:** extend `test/integration/events_aggregate_durable_test.go` — an anchored
  request returns an aligned series on the durable driver (real StateStore); identity
  propagation unchanged; under `-race`.
- **Conformance:** the 171 method-parity aggregate leg runs with an anchor set and asserts
  inmem/durable agree on the anchored series.
- **Concurrency / leak:** the aggregator stays a compiled reusable artifact; the anchor is
  per-request (read from `req`, never stored on the aggregator) — the existing N≥100 `-race`
  run covers it (D-025).

## Smoke script additions

- `scripts/smoke/phase-172.sh` (`PREFLIGHT_REQUIRES: live-server`):
  1. Route probe (401 → mounted; 404/405/501 → SKIP).
  2. Two aggregate POSTs with the same `anchor` + window + bucket a short interval apart
     (dev token) → both 200, and at least one shared `bucket_start` instant across the two
     responses (the addressability proof).
  3. An aggregate POST with NO `anchor` → 200 with the same bucket count as today (the
     backward-compatible path).

## Coverage target

- `internal/events`: 85%.
- `internal/protocol/types` (the additive field): covered by the marshal/round-trip
  conformance already in place; no drop.

## Dependencies

- 171 (the aggregate method must work on the durable driver before its grid matters).
- 118 (D-223 lockstep machinery). Shipped.

## Risks / open questions

- **Anchor vs `{since, until, bucket}` shape.** The ask offered either. I chose the single
  additive `Anchor` field over explicit `{since,until}` boundaries because (a) it is the
  smallest additive surface and composes with the existing `Window`/`Bucket` pair; (b) the
  request's `Filter.Since/Until` already clamp the window, so explicit boundary fields
  would duplicate that semantics; (c) an anchor makes every `bucket_start` a coordinate
  without changing the response shape. Recorded in D-306.
- **Grid vs window edge.** With an anchor, the last bucket's `End` may not equal `now`
  (it equals the grid boundary covering `now`). This is intentional — addressability
  requires grid-aligned edges — and documented on the field's godoc so a rendering client
  does not treat the final bucket as "up to this instant."

## Glossary additions

- **origin-anchored bucket grid** — see `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] Multi-isolation paths unchanged (the anchor does not touch identity/scope) — N/A with
      this reason
- [ ] Reusable-artifact concurrent-reuse test passes (the anchor is per-request) — D-025
- [ ] Integration test extended (anchored grid on durable), real drivers, `-race` — §17.1
- [ ] Wire change: `make protocol-ts-gen` + `make protocol-docs-gen` run, regenerated
      artifacts committed; the two lockstep-check gates pass; `use-the-harbor-protocol`
      SKILL.md updated (§18)
- [ ] New vocabulary added to `docs/glossary.md`
