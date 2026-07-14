# Phase 174 — session-projection-enrichment

## Summary

`sessions.list` / `sessions.inspect` ship a `SessionRow` with eight declared,
typed, wire-visible fields that the sole producer (`projectRow`) never assigns —
they are permanently zero on every row. The runtime then ships facets, a sort,
and a keyset cursor over those zeros, so `cost_above_cents`, `agent_ids`,
`has_failed_task`, `has_intervention`, `sort=cost_desc`, and the agent-name
`query` axis all return **false absence** (an empty page / a mis-ordered page)
on a fleet full of matching sessions. This phase populates the six numeric /
boolean counters at the runtime source via a read-time `Enricher` seam (the seam
`tasks.Projector` already ships), and closes the residual class for the two
agent fields — for which no single-valued session→agent binding exists today —
by making their absence representable and failing an operation over them loudly
rather than succeeding emptily (HA-22).

## RFC anchor

- RFC §6.9
- RFC §5.1
- RFC §7

## Briefs informing this phase

- brief 05
- brief 11

## Brief findings incorporated

- brief 05 §"Sessions and SessionManager" (RFC §6.9): the Session record is a
  pure lifecycle record — it does NOT model per-session cost, tokens, task
  counts, or an agent binding. This phase treats those as **read-time
  enrichment** aggregated from the subsystems that own them, never as new
  columns on the Session record (no shadow aggregation store).
- brief 11 §"Sessions view": the operator mockup sketched a per-row cost / token
  column and cost sort on the Sessions list. D-179 deferred it ("a dedicated
  cost aggregate wire is the V1.3 evolution that would restore a per-row list
  cost") rather than fabricate a value. This phase delivers exactly that deferred
  evolution — the runtime-side rollup — so the shipped facet / sort become
  truthful instead of remaining zero-backed.

## Findings I'm departing from (if any)

D-179 pinned "the registry projection is pure; the Console enriches from its own
event subscription." This phase **extends** that stance (it does not overturn the
pure-registry-projection of the *lifecycle* fields): the projector stays a pure
registry read for lifecycle fields, and the derived counters move to an explicit
optional `Enricher` seam the projector reads — mirroring the already-shipped
`tasks.Projector` enricher (`internal/tasks/protocol/registry_projector.go`).
The departure is that the aggregation now happens **server-side at the source,
once**, instead of being replicated in every Protocol client. Justification: the
harm HA-22 documents (a server-side facet / sort over zeros returning false
absence) is invisible to a client-side enrichment strategy — the facet runs on
the runtime before the client ever sees a row, so a third-party client using
`cost_above_cents` gets an empty page no matter how it enriches. D-309 records
this extension; D-179 is cross-referenced, not superseded (its Console-side
Cost-History tab stays valid).

## Goals

- Populate the six false-absence counters on every `SessionRow` at the runtime
  source: `tasks_count`, `events_count`, `total_cost_cents`, `total_tokens`,
  `has_pending_intervention`, `has_failed_task` — with **honest zeros** when the
  enricher is unwired (partial build), never silent degradation of a known value.
- Introduce a `sessions.Enricher` seam on `sessions.Projector` shaped exactly
  like the shipped `tasks` enricher (optional, `WithEnricher(...)`, nil ⇒ honest
  zeros), so the read-time rollup is a §4.4 seam with a swappable production
  driver — not a concrete dependency baked into the projector.
- Wire a production `Enricher` implementation that aggregates from data the
  runtime already owns one package over: `llm.cost.recorded` events (cost +
  tokens), the task registry scoped to the session (`tasks_count`,
  `has_failed_task`), the durable event stream / `events.aggregate`
  (`events_count`), and the pause registry (`has_pending_intervention`).
- Make the facet / sort / cursor over the counters truthful by construction: a
  cost / has-failed / has-intervention facet now narrows real data; `cost_desc`
  now orders by real cost; the keyset cursor encodes the real `total_cost_cents`.
- Close the residual class for `agent_id` / `agent_name` (no clean single-valued
  session→agent binding exists — the agent registry keys by the triple and a
  session may run multiple agents over its life): make their absence
  **representable** and make a filter / sort / query over an **unpopulated** agent
  field fail loudly with `invalid_request`, never a silent empty page (the class
  rule, D-311).
- Pin the test fixture against the real producer so a fixture can never again be
  richer than the runtime (§17.8).

## Non-goals

- No new Protocol method, no `ProtocolVersion` bump. The `SessionRow` wire shape
  is unchanged for the six populated fields (they are already declared). The only
  wire-shape change is the agent-field absence representation (see Acceptance) —
  additive, D-223/D-209 lockstep in the same PR.
- No shadow aggregation store and no new column on the Session record. The rollup
  is a read-time aggregation, exactly like the tasks precedent.
- No first-class session→agent binding link. Deriving a single authoritative
  agent for a multi-agent session is out of scope; this phase makes the absence
  honest and files the binding as a follow-up (see Risks).
- No change to the Console-side Cost-History detail tab (D-179) — it stays a
  valid live-event projection; the list columns / facet / sort become truthful.

