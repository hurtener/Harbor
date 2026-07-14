# Phase 173 — `events.aggregate` per-tenant attribution for admin-widened reads

## Summary

An aggregate bucket is a bag of scalars (`{"tool.invoked": 7}`) with NO tenant
attribution. Unlike a row read (`sessions.list` / `tasks.list` / `events.list`) where
every row carries its own tenant and a consumer can post-filter the merged result against
its entitled set, an aggregate consumer CANNOT verify an admin-widened count against the
`Filter.TenantIDs` it asked for — for an aggregate the runtime's honouring of the filter
IS the entire tenant boundary, a single point of enforcement with no downstream check.
This phase adds OPTIONAL, opt-in per-tenant attribution (a count per `(tenant,
event_type)`) FOR THE ADMIN-WIDENED reads that already require the elevated
admin/console:fleet scope set (D-299), returned ONLY for tenants the caller is already
entitled to. Existing callers are unchanged. This makes the isolation boundary
independently verifiable on aggregates the way it already is on rows (§6
defence-in-depth).

## RFC anchor

- RFC §6.13
- RFC §5.2
- RFC §6.5
- RFC §4
- RFC §7

## Briefs informing this phase

- brief 06
- brief 11

## Brief findings incorporated

- **brief 06 §5 (one bus, runtime owns the index):** attribution is a re-projection of the
  SAME counted events by their existing tenant identity — no new data path, no new
  identity axis. The events already carry `Identity.TenantID`; the aggregator already
  visits each matching event; the attribution is a second accumulator over the same pass.
- **brief 11 (fleet/console observability surface):** a `console:fleet` operator reading a
  widened aggregate needs to see WHICH tenants contributed, both to render a per-tenant
  breakdown and to self-check that the widened read returned only its entitled set —
  exactly the verifiability a row read gives for free.

## Findings I'm departing from (if any)

None.

## Goals

- Add opt-in per-tenant attribution to the aggregate response: a count per `(tenant,
  event_type)` alongside the existing totals.
- Attribution is returned ONLY for admin-widened reads (the verified admin/console:fleet
  scope set, derived server-side per D-299) AND ONLY for tenants the caller is already
  entitled to. An unelevated caller gains attribution for NOTHING it could not already
  read.
- Existing callers (no attribution flag) see byte-identical responses.
- The tenant boundary on an aggregate becomes independently verifiable by the consumer,
  the way it already is on row reads (§6 defence-in-depth).

## Non-goals

- No payloads and no new identity axes — a count per `(tenant, event_type)` suffices.
- No change to bucket time grid (171/172 own that), redaction, or retention.
- No elevation from the request body — authority stays server-derived (D-219/D-299).
- No `ProtocolVersion` bump (additive; stays 0.1.0).

## Acceptance criteria

- [x] `EventAggregateRequest` gains an opt-in attribution flag (design:
      `ByTenant bool`, `json:"by_tenant,omitempty"`). Absent/false ⇒ no attribution;
      response byte-identical to pre-173.
- [x] The response carries per-tenant attribution when `ByTenant` is set AND the read is
      admin-widened (design: `EventBucket.CountsByTenant map[string]map[string]int64`,
      tenant → event_type → count, `json:"counts_by_tenant,omitempty"`; the existing
      `Counts` totals are unchanged). Per-bucket so it composes with the time series and
      172's grid.
- [x] Concrete bound (NIT-4): the attribution keys are a SUBSET of the authorized
      (named-or-folded) `Filter.TenantIDs`, and `Counts` (totals) and `CountsByTenant` are
      scoped to the IDENTICAL set BY CONSTRUCTION (both computed from the same MatchWire
      pass over the same authorized filter) — so `Σ CountsByTenant == Counts` holds. There
      is NO per-tenant entitlement mechanism: `ScopeAdmin` / `ScopeConsoleFleet` are GLOBAL
      binary fan-in grants; the request body SELECTS the tenants and the scope AUTHORIZES
      the fan-in (no D-219 issue). A widened read naming `{T1,T2}` returns both; a request
      naming only `{T1}` returns only T1 (it never asked for T2) — attribution re-projects
      exactly the authorized filter, never widens it.
- [x] A non-admin (own-tenant, un-widened) read with `ByTenant` set returns at most the
      caller's OWN single tenant in `CountsByTenant` (no new information) — attribution is
      never a back-door to cross-tenant data.
- [x] Authority is server-derived: the handler passes its verified `widened` decision
      (171) into the aggregator; `ByTenant` alone (from the body) never elevates.
- [x] `Σ CountsByTenant[*][type] == Counts[type]` per bucket — the attribution reconciles
      exactly with the totals (a unit invariant; the verifiability property).
- [x] Multi-isolation test: two tenants' events aggregated under a widened read attribute
      to the correct tenants with no cross-talk; a caller entitled to one tenant sees only
      that tenant's attribution.
- [x] Full D-223 lockstep + D-209 regen committed in the same PR.

## Files added or changed

