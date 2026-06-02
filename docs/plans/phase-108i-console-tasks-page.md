# Phase 108i — Console Tasks page (carded mode-switch rebuild + viewport-lock + full wire)

> Page-polish phase against `docs/design/console/CONVENTIONS.md` +
> `docs/design/console/PAGE-POLISH-PROCEDURE.md`. The shipped Tasks page
> (Phase 73d / D-123) is the task-granularity counterpart to Sessions — a
> kanban board + list toggle + per-task detail + bulk control — but it
> predates the 108b app-shell chrome and the 108c carded retheme: it renders
> a per-page `PageHeader`, is not viewport-locked (the document full-page-
> scrolls; the board + detail stack unbounded), and its per-task detail tabs
> (`TaskDetailTabs`) are SHALLOW — the Events / Logs / Control History /
> Interventions tabs render placeholder blurbs instead of the live event-bus
> data the mock + spec call for. This phase rebuilds the page to the carded
> `.panel.card` vocabulary the six done pages set (Overview, Live Runtime,
> Settings, Playground, Sessions, Events), viewport-locks it as a single-page
> mode-switch (board/list ⇄ detail), and wires the per-task bottom dock to the
> live `events.subscribe` SSE (the Sessions 108g `BottomDockTabs` pattern,
> run-scoped) — live-verified end-to-end against a real Runtime.

## Summary

Bring the Tasks page to verbatim parity with its mock at the look-and-feel +
viewport-discipline + full-wire level. The mock crams a kanban board, a
selected-task detail bar, a bottom-dock tab strip, and a right rail all at
once — which cannot fit one viewport without a page scroll — so the page is
composed as a single-page **mode-switch** (D-181): board/list is the default
(the faceted filter strip + the board/table filling the viewport + a right-rail
live board summary); clicking a card swaps the SAME page's main region to
detail mode (a compact task header + the real per-task action bar + the
bottom-dock tabs in one internally-scrolling card) and swaps the rail to
Summary / Parent Session / Cost. A `← Board` affordance returns. No route nav,
viewport-locked, every region packs.

The data layer is rewired to be REAL end-to-end (PAGE-POLISH §3 — verified
against the live YouTube validation agent):

- **Board / list / cards** ← `tasks.list` (Phase 73d, shipped) — real rows +
  per-status aggregates (verified: 2 running rows, live aggregate counts).
- **Detail header + Details / Input / Output tabs** ← `tasks.get` (shipped) —
  task metadata + `result_inline` / `result_ref` (verified).
- **Events / Control History / Interventions / Group / Cost tabs** ← a
  RUN-scoped projection of the shipped `events.subscribe` SSE (the Sessions
  `BottomDockTabs` pattern): the dock opens ONE subscription scoped to the
  task's parent session and filters to the task's run. The run match is
  `e.run === taskID || payload.TaskID === taskID || payload.Identity.RunID ===
  taskID` — a live-wire finding: `run` is populated on `llm.cost.recorded` /
  `planner.decision` but NULL on `task.*` lifecycle events (id is in PascalCase
  `payload.TaskID`), so a naive `e.run===taskID` filter would silently drop
  every lifecycle event.
- **Cost (right rail + Summary)** ← `llm.cost.recorded` aggregated client-side
  (verified: `payload.Cost.TotalCost`, `payload.Usage.TotalTokens`) — NOT
  `tasks.get.cost`, which comes back all-zero on this runtime. Cost Breakdown
  renders by TOKEN TYPE (Input / Output / Reasoning / Total — the real fields
  on `payload.Cost`), per the operator's D-181 sign-off.
- **Bulk Cancel / Pause + per-task Cancel / Pause / Resume / Prioritize /
  Approve / Reject** ← the shipped Phase 54 control verbs via
  `client.control.*` (a verb targets a RUN = the task id), control-scope gated
  (D-066) — never a stubbed placeholder.

The phase ships NO new Protocol method (CLAUDE.md §13 — no parallel
implementation; the page is a pure consumer of `tasks.list` / `tasks.get` /
`events.subscribe` / `pause.list` / the shipped control verbs + Console DB).

Honest-state gap-fills (PAGE-POLISH §1 — no fabrication, surface findings):

- The **Logs tab** needs the Phase 73 `state.history` surface (still Pending);
  it renders an honest empty state pointing at that surface (and the Events tab
  for the live event log), never a fabricated trajectory.
- **Free-text search** has no shipped runtime-side `search.tasks`; the search
  box stays a Console-local substring match over the loaded page (honest copy).