## Acceptance criteria

- [ ] `sessions.Projector` gains an optional `Enricher` seam
      (`WithEnricher(...)`); a nil enricher yields honest-zero counters and the
      projector's godoc states the zeros are "we don't have this data," not silent
      degradation (mirroring `registry_projector.go:39-40`).
- [ ] A production `Enricher` implementation aggregates `total_cost_cents` +
      `total_tokens` from `llm.cost.recorded` scoped to the session, `tasks_count`
      + `has_failed_task` from the task registry scoped to the session,
      `events_count` from the durable event substrate, and
      `has_pending_intervention` from the pause registry — all identity-scoped,
      no cross-session bleed.
- [ ] With the production enricher wired, a `sessions.list` over a session with
      recorded cost returns a row whose `total_cost_cents` > 0, and
      `filter.cost_above_cents = N` narrows to exactly the rows above N (proven
      against real drivers end-to-end, §17.1).
- [ ] `sort=cost_desc` orders rows by real `total_cost_cents` descending
      (session-id tiebreak preserved), and the keyset cursor pages a cost-sorted
      list correctly across a page boundary.
- [ ] `filter.has_failed_task=true` / `filter.has_intervention=true` narrow to
      real matching rows; `false` excludes them.
- [ ] `agent_id` / `agent_name` absence is representable in-band (the chosen
      mechanism: nullable `*string` with `omitempty` OR a `session_counters` /
      `session_agent_binding` capability bit on `runtime.info`), AND a
      `filter.agent_ids` / `sort` / agent-name `query` issued against an
      unpopulated agent field returns `CodeInvalidRequest` (loud) rather than a
      silent empty page. The chosen mechanism is recorded in D-309.
- [ ] The `protocol_test.go` fixture (`sampleRows` / `mk`) no longer assigns
      fields the production projector cannot produce; a new test pins the
      projector's output field-set against the real producer so a richer-than-
      runtime fixture fails (§17.8).
- [ ] `*ListerProjector` and the production `Enricher` are safe for concurrent
      reuse (immutable after construction; per-call state in args/locals) — a
      concurrent-reuse test with N≥100 invocations against one shared instance
      passes under `-race` (§5, D-025).
- [ ] Console: the "Most expensive" sort option and the `cost_above_cents` facet
      chip (already rendered) now operate on truthful data; the list surfaces the
      populated counters per the Console-consistency section below (§4.5 / D-121).
- [ ] `scripts/smoke/phase-174.sh` asserts a populated counter reaches the wire
      on the live dev server (or SKIPs cleanly until the surface lands), and that
      a filter over an unpopulated agent field fails loud.

## Files added or changed

- `internal/sessions/protocol/lister_projector.go` — `Enricher` seam +
  `WithEnricher` option; `projectRow` overlays enriched counters; agent-field
  absence handling.
- `internal/sessions/protocol/enricher.go` *(new)* — the `Enricher` interface +
  production implementation aggregating from cost events / task registry / event
  substrate / pause registry.
- `internal/sessions/protocol/protocol.go` — `List` rejects a facet / sort /
  query over an unpopulated agent field with `ErrInvalidRequest`; sort / cursor
  unchanged structurally (now backed by real values).
- `internal/sessions/protocol/filter.go` — unchanged logic; covered by the new
  truthful-data tests.
- `internal/protocol/types/sessions.go` — IF the agent-absence mechanism is
  nullable fields: `AgentID` / `AgentName` become `*string` `omitempty`
  (additive); IF capability-bit: no change here, a new capability string in the
  capabilities single source. The decision is recorded in D-309.
- `internal/sessions/protocol/*_test.go` — de-enrich the fixture; pin the
  projector field-set; truthful-facet / cost-sort / concurrent-reuse tests.
- `test/integration/sessions_enrichment_test.go` *(new)* — real drivers
  end-to-end (§17.1).
- `cmd/harbor/...` (wiring) — construct the production `Enricher` and pass
  `WithEnricher(...)` where the sessions Protocol service is assembled; advertise
  the capability bit if that mechanism is chosen.
- `web/console/src/lib/protocol/...` and `wire-manifest.gen.json` — D-223
  lockstep IF the wire shape / capability changes; the
  `web/console/src/routes/(console)/sessions/` route and
  `SessionFacetChips.svelte` render truthful counters.
- `docs/skills/observe-with-the-console/SKILL.md` and
  `docs/skills/use-the-harbor-protocol/SKILL.md` — §18 same-PR skill update.
- `scripts/smoke/phase-174.sh` *(new)*.

## Public API surface

```go
// internal/sessions/protocol
type Enricher interface {
    // Counters returns the read-time counter rollup for one session,
    // aggregated from the cost-event stream, the task registry, the event
    // substrate, and the pause registry — all identity-scoped. A zero-valued
    // return is honest ("we don't have this data"), never silent degradation.
    Counters(ctx context.Context, id identity.Identity, sessionID string) SessionCounters
}

type SessionCounters struct {
    TasksCount             int
    EventsCount            int
    TotalCostCents         int64
    TotalTokens            int64
    HasPendingIntervention bool
    HasFailedTask          bool
}

func WithEnricher(e Enricher) ListerProjectorOption // nil ⇒ honest zeros
```

