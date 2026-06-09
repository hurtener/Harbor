# Phase 108p — Console Flows page

## Summary

Phase 108p brings the Console **Flows** page to the Phase 108 page-polish
acceptance bar (`docs/design/console/PAGE-POLISH-PROCEDURE.md`). It is a
Console-only pass — the six `flows.*` Protocol methods already shipped in Phase
73i, so the page is already fully real-wired; this phase closes the *foundation*
gap. Both routes (the `/flows` catalog and the `/flows/[flow_id]` detail) are
rethemed to the carded (`.panel.card`), viewport-locked composition the
Tools-108k / Memory-108n / MCP-108m / Artifacts-108o rebuilds use, the per-page
`PageHeader` is dropped (breadcrumb / ⌘K / footer are app-shell chrome, 108b),
and the king files are refactored into `FlowsListState` / `FlowDetailState`
controllers + pure `derive.ts` projections + a focused `FlowsTable` component.

The pass also corrects an **inaccurate empty-state** (§17.6 fix-what-you-find):
the catalog's "No flows registered" copy claimed flows are *defined in agents
whose planner is Graph / Workflow / Deterministic*. That is wrong — a flow is a
composable engine-graph DAG that is **registered as a tool** (`TransportFlow`,
D-023) and invocable directly via `flows.run`; `PlannerFamily` is catalog
metadata, never a runtime gate, and the flow engine is a general primitive (not
planner-bound). The copy is rewritten to that truth, future-open. See D-188.

## RFC anchor

- RFC §6.1 (Core runtime — engine graphs), §6.2 (Planner interface — flows are
  not planner-exclusive), §7 (Console layer — runtime-lens Protocol client).

## Briefs informing this phase

- brief 11 (Console feature surface — §"Flows view")
- brief 12 (Console deployment and shared UI — §"The two-surface model")

## Brief findings incorporated

- brief 11 §"Flows view": "Run this flow" is the page's only mutating action; it
  is scope-gated (`auth.ScopeAdmin`, D-079) and degrades to
  disabled-with-tooltip, never vanishes (D-066).
- brief 12 §"The two-surface model": every datum round-trips through the
  Protocol; the Console DB holds only the saved-view chips + the Console-local
  snapshot/compare state (D-061). No shadow source of truth for flow entities.

## Findings I'm departing from (if any)

Two deliberate deviations, both from the page mock (`page-flows.md`), both
documented in D-188:

1. **The separate `/flows/[flow_id]` detail route is retained**, against the
   108m/n/o rail-as-detail pattern. The flow detail surface is a read-only
   engine-graph **DAG canvas** + a run-history / run-summary stack — it needs
   full width and does not fit a ~`--size-rail` rail. The mock itself specifies
   a distinct Detail Mode (`page-flows.md` §4); the rail pattern is the wrong
   shape here. Both routes still adopt the carded + viewport-locked shell.
2. **The empty-state copy departs from the mock's planner-family wording**
   (`page-flows.md` §7) because that wording is factually wrong (see Summary).
   The mock prose is the lower authority; the runtime truth wins (§17.6).

## Goals

- Retheme both routes to the carded (`.panel.card`), viewport-locked
  composition: a filter card (list) / action row (detail) that does not scroll,
  with only the table / graph / rail scrolling internally (PAGE-POLISH §6).
- Drop the per-page `PageHeader` on both routes.
- King-file refactor: `FlowsListState` + `FlowDetailState` controllers
  (`$lib/flows/state.svelte.ts`, `$lib/flows/detail.svelte.ts`) + pure
  `$lib/flows/derive.ts` (re-exports `format.ts`, adds `toPageError` /
  `displayStatus` / `successKind` / `health`) + a focused `FlowsTable` component.
- Correct the empty-state copy to the flows-as-tools, future-open truth (D-188).
- Preserve every shipped behaviour real-wired: catalog (`flows.list`), metrics
  rail (`flows.metrics`), graph + budget (`flows.describe`), run history
  (`flows.runs.list`), run summary (`flows.runs.describe`), Run flow
  (`flows.run`), saved views, snapshot/compare (Console-local).

## Non-goals

- No new Protocol method — pure consumer of the shipped `flows.*` surface.
- No flow mutation beyond `flows.run` (authoring / versioning / import-export
  stay deferred per D-063).
- No collapse of the detail route into the rail (see departure 1).

## Acceptance criteria

- [x] Both routes carded + viewport-locked: `scrollHeight == innerHeight`, no
      horizontal overflow at 1512×945 (verified live).
- [x] The per-page `PageHeader` is gone on both routes.
- [x] The catalog renders the real `flows.list`; the metrics affordance loads
      the real `flows.metrics` into the rail; row-click opens the detail route
      (mock-faithful interaction model).
- [x] The detail route renders the real `flows.describe` graph + budget +
      `flows.runs.list` history; selecting a run loads `flows.runs.describe`.
- [x] `Run flow` calls the real `flows.run`; admin-gated, disabled-with-tooltip
      without the claim (D-066 / D-079), never a fabricated success.
