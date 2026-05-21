# Phase 73a — Console Overview page (Protocol + UI bundled)

## Summary

Bundles the Overview page into a single phase per the Wave 13 decomposition (`docs/plans/wave-13-decomposition.md` §5). The Overview page composes existing Stage-1 primitives — `runtime.counters`, `runtime.health`, `pause.list`, `notification.*` events, `tasks.list`, `events.subscribe` — rather than introducing its own catalog Protocol methods; the heavy lift is the Svelte UI + the `+ New` quick-create menu + the operator-facing posture aggregation. Stage 2.3 phase (depends on 72d/72e/72f from Stage 1 plus 73d Tasks from Stage 2.2).

## RFC anchor

- RFC §5.2 (streaming events row)
- RFC §6.13 (typed event bus)
- RFC §6.15 (Governance — cost ceilings + rate limits + MaxTokens)
- RFC §7 (Console layer)

## Briefs informing this phase

- brief 11 (Console feature surface — "Overview view", §CC-3 notifications)
- brief 12 (deployment + two-surface model)

## Brief findings incorporated

- brief 11 §"Overview view": "the operator's at-a-glance hub — counter cards, alert ribbon, recent activity feed, cost rollup, quick-links grid". The page is composition over primitives, NOT a new catalog surface. Implementation here follows that exact shape.
- brief 11 §CC-3: notifications are a rules-engine-lite mapper from the event taxonomy to a notification class. The Overview's alert ribbon is the first UI consumer of `notification.*` — the Phase 72d test consumer satisfies the §13 primitive-with-consumer rule independently of this page.
- brief 12 §"shared chat / playground library": the Overview's deep-links into Sessions / Tasks / Background Jobs / Agents / Tools / Settings MUST route through the Console's typed Protocol client (D-093 generated `protocol.ts`), never hand-rolled `fetch`.

## Findings I'm departing from (if any)

None on design. One path-resolution correction (D-127): this plan was
authored before D-121 (`docs/design/console/CONVENTIONS.md`) landed and
names the route at `web/console/src/routes/overview/+page.svelte` with a
`/console/overview` URL. `CONVENTIONS.md` §1 is the binding cross-cutting
authority — Console pages route under the `(console)` SvelteKit group
with **no `/console/` URL prefix**. The page therefore ships at
`web/console/src/routes/(console)/overview/` and is served at
`/overview`; the components live at
`web/console/src/lib/components/overview/`. The "Files added or changed"
block and the smoke-script `/console/overview` reference below read with
that correction applied (CLAUDE.md §15 — a plan that contradicts a
higher-priority artifact yields to it).

## Goals

- Ship a complete, mockup-aligned Overview page (`/console/overview`) at `web/console/src/routes/overview/+page.svelte`.
- Compose `runtime.counters` + `runtime.health` + `pause.list` + `notification.*` + `tasks.list` + `events.subscribe` into the 4-card counter row + sub-header health-chip strip + cost rollup + intervention queue + recent activity feed + Quick Links grid + footer.
- Add the `+ New` quick-create menu (Console-local navigation surface; menu items deep-link to per-page create flows owned by other phases).
- The page renders in the no-control-scope (observation-only) operator view; Approve / Reject buttons in the intervention queue invoke shipped `approve` / `reject` Protocol methods (Phase 54) gated by D-066 control-scope.
- Per-page Playwright spec at `web/console/tests/overview-page.spec.ts`.

## Non-goals

- New Protocol method on the runtime side. The Overview is composition only.
- Cross-runtime aggregation. D-091 — post-V1.
- Per-tenant cost dashboards beyond the rollup card. Deeper cost subsystem is post-V1.
- Anomaly detection / alert rules. Post-V1 per page-overview.md §10.

## Console consistency

This is a Console page phase. It is **binding** on the shared Console
design-system foundation defined in `docs/design/console/CONVENTIONS.md`
(D-121 in `docs/decisions.md`). `CONVENTIONS.md` is the cross-cutting
authority for every Console page; a page PR that diverges from a convention
below is **rejected on sight**. The Overview page is composition over Stage-1 primitives, not a new catalog surface — its counter cards, intervention queue, and Quick Links grid all render inside the shared shell and clear the depth bar like every other page.

The page MUST:

- **Route under `(console)/`.** The page lives at
  `web/console/src/routes/(console)/overview/` and is served at `/overview` with
  **no `/console/` URL prefix** (the `(console)` route group is a
  layout-grouping device and does not appear in the URL). Detail views live at
  `(console)/overview/[id]/` and are served at `/overview/<id>`. All inter-page
  links use the unprefixed form; a link to `/console/<anything>` is a bug.
- **Render inside the shared app shell.** The page renders as a child of
  `(console)/+layout.svelte` — the single app shell carrying the sidebar,
  breadcrumb, identity/connection indicator, and footer. It never ships a
  standalone layout.