## Console consistency (§4.5 / D-121)

The Console Sessions page already renders the "Most expensive" sort option
(`web/console/src/routes/(console)/sessions/+page.svelte:438`) and the
`cost_above_cents` facet chip
(`web/console/src/lib/components/sessions/SessionFacetChips.svelte:169`) — today
over permanently-zero data, so they silently misbehave for operators. This phase
makes those already-shipped affordances **truthful** (not un-hidden): with the
runtime populating real counters, "Most expensive" orders by real cost and the
cost-above filter narrows real rows. The list may surface the populated counters
(tasks / events / cost / tokens) per the carded vocabulary D-179 set; no new page
composition is introduced. All work stays on the shared `HarborClient` +
`connection.ts`, tokens only, Svelte 5 runes (D-092). If the wire shape or a
capability changes, the typed per-page Protocol client and `wire-manifest.gen.json`
are updated in the same PR under the D-223 lockstep gate. Affected operator
skills (§18): `observe-with-the-console` (surface `console`) and
`use-the-harbor-protocol` (surface `protocol`) — both updated in the same PR.

## Test plan

- **Unit:** `projectRow` overlays enriched counters; nil enricher ⇒ honest zeros;
  `List` rejects a facet / sort / query over an unpopulated agent field with
  `ErrInvalidRequest`; truthful `cost_above_cents` / `has_failed_task` /
  `has_intervention` narrowing; `cost_desc` ordering + cursor paging over real
  cost; the de-enriched fixture; a projector-field-set pin test (§17.8) that
  fails if the fixture carries a field the real producer cannot.
- **Integration:** `test/integration/sessions_enrichment_test.go` — real drivers
  (`events/drivers/inmem` + a durable StateStore, real task registry, real pause
  registry): record cost events + spawn a failing task under a session, list it,
  assert `total_cost_cents` / `has_failed_task` reach the wire; assert a
  cross-tenant isolation case (session A's cost never bleeds into session B's row);
  ≥1 failure mode (a filter over an unpopulated agent field fails loud). Runs
  under `-race`.
- **Conformance:** N/A — no new driver-plural interface (the enricher is a single
  read-time seam; a future remote enricher slots behind it).
- **Concurrency / leak:** N≥100 concurrent `ListSessions` / `Counters` against one
  shared projector+enricher under `-race`; assert no data race, no context bleed
  (per-run identity assertions), no goroutine leak (§5 / D-025).

## Smoke script additions

- `phase-174.sh` (live-server): with the dev server booted, `sessions.list`
  returns a row whose `total_cost_cents` / `tasks_count` reflect real activity
  (asserts a non-zero counter once a run has produced cost, else SKIPs); and a
  `sessions.list` with a filter over an unpopulated agent field returns an
  `invalid_request` error, never an empty 200 page. 404/405/501 → SKIP.

## Coverage target

- `internal/sessions/protocol`: 85%

## Dependencies

- 08 (Session registry), 73c/108g (`sessions.list` / `sessions.inspect` + Console
  page), 107a (`tasks` enricher precedent), 60/72a (`events.subscribe` /
  `events.aggregate`), 54 (pause registry). All shipped.

## Risks / open questions

- **`agent_id` / `agent_name` binding.** No single-valued session→agent link
  exists today (the agent registry keys by the triple; a session may run multiple
  agents). This phase deliberately does NOT invent one — it makes the absence
  honest + fails loud on a facet over it (D-311 class rule). A follow-up phase can
  add a first-class "last agent bound to this session" read once the runtime
  models it. Flagged rather than faked (CLAUDE.md §13).
- **Read-time rollup cost at scale.** The rollup runs per visible row (≤ page
  limit, default 50 / max 200). If per-row aggregation proves too expensive under
  high cardinality, fall to fallback (c): a `session_counters` capability bit +
  loud rejection of facets over unpopulated fields (representable absence). The
  runtime owns this data one package over, so (a) is expected to be feasible; the
  fallback ladder is recorded in D-309.
- **Cursor stability under live cost.** The keyset cursor encodes
  `total_cost_cents`; a session's cost advancing between pages can shift its
  keyset position (a general keyset-over-mutable-key property). Documented
  honestly — this is not page corruption (the tiebreak stays total); noted, not
  overstated.

## Glossary additions

- **false absence** — see `docs/glossary.md`.
- **representable absence** — see `docs/glossary.md`.
- **SessionRow counter enrichment** — see `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Reusable-artifact concurrent-reuse test passes (projector + enricher, N≥100, `-race`)
- [ ] Integration test exists (real drivers, identity propagation, ≥1 failure mode, `-race`)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-309, D-311)
- [ ] If the wire shape / capability changes: D-223 lockstep + D-209 regen in the same PR
- [ ] §18 skill hygiene: `observe-with-the-console` + `use-the-harbor-protocol` updated