- Task rows carry **no `agent_name`**; the card shows the parent session id +
  the query snippet instead of a fabricated agent name.
- `tasks.get.parent_session` comes back SPARSE from the registry (empty
  agent/status/started); the Parent Session card shows the real `session_id` +
  link and `—` for the empty fields, never invented values.
- The board **drag-to-transition** gesture maps only where a real control verb
  exists (running→paused = `pause`, paused→running = `resume`, running→failed
  = `cancel`); the board is primarily click-to-select (the spec makes it
  read-only — pending→running is server-controlled).

## RFC anchor

- RFC §7
- RFC §7.1

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- brief 11 §"Tasks view" / §"Per-task detail pane": Tasks is the task-
  granularity counterpart to Sessions — a kanban board + list toggle + a
  per-task drilldown that shares the Details / Input / Output / Logs panel Live
  Runtime uses. This phase keeps that composition and wires the drilldown to
  real run-scoped event data.
- brief 11 §CC-4 (high-cardinality runtime-side search): `search.tasks` is the
  intended runtime-side search surface but is unshipped at V1; the page falls
  back to a Console-local substring match over the loaded page (honest copy),
  not a fabricated server search.
- brief 12 §"two-surface model": the control-plane verbs (Cancel / Pause /
  Resume / Prioritize / Approve / Reject) are the elevated, scope-gated surface
  (D-066); the read projections (`tasks.list` / `tasks.get` / event stream) are
  the observation surface. Preserved — controls render disabled-with-tooltip
  without the control claim.

## Findings I'm departing from (if any)

None on design. The page RE-aligns to its own mock (the carded, viewport-locked,
mode-switch composition) and DEEPENS the per-task detail to the real event-bus
data the spec §3 + §5 + §12 already call for (the prior `TaskDetailTabs` blurbs
were a shortfall against the spec, not a deliberate departure). The Cost
Breakdown axis (token-type, not the mock's LLM/Tools/Embeddings/Overhead — which
has no wire source) is the one mock refinement, recorded in D-181 with the
operator's sign-off.

## Goals

- Rebuild `/tasks` to the carded `.panel.card` + `.panel-title` vocabulary
  (Overview 108c / Events 108h), tokens only, Svelte 5 runes (D-092),
  HarborClient + connection.ts only.
- Compose it as a single viewport-locked mode-switch (D-181): board/list ⇄
  detail; no full-page scroll; the board columns / table + the detail dock +
  the right rail scroll internally; the filter strip stays fixed.
- Wire the per-task bottom dock to the live RUN-scoped `events.subscribe` SSE
  (Events / Control History / Interventions+Resume / Group / Cost), reusing the
  Sessions `BottomDockTabs` pattern; Details / Input / Output from `tasks.get`.
- Keep the real bulk + per-task control verbs (Phase 54), control-scope gated.
- Render the honest states for the unshipped surfaces (Logs / search / agent /
  sparse parent-session) — never fabricated.
- Delete the shallow `TaskDetailTabs` placeholders and any dead pre-chrome code.

## Non-goals

- No new Protocol method (the page stays a pure consumer of `tasks.list` /
  `tasks.get` / `events.subscribe` / `pause.list` / the shipped control verbs +
  Console DB).
- No runtime-side `search.tasks` (Phase 73 / `[wave-13-extends]`); search stays
  Console-local substring over the loaded page (honest copy).
- No Logs trajectory surface — needs the Phase 73 `state.history` method (still
  Pending); the tab renders an honest empty state.
- No drag-to-reorder-priority on the board (D-065 / spec §10 — priority is the
  explicit `prioritize` verb, not a drag gesture).

## Acceptance criteria

- [ ] `/tasks` renders inside the app shell with NO per-page PageHeader title
  bar; a carded filter strip (search + Status / Type / Error facet chips +
  Started-window + Board/List toggle + Refresh + Export) and carded board /
  table / detail regions.
- [ ] The page is viewport-locked: the document never full-page-scrolls; the
  board columns / the list table scroll internally behind a sticky header; the
  detail dock + the right rail scroll internally; the filter strip is fixed.
- [ ] Board mode renders the five kanban columns (Pending / Running / Paused /
  Complete / Failed) from the live `tasks.list` rows + per-status aggregates;
  the List toggle renders the same rows in the shared `DataTable`; Pagination is
  real (cursor next/prev, page-size).
- [ ] Clicking a card / row swaps the SAME page to detail mode: a compact task
  header (id + copy + status + kind + started + duration + tools), the real
  action bar (Cancel / Pause / Resume / Prioritize / Approve / Reject —
  control-scope gated), and the bottom-dock tab strip; `← Board` returns.
