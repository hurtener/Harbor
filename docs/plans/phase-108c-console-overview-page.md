# Phase 108c — Console Overview page (visual rebuild + dead-code cleanup)

> Third wave of the Phase 108 page-polish series. Brings the **Overview** page
> to verbatim parity with its mock (`docs/rfc/assets/console-overview-page.png`),
> every datum wired to real Protocol data and verified live, governed by
> `docs/design/console/PAGE-POLISH-PROCEDURE.md` + `CONVENTIONS.md`. The current
> page is far from the mock — a flat counter row, a redundant right detail-rail
> the mock doesn't have, and a top FilterBar the mock replaces — so this wave is
> a **visual rebuild with code cleanup** (no dead code left behind), reusing the
> existing data layer where it's correct.

## Summary

The Overview's **data plumbing already exists** (Phase 73a / D-127): `runtime.counters`,
`runtime.health`, `pause.list`, the `events.subscribe` SSE fold (activity /
sparklines / cost), `approve`/`reject`, and Console-DB saved views. What diverges
from the mock is **presentation + layout + a few missing/incorrect panels**:

- bare counter cards (no sparkline / delta / status icon / per-card view-link);
- a right **DetailRail** (Runtime / Counters / Pending) the mock does **not** have;
- a top **FilterBar** (Save view + 1m/5m/15m + activity filter) the mock replaces
  with a runtime-context + alerts row (operator decision: rework — drop the strip,
  move "Save current layout" to Quick Links, keep a minimal sparkline window);
- **no alerts strip**, **no audit ribbon**;
- a **cost card** that claims a by-**agent** breakdown the event source cannot
  supply (latent bug — `llm.cost.recorded` carries Model, not agent).

This wave rebuilds the canvas to the mock, deletes the dead FilterBar/DetailRail
wiring, and fixes the cost dimension.

## RFC anchor

- **RFC §7** (Console layer), §7.1 runtime-lens principle (every panel is a
  projection over state + events + control — no privileged hooks), §7.2 IA.
- **page-overview.md** (the authoritative per-page spec) — §3 functionality
  matrix tags, §4 anatomy, §5 data-in/actions, §7 states, §12 mock reconciliation.
- **Decisions:** D-127 (Overview composition), D-061 (Console DB local-only —
  saved layout), D-065 (no session priority), D-066 (control-scope on
  Approve/Reject/Resume), D-079 (cross-tenant scope), D-026 (heavy-content), and
  the 108b chrome decision (see "Findings I'm departing from").

## Briefs informing this phase

- **Brief 11** §"Overview view", §"Layout decomposition", §CC-2 (identity-aware
  UI), §CC-3 (notifications — why the bell stays deferred).

## Findings I'm departing from (mock vs reality, verified live 2026-05-31)

1. **The mock's per-page top strip (search / `+New` / bell) is superseded by the
   global chrome (108b).** The RFC-era mock predates the chrome decision. The
   search, breadcrumb, and identity live in the app-shell top bar; the bottom
   AppStatusBar carries runtime name, Protocol, and Console version. **The page does NOT rebuild
   any of these.** (Operator-confirmed; see memory `feedback-console-chrome-supersedes-mock-topbar`.)
2. **Cost "by agent" has no source → by MODEL.** `llm.cost.recorded` carries
   `Model` / `Identity{Tenant,User,Session,Run}` / `Cost.TotalCost`, **no agent
   attribution** (verified live). The mock's "Research Agent / Ingestion Agent …"
   rows are illustrative; a faithful breakdown groups by **model** (real), with
   per-tenant as the admin elevation (D-079). Surfaced, not faked. (Also fixes the
   latent `cost.ts` `'agent'` path that keyed on an absent field — §17.6.)
3. **Counter delta badges ("+12% vs 10m ago") only where real.** `runtime.counters`
   is a point-in-time snapshot with no history; only **events/min** has a windowed
   series (`events.aggregate` / the SSE rate fold) from which a delta is real.
   Snapshot-only counters (tasks running / background jobs / MCP) render **without**
   a fabricated delta.
