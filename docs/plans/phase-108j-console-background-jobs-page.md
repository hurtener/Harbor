# Phase 108j — console-background-jobs-page

## Summary

Phase 108j brings the Console **Background Jobs** page to verbatim-vs-mock parity
(look & feel + functionality) under the PAGE-POLISH procedure: it rethemes the
page to the carded, viewport-locked Events-108h composition (TABLE-primary on the
left + a right-rail detail on the right — the table stays visible, the rail shows
the selected job or an idle hint; NOT a Tasks-style mode-switch), deepens the
right rail to full mock fidelity (Details / Progress / Events / Control History
tabs + Artifacts-so-far / Parent task / Related Sessions sections), and refactors
the ~801-line king file into a `BackgroundJobsPageState` controller + pure
unit-tested projections (`derive.ts` / `orphan-detector.ts`) + focused rail
components, reusing the Tasks-108i `TaskRunStream` / `run-events` data layer
(never forked). The page ships NO new Protocol method — it stays a pure consumer
of `tasks.list` / `tasks.get` / `artifacts.list` / `events.subscribe` + the
shipped Phase 54 control verbs + the Console DB.

## RFC anchor

- RFC §6.8
- RFC §7

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- **brief 11 §"Background Jobs view":** Background Jobs is the focused queue UI
  for the background subset of `tasks.list` (the planner's `SpawnTask` work) with
  cancel / requeue / retry affordances and per-job progress drilldown — this
  phase keeps it a filtered projection (`kinds: ['background']`), never a parallel
  task store.
- **brief 11 §CC-4:** there is no shipped runtime-side `search.tasks`; the page's
  free-text search stays a Console-local substring match over the loaded page with
  honest copy (never a fabricated server search).
- **brief 12 §"The two-surface model":** the Console is a Protocol client — every
  queue row, detail, artifact and event round-trips through the Protocol; the
  `AwaitTask` orphan detector + the saved filters are Console-local derivations
  (D-061), adding no Protocol field and no Protocol call.
- **brief 12 §"shared UI":** the page composes the shared `ui/` inventory
  (`DataTable`, `FilterBar`, `BulkActionBar`, `Pagination`, `PageState`,
  `SavedViewChips`) and never forks a primitive.

## Findings I'm departing from (if any)

None.

## Goals

- Retheme the page to the carded `.panel.card` + `.panel-title` vocabulary the
  seven done pages set; drop the per-page `PageHeader` (the breadcrumb / ⌘K /
  footer are 108b app-shell chrome).
- Viewport-lock the page (PAGE-POLISH §6): the document never full-page-scrolls;
  only the queue table (sticky `<thead>`) and the right rail scroll internally.
- Deepen the right rail to full mock fidelity, wiring Events / Control History to
  the run-scoped `events.subscribe` stream and Progress to `task.progress` + a
  derived ETA + the run's `task.*` lifecycle timeline.
- Refactor the king file into a controller + pure projections + focused rail
  components; delete the orphaned pre-chrome `RightRail.svelte`.
- Render every genuine V1 gap as an honest state — never a fabricated value.

## Non-goals

- No new Protocol method (no `search.tasks`, no `tasks.spawn_background` force,
  no `parent_alive` runtime field — the orphan detector stays Console-side).
- No durable background-job persistence (D-006 — in-process at V1).
- No queue-wide live SSE delta feed — the page is load + Refresh; only the
  per-job rail opens a run-scoped subscription.

## Acceptance criteria

- [ ] The page routes under `(console)/background-jobs/` (no `/console/` prefix),
      renders inside the app shell, and carries NO per-page `PageHeader`.
- [ ] Carded, viewport-locked: `scrollHeight == innerHeight` (no full-page
      scroll) and the queue table has no horizontal overflow at the supported
      width; the table + rail scroll internally.
- [ ] The queue is the `kinds: ['background']` projection of `tasks.list`; rows
      render Job ID (short) / Title / Parent session / Status / Progress /
      Started / Last activity / Tags + a derived type badge + the orphan badge.
- [ ] Clicking a row opens the right rail; its tabs (Details / Progress / Events /
      Control History) navigate, and the Artifacts / Parent task / Related
      Sessions sections render real-or-honest-empty data.
- [ ] Bulk Cancel / Pause / Resume / Prioritize invoke the shipped Phase 54 verbs
      per selected row, control-scope gated (disabled-with-tooltip without the
      claim) — no stubbed action.
- [ ] All four `PageState` branches render (Disconnected / Loading / Error /
      Empty); the four states are observed live.
- [ ] ETA / type / spawned-by / artifacts / related-sessions / events render
      honest values or honest-empty — never fabricated.
- [ ] `npm run check` 0/0, `npm run lint` clean, `npm run test` green (incl. the
      new `derive.test.ts`), the static smoke sweep + drift-audit pass, and the
      Playwright `background-jobs-page.spec.ts` passes against the rebuilt page.

## Files added or changed

```text
web/console/src/routes/(console)/background-jobs/+page.svelte   # rebuilt (thin view)
web/console/src/lib/background-jobs/state.svelte.ts             # NEW controller
web/console/src/lib/background-jobs/derive.ts                   # NEW pure projections (ETA / type / timeline)
web/console/src/lib/background-jobs/derive.test.ts              # NEW vitest
web/console/src/lib/background-jobs/orphan-detector.ts          # unchanged (kept)
web/console/src/lib/components/background-jobs/JobDetailRail.svelte   # NEW rail (replaces RightRail.svelte)
web/console/src/lib/components/background-jobs/JobProgressTab.svelte  # NEW progress tab
web/console/src/lib/components/background-jobs/QueueTable.svelte      # retheme (short id, type badge, compact)
web/console/src/lib/components/background-jobs/RightRail.svelte       # DELETED (superseded)
web/console/src/lib/components/background-jobs/{BulkToolbar,OrphanBadge,SavedFilterChips}.svelte  # kept
web/console/tests/background-jobs-page.spec.ts                  # rewritten for the new structure
scripts/smoke/phase-108j.sh                                     # NEW (static-only)
docs/plans/phase-108j-console-background-jobs-page.md           # this plan
docs/design/console/page-background-jobs.md                     # §13 reframe note
docs/decisions.md                                               # D-182
```

## Public API surface

N/A — this is a Console (SvelteKit) page-polish phase. It adds no Go Protocol
surface. The TypeScript surface other modules could import: the
`BackgroundJobsPageState` controller and the pure `derive.ts` projections
(`deriveETA` / `deriveJobType` / `projectStateTimeline`) — both Console-internal.

## Test plan

- **Unit:** `web/console/src/lib/background-jobs/derive.test.ts` (Vitest) locks
  the ETA / type / timeline projections against their honest states (no-progress
  → "Unknown"; no-signal → "Job"; newest-first events → oldest-first timeline);
  the existing `orphan-detector.test.ts` is unchanged.
- **Integration:** the Playwright `background-jobs-page.spec.ts` exercises the
  page end-to-end against a live `harbor console` Runtime (hydration, the carded
  filter strip, the background-kind queue, bulk-select → toolbar, row → rail tab
  navigation, the orphan dialog, control-scope gating, viewport-lock, and the
  disconnected redirect). It reuses real Protocol surface — no mock at the seam.
- **Conformance:** N/A — no Go driver surface touched.
- **Concurrency / leak:** N/A — no Go reusable artifact built. The reused
  `TaskRunStream` closes its subscription on rail re-scope / unmount (its leak
  contract is owned by Phase 108i).

## Smoke script additions

`scripts/smoke/phase-108j.sh` (static-only) asserts: the route file exists; the
per-page `PageHeader` is gone; the carded `.panel.card` + `.panel-title`
vocabulary is present; the `BackgroundJobsPageState` controller + `derive.ts` +
`JobDetailRail.svelte` + `JobProgressTab.svelte` exist; the deleted
`RightRail.svelte` is gone; the load-bearing testids / exported symbols are
present; the `Save view` button + `Save current as…` input contract (phase-83s /
disconnected-state N7) is preserved; `DISCONNECTED_TOOLTIP` is used.

## Coverage target

`web/console/src/lib/background-jobs/`: the pure projections (`derive.ts`,
`orphan-detector.ts`) are covered by Vitest. The controller + `.svelte`
components are covered by the Playwright e2e against a live Runtime (the Console
coverage convention — no per-package % gate on `.svelte`).

## Dependencies

- Phase 73h (the page this rebuilds) — Shipped.
- Phase 73d (`tasks.list` / `tasks.get` wire types) — Shipped.
- Phase 54 (the control verbs) — Shipped.
- Phase 73l (`artifacts.list`) — Shipped.
- Phase 60 / 72 (`events.subscribe` SSE) — Shipped.
- Phase 108b (app-shell chrome), 108c (carded vocabulary), 108h (viewport-locked
  carded Events pattern), 108i (the `TaskRunStream` / `run-events` data layer +
  the DataTable sticky-header / clickable-row fix) — Shipped.

## Risks / open questions

- **The run-scoped SSE is live-only** (no backlog on a fresh connect), so the rail
  Events / Control History tabs + the Progress state-timeline render honest-empty
  for a quiet or already-finished run. This is the same constraint the Sessions /
  Tasks docks carry; it is surfaced in honest copy, not hidden.
- **Artifacts-so-far filters by `scope.task` = the job's run id** (spec §3 / §12).
  A job that produced no task-scoped artifact renders honest-empty (verified live
  on the seeded runtime, whose fixture artifacts are session-scoped, not
  task-scoped).
- **ETA is the planner's own progress projected** — it is honest but only as good
  as the planner's progress emission; with no hint it reads "Unknown".

## Glossary additions

None — no new vocabulary (Background Jobs / TaskGroup / orphan / saved view are
already in the glossary).

## Console consistency (CONVENTIONS.md §9 + PAGE-POLISH-PROCEDURE.md)

This phase is built against the binding Console design system (D-121,
`docs/design/console/CONVENTIONS.md`) and passes the page-polish verification
gate (`docs/design/console/PAGE-POLISH-PROCEDURE.md`). The page:

- routes under `(console)/background-jobs/` with **no `/console/` URL prefix**
  (CONVENTIONS.md §1) and renders inside the shared app shell;
- uses the `components/ui/` inventory (`DataTable`, `FilterBar`, `BulkActionBar`,
  `Pagination`, `PageState`, `SavedViewChips`) and **does not fork a primitive**
  — the page-specific pieces (`QueueTable`, `BulkToolbar`, `OrphanBadge`,
  `SavedFilterChips`, `JobDetailRail`, `JobProgressTab`) compose `ui/` underneath,
  and the run-stream data layer is the reused `lib/tasks/` modules (§3);
- routes all async state through the four-state `PageState` (§4) — Disconnected
  is never conflated with Error; the rail detail loads behind its own gate;
- clears the §5 depth bar (FilterBar + DataTable + DetailRail-equivalent +
  Console-DB SavedViewChips + real Pagination + the app-shell ConnectionFooter +
  four-state PageState);
- talks to the Runtime only through `HarborClient` + `connection.ts` (§6) — no
  hand-rolled `fetch`, no direct `localStorage`;
- introduces no raw token literals (§7) — design tokens only, `stylelint` clean.
- Per PAGE-POLISH §0, every datum is real-wired and verified live (the §8
  per-datum ledger ships in the PR), and the four `PageState` branches + the
  viewport-lock + zero console errors were observed in the browser (§3–§7).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes
- [x] `make check-mirror` passes (no AGENTS.md / CLAUDE.md change)
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (pure projections unit-tested)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (no Go path changed; the page is identity-scoped via the shipped `tasks.list` / SSE session scoping)
- [x] **If this phase builds a reusable artifact: concurrent-reuse test** — N/A: this is a Console page-polish phase; it builds no Go reusable artifact. The reused `TaskRunStream` carries its Phase 108i contract.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists** — the Playwright `background-jobs-page.spec.ts` exercises the page against a live Runtime over the real Protocol surface (no mock at the seam).
- [x] If new vocabulary: glossary updated — N/A (no new terms)
- [x] If a brief finding was departed from: justified + decisions entry — N/A (none)