- [ ] The bottom dock renders REAL run-scoped data: Details / Input / Output
  from `tasks.get`; Events / Control History / Interventions / Group / Cost from
  the live `events.subscribe` SSE filtered to the task's run (lifecycle events
  matched via `payload.TaskID`, not just `e.run`); Interventions backfills
  `pause.list` and offers a real Resume / Reject. The Logs tab renders an honest
  empty state (needs Phase 73 `state.history`).
- [ ] The right rail (detail mode) shows Summary (status / duration / progress /
  tools / events / cost / tokens), Parent Session (real id + link; `—` for the
  sparse registry fields), and Cost Breakdown by token type (Input / Output /
  Reasoning / Total) from `llm.cost.recorded`. Board mode shows a board summary
  (live per-status counts).
- [ ] Bulk Cancel / Pause over selected rows + the per-task verbs dispatch the
  shipped Phase 54 control methods and the resulting state change is observed on
  reload; without the control claim they render disabled-with-tooltip.
- [ ] All four `PageState` branches render (loading / loaded / empty / error)
  plus the disconnected branch; the detail load has its own nested state.
- [ ] `npm run check` 0/0, `npm run lint` clean, `npm run test` green.
- [ ] `scripts/smoke/phase-108i.sh` 0 FAIL; phase-73d (and the other Console
  smokes) stay green.

## Files added or changed

- `web/console/src/routes/(console)/tasks/+page.svelte` — rebuilt: carded +
  viewport-locked single-page mode-switch (board/list ⇄ detail); PageHeader
  removed; real run-scoped detail wiring.
- `web/console/src/lib/components/tasks/TaskBottomDock.svelte` — NEW: the
  per-task bottom-dock tab strip (Details / Input / Output / Logs / Events /
  Control History / Interventions / Group / Cost), owning ONE run-scoped
  `events.subscribe` subscription (the Sessions `BottomDockTabs` pattern). The
  Events / Control / Interventions / Group / Cost tabs render the live stream
  filtered to the task's run; Details / Input / Output render `tasks.get`; Logs
  is the honest empty state.
- `web/console/src/lib/tasks/run-events.ts` — NEW: the pure `eventBelongsToRun`
  predicate + the per-task projections (trajectory / control / interventions /
  group / cost), unit-tested against a captured real SSE frame (the
  `payload.TaskID` lifecycle-event finding).
- `web/console/src/lib/components/tasks/TaskDetailHeader.svelte` — NEW: the
  compact detail-mode header (id + copy + status + kind + meta + `← Board`).
- `web/console/src/lib/components/tasks/RightRailCostBreakdown.svelte` —
  reworked to the token-type (Input / Output / Reasoning / Total) projection of
  `llm.cost.recorded` (was the empty `tasks.get.cost.per_step`).
- `web/console/src/lib/components/tasks/RightRailSummary.svelte` /
  `RightRailParentSession.svelte` — reworked to the carded rail composition +
  the live cost/event figures + the sparse-parent-session honest render.
- `web/console/src/lib/components/tasks/SelectedTaskActionBar.svelte` —
  retheme + the detail-mode placement; verbs unchanged (Phase 54).
- `web/console/src/lib/components/tasks/KanbanBoard.svelte` /
  `KanbanColumn.svelte` / `TaskCard.svelte` — retheme to carded + the no-agent
  honest card (session id + query snippet); viewport-locked internal column
  scroll.
- `web/console/src/lib/components/tasks/TaskDetailTabs.svelte` — DELETED
  (superseded by `TaskBottomDock.svelte`; its placeholder blurbs were the
  shortfall this phase closes).
- `web/console/tests/tasks-page.spec.ts` — Playwright e2e, updated for the
  rebuilt mode-switch structure.
- `web/console/src/lib/tasks/run-events.test.ts` — NEW vitest for the run-match
  predicate + projections.
- `scripts/smoke/phase-108i.sh` — new static-only guard.
- `docs/plans/phase-108i-console-tasks-page.md` — this plan.
- `docs/decisions.md` — D-181.
- `docs/design/console/page-tasks.md` — §13 reframe note.

## Public API surface

N/A — Console-only page-polish phase. No Go public API, no new Protocol method.

## Console consistency (CONVENTIONS.md §9 + PAGE-POLISH-PROCEDURE.md)