```text
internal/protocol/types/events.go                    # + ByTenant on request; + CountsByTenant on EventBucket
internal/events/aggregate.go                          # second accumulator keyed by (tenant,type) when ByTenant && widened; entitled-set guard
internal/events/aggregate_test.go                     # attribution reconciliation + entitled-set isolation tests
internal/protocol/transports/stream/handlers.go       # pass ByTenant through with the server-derived widened decision
web/console/src/lib/protocol/events.ts (or protocol.ts) # mirror both new wire fields by hand (D-223)
web/console/src/lib/protocol/wire-manifest.gen.json    # regenerated via `make protocol-ts-gen`
web/console/src/lib/events/... (the aggregate consumer) # render/verify per-tenant breakdown on the fleet view
docs/site/protocol/types.md                            # regenerated via `make protocol-docs-gen` (D-209)
docs/skills/use-the-harbor-protocol/SKILL.md           # §18 — aggregate request+response wire shape gained fields
test/integration/events_aggregate_durable_test.go      # extend: widened attribution reconciles on durable; cross-tenant isolation
scripts/smoke/phase-173.sh                             # live-server attribution assertion
docs/decisions.md                                      # D-307
docs/glossary.md                                       # counts-by-tenant attribution
docs/plans/README.md                                   # row + detail block (Pending)
```

## Public API surface

- `EventAggregateRequest.ByTenant bool` (`json:"by_tenant,omitempty"`) — additive, opt-in.
- `EventBucket.CountsByTenant map[string]map[string]int64` (`json:"counts_by_tenant,omitempty"`)
  — additive; present only when `ByTenant` && widened; keyed tenant → event_type → count.
- Go-internal: the aggregator's second accumulator + the entitled-set guard.

## D-223 lockstep touch points (wire-type field additions)

- No new method → `methods.go` untouched.
- Register the additive fields wherever the canonical `EventAggregateRequest` /
  `EventBucket` types are enumerated (`internal/protocol/types` single source).
- Hand-mirror both fields into the Console per-page wire module; no hand-rolled `fetch`.
- `make protocol-ts-gen` → regenerate `wire-manifest.gen.json` (committed, never
  hand-edited); `npm run lint` passes.
- `make protocol-docs-gen` → regenerate `docs/site/protocol/types.md` (D-209).
- `make protocol-ts-gen-check` + `make protocol-docs-gen-check` gate before final push.

## Test plan

- **Unit:** aggregate.go — attribution reconciles with totals (`Σ by-tenant == total`);
  `ByTenant` off ⇒ nil `CountsByTenant`; a non-widened read with `ByTenant` returns at most
  the caller's own tenant; the entitled-set guard drops a tenant the caller cannot read.
- **Integration:** extend `test/integration/events_aggregate_durable_test.go` — under a
  widened read over a real StateStore across two tenants, attribution splits correctly and
  reconciles; a caller entitled to one tenant sees only that tenant; identity propagation;
  ≥1 failure mode (un-widened body with `ByTenant` gets no cross-tenant attribution). Under
  `-race`.
- **Conformance:** the 171 method-parity aggregate leg runs with `ByTenant` set and asserts
  inmem/durable agree on the attribution.
- **Concurrency / leak:** attribution is per-request (accumulators are per-call); the
  aggregator stays immutable — existing N≥100 `-race` run covers it (D-025).

## Smoke script additions

- `scripts/smoke/phase-173.sh` (`PREFLIGHT_REQUIRES: live-server`):
  1. Route probe (401 → mounted; 404/405/501 → SKIP).
  2. With a dev token carrying admin/console:fleet scope: an aggregate POST with
     `by_tenant: true` and a cross-tenant filter → 200 with `counts_by_tenant` present, and
     the per-tenant counts summing to the bucket totals.
  3. An aggregate POST WITHOUT `by_tenant` → 200 with NO `counts_by_tenant` key (backward
     compatible).
  4. A non-elevated dev token with `by_tenant: true` + a cross-tenant filter → 403 (the
     widening gate fires before attribution; attribution never bypasses it).

## Coverage target

- `internal/events`: 85%.
- `internal/protocol/transports/stream` (attribution pass-through): 85%.

## Dependencies

- 171 (the aggregate must work on the durable driver and carry the server-derived
  `widened` decision before attribution can ride it).
- Composes with 172 (same response; attribution is per-bucket over whichever grid 172
  produced).
- 118 (D-223). Shipped.

## Risks / open questions

- **Per-bucket vs per-response attribution.** The ask required only "a count per (tenant,
  event_type)." I put `CountsByTenant` per-bucket (on `EventBucket`) rather than a single
  response-level rollup so it composes with the time series and 172's addressable grid;
  the per-bucket totals still reconcile to `Counts`. A response-level rollup is derivable
  by summing buckets, so no information is lost. Recorded in D-307.
- **Attribution must never widen the read.** The entitled-set guard is defence-in-depth on
  top of the existing widening gate: `CountsByTenant` is filtered to the caller's entitled
  tenants even though the widening gate already scoped the counted events. The invariant
  `Σ CountsByTenant == Counts` is the test that proves attribution is a pure re-projection,
  not a second, looser read path.

## Glossary additions

- **counts-by-tenant attribution** — see `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references resolve
- [x] Coverage ≥ target
- [x] Multi-isolation: cross-tenant attribution isolation test passes (the entitled-set
      guard + the `Σ == total` reconciliation)
- [x] Reusable-artifact concurrent-reuse test passes (attribution is per-request) — D-025
- [x] Integration test extended (widened attribution + cross-tenant isolation on durable),
      real drivers, `-race` — §17.1
- [x] Wire change: `make protocol-ts-gen` + `make protocol-docs-gen` run, regenerated
      artifacts committed; both lockstep-check gates pass; `use-the-harbor-protocol`
      SKILL.md updated (§18)
- [x] New vocabulary added to `docs/glossary.md`