- [x] The empty-state renders the corrected flows-as-tools copy (D-188).
- [x] All four `PageState` branches + the disconnected redirect preserved on
      both routes; the nested rail PageStates resolve.
- [x] `npm run check` 0/0, `npm run lint` clean (tokens only), the unit
      (`derive.test.ts`) + e2e (`flows-page.spec.ts`) suites green.

## Files added or changed

- `web/console/src/routes/(console)/flows/+page.svelte` — rebuilt (carded list).
- `web/console/src/routes/(console)/flows/[flow_id]/+page.svelte` — rebuilt
  (carded detail; graph full-width).
- `web/console/src/lib/flows/state.svelte.ts` — NEW `FlowsListState` controller.
- `web/console/src/lib/flows/detail.svelte.ts` — NEW `FlowDetailState` controller.
- `web/console/src/lib/flows/derive.ts` — NEW pure projections (+ re-exports
  `format.ts`).
- `web/console/src/lib/flows/saved_views.ts` — NEW Console-DB store opener
  (mirrors `artifacts/saved_views.ts`).
- `web/console/src/lib/components/flows/FlowsTable.svelte` — NEW (catalog rows).
- `web/console/src/lib/flows/tests/derive.test.ts` — NEW unit specs.
- `web/console/tests/flows-page.spec.ts` — updated header + carded structure.
- `scripts/smoke/phase-108p.sh` — NEW static Console guard (+ optional live
  `flows.list` probe).
- `docs/plans/phase-108p-console-flows-page.md` — this plan.
- `docs/decisions.md` — D-188.
- `docs/design/console/page-flows.md` — §7 empty-state copy reconciled (D-188).

## Public API surface

None — Phase 108p adds no Protocol method. It is a pure consumer of the Phase
73i `flows.list` / `flows.describe` / `flows.runs.list` / `flows.runs.describe` /
`flows.run` / `flows.metrics` surface.

## Test plan

- **Unit:** `web/console/src/lib/flows/tests/derive.test.ts` — `displayStatus`
  (ready/empty derived live), `toPageError` (ProtocolError vs unknown),
  `successKind` / `health` thresholds, plus the re-exported `format.ts`
  projections against the real wire shapes.
- **Integration / e2e:** `web/console/tests/flows-page.spec.ts` (Playwright) —
  catalog, four-state, detail graph, Run-flow scope gate, run-history drill,
  view-only invariant. Gated on `consoleSubcommandAvailable()` per the harness.
- **Smoke:** `scripts/smoke/phase-108p.sh` — static guard (PageHeader gone,
  carded vocabulary, controllers + derive.ts exist, FlowsTable, no hand-rolled
  fetch, corrected empty-state copy) + an optional live `flows.list` route probe
  (404/405/501 → SKIP per §4.2).

## Smoke script additions

`scripts/smoke/phase-108p.sh` (live-server): a best-effort probe that the
shipped `POST /v1/flows/list` route is mounted and fails closed without a bearer
(401), with the 404/405/501 → SKIP convention for a pre-73i build. Plus the
static Console guard — the per-page PageHeader gone on both routes, the carded
`.panel.card` vocabulary, the `FlowsListState` / `FlowDetailState` controllers +
`derive.ts` (`displayStatus` / `successKind` / `health`) + the `FlowsTable`
component exist and are composed, the corrected flows-as-tools empty-state copy
(and the absence of the old planner-family wording), the admin-gated Run actions,
no hand-rolled `fetch`, and the Save-view N7 contract.

## Coverage target

- `web/console/src/lib/flows`: `derive.ts` unit-covered; the controllers'
  four-state paths covered via the e2e spec + the live verification ledger.

## Dependencies

- Phase 73i / D-117 (the `flows.*` Protocol surface + the pre-chrome page).
- Phase 108b (chrome — supersedes the per-page header).
- Phase 108k / D-183 / 108n / D-186 / 108o / D-187 (the carded master-detail
  pattern + the controller / derive.ts king-file refactor it mirrors).
- Phase 105 (the disconnected redirect).

## Risks / open questions

- The detail route is retained against the rail-as-detail pattern (D-188,
  departure 1) — a deliberate, documented divergence justified by the DAG canvas.
- `flows.run`'s scope gate is `auth.ScopeAdmin` (D-079), surfaced in the UI as
  `hasScope(connection, 'admin')`; the runtime is the authoritative gate and the
  UI degrades disabled-with-tooltip.

## Glossary additions

None — `Flows (Console page)`, `Runtime lens`, `Scope claim` already live in
`docs/glossary.md`; this phase adds no new domain vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (no AGENTS/CLAUDE edit in this phase)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: N/A — no Go change; the consumed
      `flows.*` surface is already identity-mandatory + admin-gated.
- [ ] **If this phase builds a reusable artifact:** N/A — Console controllers
      are per-page-instance Svelte state, not shared compiled artifacts.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam:** the Console↔`flows.*` seam is covered by the live
      verification ledger + the Playwright spec.
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: justified above + decisions.md entry
      filed — yes (D-188, two mock deviations).