4. **Alerts strip + audit ribbon are real-wired but empty on a healthy dev
   runtime.** They subscribe to `governance.budget_exceeded` / `rate_limited`,
   `runtime.warning`/`error`, `bus.dropped`, `memory.health_changed` /
   `audit.admin_scope_used`. Verified the wiring; rows appear only when those
   events actually flow (never synthesised).
5. **No right detail-rail** (page-overview.md §4: "Right rail — empty on this
   page"). The current DetailRail (Runtime / Counters / Pending Interventions) is
   removed — its data is the counter row + (chrome) context, redundant.

## Console consistency (CONVENTIONS.md §9 + PAGE-POLISH-PROCEDURE.md)

- Routes at `/overview` under `(console)/` (no prefix); renders in the app shell.
- Uses `components/ui/` primitives; page-specific pieces stay in
  `components/overview/`. Talks to the Runtime ONLY via `HarborClient` +
  `connection.ts` — no hand-rolled fetch.
- Four-state `PageState` (Disconnected / Loading / Loaded / Empty/Error) at the
  page level + **nested** `PageState` per panel (health / queue / cost) — a panel
  can load/fail independently (CONVENTIONS §4). Disconnected via `isDisconnected()`.
- Tokens only (teal accent + Inter from 108b); no raw literals.
- **Depth-bar note (CONVENTIONS §5):** the Overview is a hub, not a catalog — it
  intentionally has no DataTable/right-rail/Pagination. The §5 depth bar is read
  through the page-overview.md §4 anatomy (counter row + panels + quick links),
  not the generic catalog shape. Documented so the audit doesn't flag it.

### Component inventory (mock → rebuild)

| Component | Mock | Source | Treatment |
|---|---|---|---|
| Runtime-context / audit row | acme-prod·healthy·v·Protocol \| Audit: admin scope used | chrome + `runtime.info`/`runtime.health`; `audit.admin_scope_used` events | Slim page row: health pill + **audit ribbon** (count + View in Events). Runtime name/version/protocol are chrome (not duplicated). |
| Alerts strip | dismissible banner row | `governance.*`, `runtime.warning/error`, `bus.dropped`, `memory.health_changed` events | **Build** — most-recent-per-type in last 5m; click → Events filtered; dismiss (local). Empty when none. |
| KPI counter card ×4 | number + delta + status icon + sparkline + "View X →" | `runtime.counters` + SSE rate fold (`aggregations.ts`) | **Rebuild** card: icon, value, sparkline, delta (events/min only), per-card deep-link. |
| Interventions panel | Type·Session·Requested·By·Reason + Approve/Reject/Resume | `pause.list` + `approve`/`reject`/`resume` | **Rebuild** as the mock table; control-gated buttons (D-066); "View all in Live Runtime". |
| Cost panel | $total + Δ% + stacked bar + per-row breakdown + selector | `llm.cost.recorded` SSE fold | **Rework** to **by-model** (real) + admin per-tenant; stacked bar + rows + total. |
| Recent activity | Time·Type·Description·Source·Session/Run | `events.subscribe` (session.opened/task.completed/task.failed/agent.*) | **Rebuild** as the mock table; row → entity; "View all events". |
| Quick Links | 2×3 tiles + Customize/Save layout | local nav + Console DB | Keep 6 tiles (no Evaluations — D-064); add **"Save current layout"** (relocated saved-views, D-061). |

### Per-datum source map (findings flagged)

| Datum | Source (verified live) | Result |
|---|---|---|
| events/min, tasks running, bg jobs, MCP healthy | `runtime.counters` | PASS (real) |
| counter sparkline | SSE event-rate fold (`aggregations.ts`) | PASS |
| counter delta % | `events.aggregate`/SSE window — events/min only | PASS (events/min); **omit** on snapshot counters (finding #3) |
| health pill | `runtime.health` (state/events/artifacts/memory) | PASS |
| audit ribbon count | `audit.admin_scope_used` events | PASS (empty in healthy dev) |
| alerts | `governance.*`/`runtime.*`/`bus.dropped`/`memory.health_changed` | PASS (empty in healthy dev) |
| interventions | `pause.list` (+ approve/reject/resume) | PASS |
| cost total + breakdown | `llm.cost.recorded` (Model/Cost/Identity) | by-**model** (finding #2 — no agent source) |
| recent activity | `events.subscribe` | PASS |
| quick links / save layout | local + Console DB | PASS |
| notifications bell | — | DEFERRED (no `notification.*` topic — finding) |

## Dead code to delete (no dead code left behind)

- Overview page: remove `FilterBar`, top `SavedViewChips` strip, `DetailRail` +
  `RailCard` usage, `Pagination` import (queue uses its own), the `activitySearch`
  facet + `counterWindow`-as-page-filter wiring (keep a minimal window control).
- Remove now-unused imports + state after the above.
- `components/overview/`: replace/rework `CounterCard` (rich), `CostRollupCard`
  (by-model), `InterventionQueue` + `RecentActivityFeed` (mock tables); delete any
  component left with zero consumers (e.g. `HealthChipStrip` if folded into the
  context row — decide during impl). `cost.ts`: drop the dead `'agent'` axis.
- Grep-prove zero dangling imports / unused exports after the cull
  (`svelte-check` + `eslint` no-unused gate).

## Acceptance criteria

1. Canvas matches the mock rows: context/audit + alerts → 4 KPI cards (sparkline +
   real delta where sourced + status + view-link) → interventions | cost(by-model)
   → recent activity → quick links + save-layout. No right rail, no top FilterBar.
2. Every datum real-wired & verified live (§3); no fabricated values; deferred
   items (bell, by-agent) explicitly absent, not faked.
3. Approve/Reject/Resume hit the real control verbs; disconnected/ no-scope →
   disabled-with-tooltip (D-066/D-160).
4. Four-state PageState + nested panel states all force-rendered (§5).
5. Viewport-locked, tokens only, zero console errors, hydration holds (§6/§7).
6. **No dead code**: svelte-check 0/0, eslint no-unused clean, no orphaned
   components/imports/exports; removed components deleted (git rm).
7. §8 ledger produced; §9 checkpoint audit FAIL-free.

## Files added or changed

- `web/console/src/routes/(console)/overview/+page.svelte` (rebuild)
- `web/console/src/lib/components/overview/*` (rework counter/cost/interventions/
  activity; add alerts strip + context/audit row; delete orphans)
- `web/console/src/lib/overview/cost.ts` (by-model; drop dead agent axis) + tests
- `web/console/tests/overview-page.spec.ts` (update to the rebuilt surface)
- `scripts/smoke/phase-108c.sh` (new, static)
- `docs/plans/phase-108c-console-overview-page.md` (this)

## Test plan

- Unit: `cost.ts` by-model projection against a captured real `llm.cost.recorded`
  frame; `aggregations.ts`/`activity.ts` retained tests still pass.
- Playwright: rebuilt `overview-page.spec.ts` — canvas rows present, counters
  render, intervention actions gated, cost by-model, no `.sidebar`-class right
  rail / no top FilterBar, disconnected honest, zero console errors.
- Live (§3–§7): against the validation agent — seed a task (cost/activity), force
  empty + (where possible) an alert.

## Smoke script additions

`scripts/smoke/phase-108c.sh` (static): rebuilt page references the panels,
DetailRail/FilterBar removed from the page, cost is by-model, deleted components
absent. Console-only — no new Runtime surface.

## Coverage target

Frontend: svelte-check 0/0, lint clean, unit + Playwright green. No Go change.

## Dependencies

None new. Builds on 108b chrome + the shipped 73a data layer.

## Risks / open questions

- **Cost dimension** (no agent source) — by-model chosen; confirm with operator.
- **Runtime-context row vs chrome overlap** — slim page row (health + audit) vs
  rely fully on chrome; confirm.
- **Alerts/audit empty in healthy dev** — wiring verified; real rows need a
  triggered condition (budget/rate-limit). Note in the ledger.

## Glossary additions

None.

## Per-component ledger (PAGE-POLISH-PROCEDURE §8 — verified live 2026-05-31)

Live env: youtube validation agent on :18080 + Console live source on :18790.
Zero console errors on a clean authenticated load.

| Component / datum | Source | Verified | Result |
|---|---|---|---|
| Context health pill | `runtime.health` | "all subsystems ready" (state/events/artifacts/memory) | PASS |
| Audit ribbon | `audit.admin_scope_used` events | "0× (24h)" (none in healthy dev) → View in Events | PASS (empty real) |
| Alerts strip | `governance.*`/`runtime.*`/`bus.dropped`/`memory.health_changed` | renders nothing (no in-window alerts) | PASS (empty real) |
| KPI: Events/min | `runtime.counters` + events-rate fold | value + real rate sparkline | PASS |
| KPI: Tasks/Jobs/MCP | `runtime.counters` (5s gauge sampler → ring buffer) | value + real sampled sparkline + real Δ (MCP showed +0% from 2 samples) | PASS |
| Cost total + bar + rows | `llm.cost.recorded` (Model/Cost) → by-model | $0.00 (no in-window cost events); Model/Agent selector flips axis | PASS (empty real) |
| Cost by-agent | — (no agent_id on event) | runtime/agent axis = 1 row (runtime label); genuine per-agent DEFERRED | finding (tracked) |
| Interventions | `pause.list` + approve/reject | empty ("No pending interventions") | PASS |
| Recent activity | `events.subscribe` | empty until events flow since subscribe | PASS (live-cursor) |
| Quick Links | local nav (6 tiles, no Evaluations) | renders; Tasks tile → /tasks | PASS |
| Customize overview | — | disabled-with-tooltip (personal layouts deferred §10) | PASS (honest) |
| Removed: top FilterBar, right DetailRail, +New | n/a | absent (asserted count 0) | PASS |

Tests: `cost.test.ts` 7/7 (model + runtime axes); rebuilt `overview-page.spec.ts`
6/6 vs the real embed; full e2e **157 passed / 0 failed** (incl. `disconnected-state`
N7 updated — Overview no longer carries a Save-view bar). §17.6: updated
`disconnected-state.spec.ts` (removed Overview from the per-page Save-view check).

Dead code deleted: `NewMenu.svelte`, `HealthChipStrip.svelte`,
`saved_filters_overview.ts` + its spec; `cost.ts` dead `agent`/`tenant` axes
replaced by real `model`/`runtime`. svelte-check 0/0, eslint no-unused clean.

**§9 checkpoint audit (read-only fork):** 0 FAIL, 2 WARN, 4 NIT. Both WARNs
landed — added `tests/alerts.test.ts` (projectAlerts + auditScopeCount, 7/7) and
corrected the stale `cost.ts` module header (per-agent/tenant → model/runtime) +
phase attribution. NIT 4 (alleged `audit.redaction_failed` not subscribed) was a
false positive — the page spreads `...ALERT_TYPES` into the subscription, so all
seven alert types stream. Remaining NITs (sparkline floor %, "Agent" label vs
`runtime` axis) are defensible and skipped.

## Pre-merge checklist

- [ ] svelte-check 0/0 · eslint no-unused clean (dead-code gate)
- [ ] lint clean (tokens) · Playwright + unit green
- [ ] drift-audit + phase smoke + check-mirror + markdownlint
- [ ] §8 ledger in PR · §9 audit FAIL-free
- [ ] deleted components have zero remaining references