- **Use the shared `components/ui/` inventory.** It composes the cross-page
  primitives in `web/console/src/lib/components/ui/` — `PageHeader`,
  `FilterBar`, `DataTable`, `DetailRail`/`RailCard`, `BulkActionBar`,
  `SavedViewChips`, `Pagination`, `StatusChip`, `ConnectionFooter`,
  `PageState`. It **never forks a primitive that already exists**;
  page-specific components go in `components/overview/`.
- **Route all async state through the four-state `<PageState>`.** Every async
  surface flows through `<PageState>`'s four mutually-exclusive states —
  Disconnected / Loading / Error / Empty. The Error state ships a working
  **Retry** that re-invokes the loader and suppresses any stale primary view;
  **Disconnected** ("no Runtime attached") is detected via `connection.ts`
  returning `null` and is **never conflated with Error**.
- **Clear the §5 depth bar.** The page is not "done" until it has all of:
  a `PageHeader`; a `FilterBar`; a primary `DataTable` or canvas; a
  `DetailRail` or a tabbed detail route; Console-DB-backed `SavedViewChips`;
  real `Pagination` (page / size / total, prev / next — not a fake "load
  more"); a `ConnectionFooter`; and the full four-state `PageState`.
- **Talk to the Runtime only through `HarborClient` + `connection.ts`.** All
  Protocol calls go through the single typed `HarborClient` (adding a
  namespace, never a new top-level client); the connection resolves through
  `web/console/src/lib/connection.ts`. **No `fetch` in `.svelte` files, no
  direct `localStorage` access, no hand-rolled per-page client.**
- **Introduce no raw token literals.** No raw color / spacing / type-scale
  literals in `.svelte` files — design tokens from `tokens.css` only
  (Stylelint enforces this; `npm run lint` fails CI on a violation).
- **Ship no stubbed action presented as done.** Every action either invokes
  the real Protocol method or renders **disabled-with-tooltip** explaining
  why. A button that fakes success with a feedback string is a §13-class
  silent-degradation violation.

See `docs/design/console/CONVENTIONS.md` §9 for the per-phase callout
contract and D-121 for the rationale.

## Acceptance criteria

- [x] `web/console/src/routes/overview/+page.svelte` renders the 4-card counter row (Events/min, Tasks Running, Background Jobs, MCP Connections), the sub-header health-chip strip, the cost rollup card, the intervention queue (composed from `pause.list`), the recent activity feed (composed from `events.subscribe`), the Quick Links 2x3 grid, the `+ New` quick-create menu, and the footer.
- [x] All data flows go through the typed Protocol client at `web/console/src/lib/protocol.ts` (D-093). NO hand-rolled `fetch` calls in `.svelte` files.
- [x] Counter-card sparklines render windowed event-rate aggregation client-side from the `events.subscribe` cursor — no new Protocol method.
- [x] Health sub-header strip renders chip-per-subsystem from `runtime.health` (Stage 1 Phase 72f) + per-driver `*.health_changed` events.
- [x] Intervention queue uses `pause.list` (Stage 1 Phase 72e) filtered by the operator's identity scope; cross-tenant view requires admin scope per D-079.
- [x] Cost rollup card renders per-agent breakdown by default; per-tenant view is admin elevation (operator's existing scope claim determines which appears).
- [x] Approve / Reject buttons invoke EXISTING shipped Protocol methods (`approve`, `reject` — Phase 54). No parallel implementation (§13). Buttons gated by `tasks.control` / `tools.approve` claims (D-066) and degrade to disabled-with-tooltip when missing.
- [x] `+ New` menu items deep-link into per-page create flows (the actual create routes are owned by their page's phase plan — Overview only provides the menu).
- [x] Quick Links grid contains exactly 6 tiles: Sessions, Tasks, Background Jobs, Agents, Tools, Settings. No `/console/evaluations` tile (D-064 post-V1).
- [x] Footer renders `Connected to <runtime> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`.
- [x] Design tokens only — no raw color/spacing/type-scale literals in `.svelte` files (§13 + Stylelint enforcement).
- [x] `svelte-check --fail-on-warnings` passes (no Svelte 4 reactivity syntax per D-092).
- [x] Per-page Playwright spec `web/console/tests/overview-page.spec.ts` covers: (a) initial load renders all panel skeletons, (b) counter cards populate from event stream, (c) intervention queue renders pause.list rows with Approve / Reject buttons hidden when scope absent, (d) Quick Links navigation works, (e) `+ New` menu deep-links resolve.
- [x] `scripts/smoke/phase-73a.sh` asserts the page route returns 200 (gated on `harbor console` being up; SKIPped until 73m).

## Files added or changed

```text
web/console/src/routes/overview/+page.svelte
web/console/src/lib/components/overview/CounterCard.svelte
web/console/src/lib/components/overview/CounterCardSparkline.svelte
web/console/src/lib/components/overview/HealthChipStrip.svelte
web/console/src/lib/components/overview/CostRollupCard.svelte
web/console/src/lib/components/overview/InterventionQueue.svelte
web/console/src/lib/components/overview/RecentActivityFeed.svelte
web/console/src/lib/components/overview/QuickLinksGrid.svelte
web/console/src/lib/components/overview/NewMenu.svelte
web/console/src/lib/components/overview/Footer.svelte
web/console/src/lib/overview/aggregations.ts     # client-side event-rate aggregation (no Protocol mutation)
web/console/tests/overview-page.spec.ts
scripts/smoke/phase-73a.sh
docs/glossary.md                                  # no new vocabulary expected; verify
```

## Public API surface

No new Go-side surface; this phase is UI composition over existing Protocol methods. The SvelteKit page consumes:

- `runtime.counters` (Phase 72f) — counter card values.
- `runtime.health` (Phase 72f) — health-chip strip.
- `pause.list` (Phase 72e) — intervention queue rows.
- `notification.*` events (Phase 72d) — alert ribbon items.
- `tasks.list` (Phase 73d) — Tasks Running counter detail drill-through.
- `events.subscribe` with filter (Phase 72a) — recent activity feed + counter sparklines.
- `approve` / `reject` (Phase 54 — shipped) — intervention queue Approve / Reject actions.

## Test plan

- **Unit:**
  - `web/console/src/lib/overview/aggregations.ts` unit tests via `vitest` — event-rate aggregation correctness across window boundaries (1m / 5m / 15m windows).
- **Integration:**
  - N/A — no new Go-side surface. The integration assurance comes from the page's Playwright spec running against a live `harbor console` instance backed by a real runtime, and from the upstream Phase 73d's `tasks.list` integration test.
- **Conformance:**
  - N/A.
- **Concurrency / leak:**
  - N/A — no new reusable Go artifact.
- **UI (Playwright):**
  - `overview-page.spec.ts` — page load returns 200; all panels render skeleton then populate; counter cards update on event injection; intervention queue Approve hides when scope missing; `+ New` menu deep-links resolve to the right routes; footer chips reflect connection state.

## Smoke script additions

`scripts/smoke/phase-73a.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'runtime/counters' '{}'` (the upstream Stage-1 method) → stub SKIPped until 72f lands.
- `protocol_call 'pause/list' '{}'` → stub SKIPped until 72e lands.
- Page route probe: `skip_if_404 "$(api_url /console/overview)"` → SKIPped until 73m's `harbor console` subcommand lands.

## Coverage target

- `web/console/src/lib/overview/`: 75% (via `vitest`).
- `web/console/src/routes/overview/`: 70% (via `svelte-check` + Playwright).

## Dependencies

**Same-wave (Wave 13):**

- Phase 72d (notification.* event topic — alert ribbon)
- Phase 72e (pause.list snapshot — intervention queue)
- Phase 72f (runtime.counters + runtime.health — counter row + health chips)
- Phase 72a (events.subscribe filter — recent activity feed + sparklines)
- Phase 73d (tasks.list — Tasks Running counter + drill-down)
- Phase 75 (Playwright harness baseline)

**Already shipped (pre-Wave 13):**

- Phase 54 (Protocol task control surface — supplies `approve` / `reject`)
- Phase 60 (Protocol wire transport)
- Phase 61 (Protocol auth + scope claims)

## Risks / open questions

- **Sparkline aggregation accuracy under high event rates.** Client-side aggregation from the `events.subscribe` cursor: if events fire faster than the SvelteKit page can drain the cursor, sparklines under-count. Mitigation: use the time-bucketed counts from 72a's `events.aggregate` as the canonical sparkline source; the local cursor only carries the "since last bucket" delta. Documented in 72a's plan as the bucketing model.
- **Intervention queue's `pause.list` cost.** For a runtime with many concurrent paused tasks, `pause.list` could return a large payload. Mitigation: 72e ships pagination + identity-scope filter. The Overview always paginates to the top N (default 10); a "View all" deep-link sends the operator to a dedicated page (deferred post-V1; for V1 the deep-link goes to the Tasks page filtered by `status=paused`).
- **`+ New` menu's `Connect runtime` item.** That action requires writing into Console DB (D-061) via the runtime-registry table (Phase 72h schema). If 72h ships a different schema than this plan assumes, the menu item lands as disabled-with-tooltip ("Coming with Settings").

## Glossary additions

- No new vocabulary expected. Verify no existing glossary entries collide.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] `make protocol-ts-gen-check` passes (no Go-side type changes here, but the gen check runs regardless per CI shape)
- [x] `svelte-check --fail-on-warnings` passes (no Svelte 4 reactivity syntax per D-092)
- [x] `npm run lint` passes in `web/console/` (no raw color / spacing literals per §13)
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: N/A — Overview is observation-only, all data goes through identity-scope-aware upstream Protocol methods
- [x] **Concurrent-reuse test:** N/A — no new Go-side reusable artifact (this phase is UI only)
- [x] **Integration test:** N/A — no new Go-side seam; UI Playwright spec covers the cross-stack integration end-to-end
- [x] **Per-page Playwright spec lands in this phase's PR** — `web/console/tests/overview-page.spec.ts` exists and passes
- [x] Glossary updated (no additions expected, but verified)
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase)
- [x] **Coordinator-verify pass complete** before the PR is opened for operator review (decomposition doc §12 lock-in)
