# Phase 108g — Console Sessions page (rebuilt + fully wired)

> Page-polish phase against `docs/design/console/CONVENTIONS.md` +
> `docs/design/console/PAGE-POLISH-PROCEDURE.md`. The shipped Sessions page
> (Phase 73c / D-122) predates the 108b app-shell chrome and the 108c carded
> retheme, and it ships placeholders: the detail-view bottom-dock renders
> static descriptive blurbs instead of real data, and the list's bulk
> Cancel / Pause buttons are permanently disabled. This phase rebuilds both
> the list (`/sessions`) and the detail (`/sessions/[id]`) routes to the
> carded vocabulary the four done pages (Overview, Live Runtime, Settings,
> Playground) set, and **wires every datum and action to the shipped Protocol
> surface** — zero placeholders.

## Summary

Rebuild the Sessions list + detail routes to the design-system foundation
(D-121) and the PAGE-POLISH bar (every datum real-wired, every action real,
four states, zero fabrication). The list keeps `sessions.list` (cursor-paged)
with a calm carded toolbar (free-text search + Status facet + admin Tenant
facet + Sort + Refresh), lean registry-owned columns plus an Events count
enriched per visible row, and bulk Cancel / Pause **wired for real** (iterate
the shipped `cancel` / `pause` control methods per selected row, control-scope
gated). The detail view replaces the placeholder `BottomDockTabs` with five
real tabs — Trajectory, Events, Cost History, Control History, Interventions —
each a session-filtered projection of the shipped `events.subscribe` SSE
(reusing the `events/` lib and `overview/cost.ts`), plus a real Resume action
on pending interventions and a Clone / Continue / Cancel / Export-events action
set. The list deliberately omits a Cost column: per-session cost has no shipped
aggregate wire (`events.aggregate` counts events by type, it does not sum
`llm.cost.recorded`), so cost is surfaced where it can be computed honestly —
the detail's Cost History tab (live SSE sum). The scrubbing replay player and
the Markdown full-transcript export stay deferred (they need the Phase 73
`state.history` surface, still `Pending`); Convert-to-Evaluation stays disabled
with a tooltip (D-064). All such gaps render honest states, never fabricated
values.

## RFC anchor

- RFC §7
- RFC §7.1

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §"Sessions view": Sessions is the investigative past-and-active
  record — a virtualised list + per-session detail with a shared per-task
  detail pane. This phase keeps the list/detail two-surface split and makes the
  detail pane real (the bottom-dock tabs) rather than a blurb placeholder.
- brief 11 §CC-2 (identity-aware UI): the Identity column + the admin Tenant
  facet render the impersonation triplet (D-107) and gate the cross-tenant
  facet on the `admin` claim (D-079). Preserved.
- brief 11 §CC-4 (search): sessions are high-cardinality; free-text search is
  runtime-side (`search.sessions`). The toolbar's search box compiles into the
  `sessions.list` query (the Service forwards to `search.sessions`).
- brief 12 §"The two-surface model": Live Runtime is present-tense (one
  session, steered now); Sessions is investigative (any session, replayed).
  This phase keeps that separation — the bottom-dock tabs are read-only
  projections; live steering stays on Live Runtime.

## Findings I'm departing from (if any)

- brief 11 §"Sessions view" sketches a per-row cost + token column on the list.
  This phase departs: the Phase 08 Session registry does not model per-session
  cost / tokens (D-122 — the row is a pure lifecycle projection), and no
  shipped aggregate wire sums `llm.cost.recorded` per session. Rather than fake
  a value or ship an empty `—` column, the list omits Cost / Tokens and the
  detail's Cost History tab computes cost from the live event stream. Recorded
  in D-179; a future `cost.aggregate` wire (V1.3) is the natural home for a
  per-row list cost.

## Goals

- Rebuild `/sessions` + `/sessions/[id]` to the carded `.panel.card` +
  `.panel-title` vocabulary (Overview 108c), tokens only, Svelte 5 runes
  (D-092), HarborClient + connection.ts only.
- Wire bulk Cancel / Pause to the shipped `cancel` / `pause` control methods
  (control-scope gated, iterate per selected row) — no disabled placeholder.
- Replace the placeholder `BottomDockTabs` with five real, session-filtered
  tabs sourced from the shipped `events.subscribe` SSE + `pause.list`.
- Wire the detail action set: Continue in Live Runtime (nav), Clone (`start`),
  Cancel session (`cancel` per active task), Resume intervention
  (`resume` / `approve` / `reject`), Export events (JSONL).
- Render honest states for the V1 gaps (scrubbing replay player + MD transcript
  export → deferred; Convert-to-Evaluation → disabled w/ tooltip).
- Delete the dead placeholder code paths; keep the reusable libs
  (`sessions/format.ts`, `SessionFacetChips`, `IdentityCell`).

## Non-goals