- **Route group + shell.** Stays under `(console)/tasks/+page.svelte`, served at
  `/tasks`, inside the one app shell. The breadcrumb, ⌘K search and footer are
  chrome (108b) — never rebuilt per page. The mode-switch is in-page (no route).
- **`<PageState>` four-state contract.** The page routes its async state through
  `<PageState>` (loading / ready / empty / error) plus the disconnected branch;
  the detail load uses its own nested state.
- **Shared `ui/` inventory + tokens.** Reuses `DataTable` (list mode, sticky
  header + clickable rows — the 108g fix), `StatusChip`, `Pagination`,
  `SavedViewChips`, `PageState`; carded `.panel.card` + `.panel-title`
  vocabulary copied from Overview / Events; design tokens only (stylelint).
  The rail uses `--size-rail`.
- **HarborClient + connection.ts.** Every Runtime read/write flows through
  `HarborClient` (`tasks.*` / `control.*` / `events.*` / `pause.*`) +
  `connection.ts`; the run-scoped subscription builds a session-scoped client
  the same way Sessions does — no hand-rolled `fetch`, no direct `localStorage`.
- **No fabrication (PAGE-POLISH §1).** Every datum is traced to a live wire and
  verified; the V1 gaps (Logs / search / agent / sparse parent-session) render
  honest states; cost comes from the event stream, not the all-zero
  `tasks.get.cost`.
- **Viewport discipline (PAGE-POLISH §6).** The board / table + dock + rail
  scroll internally; the chrome never full-page-scrolls; no white bleed.

## Test plan

- **Unit (Vitest):** `src/lib/tasks/run-events.test.ts` — the `eventBelongsToRun`
  predicate against a captured real SSE frame (asserts a `task.completed` whose
  `run` is null but `payload.TaskID` matches IS included, and a foreign-run
  event is excluded), plus the trajectory / control / interventions / cost
  projections.
- **Integration (Playwright e2e):** `web/console/tests/tasks-page.spec.ts` —
  hydration, the carded board regions, the four PageState branches, board→detail
  mode-switch, the dock tab strip, the action bar gating, and the disconnected
  shell.
- **Conformance / concurrency:** N/A — no driver / reusable Go artifact.

## Smoke script additions

- `scripts/smoke/phase-108i.sh` (static-only): asserts the page drops PageHeader,
  adopts `panel card`, imports `TaskBottomDock`, keeps the load-bearing testids
  (`tasks-page`, the board, the detail dock, the action bar), and that the
  `TaskDetailTabs` placeholder is gone. Anchors on imports / testids / exported
  symbols (never bare comment strings; never a templated literal like
  `data-testid={`x-${k}`}` — anchors on the template + the keys).

## Coverage target

`web/console` (front-end): the new `run-events` vitest suite + the updated
Playwright spec must pass; no Go coverage delta (no Go code touched).

## Dependencies

- Phase 73d / D-123 (the Tasks page this phase rebuilds + its `tasks.list` /
  `tasks.get` wire types)
- Phase 54 (the shipped task-control verbs the action bar consumes)
- Phase 60 / 72 (`events.subscribe` SSE) + Phase 72e (`pause.list`)
- Phase 108b (app-shell chrome) + Phase 108c (the carded vocabulary copied)
- Phase 108g (the Sessions `BottomDockTabs` run-scoped pattern + the DataTable
  sticky-header / clickable-row fix reused) + Phase 108h (the viewport-lock /
  carded Events pattern)

## Risks / open questions

- The dock tabs + cost are only as populated as the live event source. On an
  in-memory event driver a fresh subscription sees events going forward (limited
  historical backfill); a task whose run completed before the dock opened may
  show an honest empty Events tab. Surfaced in the empty copy, not hidden;
  generating activity while the dock is open (or a `durable` driver) fills it.
- The run-match predicate must include the `payload.TaskID` lifecycle case or it
  silently drops `task.*` events — locked by the `run-events` unit test against a
  captured real frame.
- The mode-switch must not orphan a stale subscription when the operator returns
  to the board or selects a different task — the dock's `$effect` closes the
  prior subscription on re-scope (the Sessions pattern).

## Glossary additions

None — reuses existing vocabulary (`TaskRegistry`, `GroupCompletion`, `Console`,
`Scope claim`, `Runtime lens`).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A; the run-scoped subscription carries the triple via the unchanged transport.
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, no reusable Go artifact built (Console-only page-polish).
- [ ] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — covered by the updated Playwright e2e + the run-events vitest; no Go seam touched.
- [ ] If new vocabulary: glossary updated — N/A.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — D-181 records the mode-switch composition + the token-type Cost Breakdown refinement.