- No new Runtime / Protocol method. The page stays a pure consumer of the
  shipped surface (`sessions.list` / `sessions.inspect` / `events.subscribe` /
  `events.aggregate` / `pause.list` / `cancel` / `pause` / `resume` /
  `approve` / `reject` / `start` / `artifacts.list` / `search.sessions`).
- No `cost.aggregate` wire (deferred to V1.3 — D-179).
- No `state.history` / `state.list_trajectories` consumer (Phase 73 `Pending`);
  the scrubbing replay player + MD transcript export stay deferred.
- No change to the multi-isolation contract — every call carries the triple.

## Acceptance criteria

- [ ] `/sessions` renders a carded list: a calm toolbar (search + Status facet +
  admin-only Tenant facet + Sort + Refresh) over the `sessions.list` table with
  cursor pagination, all inside the app shell (no per-page PageHeader title bar
  duplicating the breadcrumb chrome).
- [ ] List columns: Session, Status, Agent, Identity, Started, Last activity,
  Events, Duration. Per visible row, the Events count is enriched via
  `events.aggregate` and the Duration is the **active** processing time
  (Σ of the session's run `duration_ms` from a session-scoped `tasks.list`),
  not wall-clock from open to now — mirroring the Playground's `activeWorkMs`.
  No Cost column (D-179).
- [ ] Bulk Cancel / Pause are **functional** when rows are selected and the
  operator holds the control scope: each iterates the shipped `cancel` /
  `pause` method per selected session's active run, and the resulting state
  change is observed on reload. When the control scope is absent, the actions
  are disabled with a tooltip naming D-066.
- [ ] `/sessions/[id]` renders a carded header + a right rail (Session Summary,
  Recent Interventions, Recent Artifacts) + a five-tab bottom dock.
- [ ] Each bottom-dock tab renders **real** data from the session-filtered
  event stream: Trajectory (planner/tool/task lifecycle timeline), Events (raw
  filtered log), Cost History (`llm.cost.recorded` summed client-side), Control
  History (`control.received/applied/rejected`), Interventions
  (`pause.*` / `tool.approval_*` / `tool.auth_*` + `pause.list` backfill).
- [ ] The Interventions tab's Resume action invokes `resume` / `approve` /
  `reject` against the live Runtime when an intervention is still pending.
- [ ] The detail action set works: Continue in Live Runtime navigates with the
  session id; Clone invokes `start` with the cloned query; Cancel session
  invokes `cancel`; Export events downloads JSONL.
- [ ] V1 gaps render honest states: the scrubbing replay player + MD transcript
  export are absent/deferred (not faked); Convert-to-Evaluation is disabled
  with a D-064 tooltip.
- [ ] All four `PageState` branches render on both routes (loading / loaded /
  empty / error) plus the disconnected branch.
- [ ] Both routes are viewport-locked (PAGE-POLISH §6): the document never
  full-page-scrolls; the list table, the detail dock tab-panel, and the rail
  cards each scroll internally. The list table columns are column-aligned
  (header label over its cell) with a sticky header.
- [ ] `npm run check` 0/0, `npm run lint` clean, `npm run test` green.
- [ ] `scripts/smoke/phase-108g.sh` 0 FAIL; phase-73c (if present) and the
  other Console smokes stay green.

## Files added or changed

- `web/console/src/routes/(console)/sessions/+page.svelte` — list, rebuilt.
- `web/console/src/routes/(console)/sessions/[id]/+page.svelte` — detail,
  rebuilt.
- `web/console/src/lib/components/sessions/BottomDockTabs.svelte` — rewritten
  from placeholder blurbs to five real, event-sourced tabs over one
  session-scoped `events.subscribe` subscription (the per-tab panels render
  inline — one subscription, derived views, no prop-drilling the event array).
- `web/console/src/lib/sessions/trajectory.ts` — pure event→step projection.
- `web/console/src/lib/sessions/tests/trajectory.test.ts` — unit specs for the
  new projection logic.
- `web/console/tests/sessions-page.spec.ts` — Playwright e2e, rebuilt for the
  carded, fully-wired structure.
- `web/console/src/lib/components/ui/DataTable.svelte` — §17.6 cross-component
  fix: the clickable-row path wrapped each row's cells in a separate nested
  `<table>`, so the `<thead>` columns and the body columns were laid out by
  independent tables and never aligned. Render the row cells DIRECTLY in a
  clickable `<tr>` (checkbox cell stops propagation) so header + cells share
  one column model, and pin the `<thead>` sticky for the scrolling-table case.
  Fixes column alignment for every list page that uses `onrowclick`.
- `web/console/src/lib/components/sessions/{RecentInterventionsCard,RecentArtifactsCard}.svelte`
  — the rail lists scroll internally (`--layout-rail-list-max`) so the rail
  never grows unbounded (viewport-lock).
- `web/console/src/lib/tokens.css` — adds `--layout-rail-list-max`.
- Duration is **active processing time** (Σ per-run `duration_ms`), not
  wall-clock: the list enriches it per visible row and the detail enriches it
  via a session-scoped `tasks.list` (the connection-default client returns the
  wrong session's runs — fixing the detail's prior "0 tasks" count bug too).
- `scripts/smoke/phase-108g.sh` — new static-only guard.
- `docs/plans/phase-108g-console-sessions-page.md` — this plan.
- `docs/decisions.md` — D-179.
- `docs/design/console/page-sessions.md` — §13 reframe note.

## Public API surface

N/A — Console-only page-polish phase. No Go public API, no new Protocol method.

## Console consistency (CONVENTIONS.md §9 + PAGE-POLISH-PROCEDURE.md)

- **Route group + shell.** Both routes stay under `(console)/sessions/`, served
  at `/sessions` and `/sessions/<id>`, rendered inside the one app shell. The
  breadcrumb, ⌘K search and footer are chrome (108b) — never rebuilt per page.
- **`<PageState>` four-state contract.** Both routes route their async state
  through `<PageState>` (loading / ready / empty / error) plus the disconnected
  branch.
- **Shared `ui/` inventory + tokens.** Carded `.panel.card` + `.panel-title`
  vocabulary copied from the Overview page (108c); design tokens only — no raw
  color / spacing / type-scale literals (stylelint-enforced). The rail uses
  `--size-rail`.
- **HarborClient + connection.ts.** All async state flows through
  `HarborClient` + the typed `SessionsProtocol` + the shared `events/`
  subscription — no hand-rolled `fetch`.
- **No fabrication (PAGE-POLISH §1).** Every datum is traced to a shipped
  Protocol method / event; the V1 gaps (cost-per-row aggregate, state.history)
  render honest states, never invented values.
- **Viewport discipline (PAGE-POLISH §6).** The list table + the detail dock
  scroll internally; the chrome never full-page-scrolls; no white bleed.

## Test plan

- **Unit (Vitest):** `sessions/trajectory.ts` event→step projection ordering;
  the per-session cost sum (reusing / extending `overview/cost.ts`); the
  per-row Events enrichment fold; the dock tab event-type filtering. Each
  decoder is tested against a captured real wire frame (SSE payload PascalCase
  gotcha — PAGE-POLISH §3.3).
- **Integration (Playwright e2e):** `web/console/tests/sessions-page.spec.ts` —
  hydration on both routes, list → detail navigation, the four PageState
  branches, the bulk-action scope gating, the dock tab switching, and the
  disconnected shell.
- **Conformance:** N/A — no driver / interface added.
- **Concurrency / leak:** N/A — no reusable Go artifact built.

## Smoke script additions

- `scripts/smoke/phase-108g.sh` (static-only): asserts the list page drops the
  disabled-bulk placeholder (the dock + bulk are wired, not faked), keeps the
  `sessions-page` / `catalog-row` testids, and the detail page's
  `BottomDockTabs` no longer carries the placeholder blurb strings but imports
  the real per-tab components + the `events` subscription. Anchors on
  imports / testids / exported symbols (never bare comment strings).

## Coverage target

`web/console` (front-end): the new Vitest specs + the rebuilt Playwright e2e
must pass; no Go coverage delta (no Go code touched).

## Dependencies

- Phase 73c / D-122 (the Sessions page this phase rebuilds)
- Phase 60 / 72 (`events.subscribe` SSE — the dock tabs' source)
- Phase 72a (`events.aggregate` — the per-row Events enrichment)
- Phase 54 / 72e (`pause.list` + `approve` / `reject` — Interventions)
- Phase 42 / D-047 (`cancel` / `pause` / `resume` / `start` control methods)
- Phase 108b (app-shell chrome) + Phase 108c (the carded vocabulary copied)

## Risks / open questions

- The detail dock's event tabs are only as complete as the event source. On an
  in-memory event driver (`driver_events=inmem`) a long-past session's events
  have aged out of the ring buffer — the tabs render an honest empty state for
  such sessions; a durable event log (Phase 57) backfills where configured.
  This is surfaced, not hidden.
- Per-row Events enrichment issues one `events.aggregate` call per visible row.
  Bounded to the visible page (≤ page size); not a global scan. A future
  batched aggregate would reduce round-trips.
- Cost-per-row in the list awaits a `cost.aggregate` wire (V1.3 — D-179). The
  detail Cost History tab computes cost from the live stream in the meantime.

## Glossary additions

None — the page reuses existing vocabulary (`Live Runtime`, `Runtime lens`,
`Faceted filter (sessions)`, `Scope claim`).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A; every call carries the triple via the unchanged transport, no isolation path added.
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, no reusable Go artifact built (Console-only page-polish).
- [ ] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — covered by the rebuilt Playwright e2e (front-end seam); no Go seam touched.
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — D-179 records the cost-column departure + the reframe.
