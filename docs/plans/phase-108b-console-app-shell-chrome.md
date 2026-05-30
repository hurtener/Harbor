# Phase 108b — Console app-shell chrome (sidebar + top bar + status bar)

> Second wave of the Phase 108 page-polish series (after 108 / 108a Playground).
> Brings the **app shell chrome** — the persistent sidebar, the top bar, and the
> global status bar that render on **every** Console page — to verbatim parity
> with the canonical mock, with every datum wired to real Protocol data and
> verified end-to-end against a live Runtime. Highest ROI in the series: the
> chrome renders on all 14 pages.

## Summary

The app shell (`web/console/src/routes/(console)/+layout.svelte`) is the frame
every page renders inside (CONVENTIONS.md §2). Against the canonical mock
(`docs/rfc/assets/console-overview-page.png`) and verified live (2026-05-30) the
current chrome diverges in three regions:

1. **Sidebar** — too wide (385px / 20.1% of a 1920px viewport vs the mock's
   compact ~200px / ~13%), text-only nav items (the mock gives each item a
   leading icon), a single-line "Harbor Console" wordmark (the mock stacks
   **Harbor** over **CONSOLE**), and an active state with no icon treatment.
2. **Top bar** — carries only a breadcrumb + scope chip + status dot. The mock's
   hamburger (sidebar collapse), global search, identity avatar, (and the
   bell / theme / help affordances) are absent.
3. **Status bar** — the global `AppStatusBar` (108a) is correct and real-wired,
   but two pages (Overview, Live Runtime) still import page-local footer strips
   (`overview/Footer.svelte`, `live-runtime/footer.svelte`) that duplicate it.
   The status bar is now chrome; per-page copies are removed.

This wave fixes all three to the mock, real-wiring every datum or explicitly
deferring it (never faking), and consolidating the status bar into the chrome.

## RFC anchor

- **RFC §1** — the Console is a Protocol client; the shell is its frame.
- **RFC §7** — Console layer; every page is a projection over state + events +
  control (D-062). The chrome introduces no privileged hooks.
- **Decisions:** D-121 (Console design-system conventions / app shell), D-159
  (Playground sidebar entry + breadcrumb derives from NAV), D-064 (Evaluations
  is post-V1 — no sidebar entry), D-091 (multi-runtime; `harbor console`
  serves the shell), D-171 (per-request session; connection token carries
  tenant+user, session blank), D-160 (disconnected-state hygiene + the
  `isDisconnected()` predicate), D-026 (heavy-content bypass — search rows).

## Briefs informing this phase

- **Brief 11** (`docs/research/11-console-feature-surface.md`) — §"Layout
  decomposition" (sidebar / top bar / main viewport / bottom dock), §CC-2
  (identity-aware UI), §CC-4 (search), §CC-5 (keyboard nav).
- **Brief 12** (`docs/research/12-console-deployment-and-shared-ui.md`) — the
  shared-UI inventory and the `harbor console` deployment posture.

## Brief findings incorporated

- Brief 11 §"Layout decomposition" — the four shell regions (sidebar / top bar /
  main viewport / bottom dock). This wave finishes the sidebar + top bar +
  bottom dock; the main viewport is each page's own wave.
- Brief 11 §CC-4 — global search across sessions/tasks/events/artifacts. Wired
  to the shipped `search.query` method (the ⌘K launcher).
- Brief 11 §CC-2 — identity-aware UI; the top-bar avatar + connection popover
  surface the resolved `(tenant, user, session)` triple from `connection.ts`.

## Findings I'm departing from (if any)

- **The mock's sidebar "Harbor Runtime" status card** (version · Healthy ·
  Uptime · Events/sec · Connected Gateways) is **not built.** Two reasons, both
  binding: (a) `page-overview.md` §12 explicitly supersedes the sidebar
  "counter strip" — those counters move to the Overview main canvas and the
  footer carries connection posture only; (b) the data (uptime / events-per-sec
  / connected-gateways) has **no shipped Protocol source** — `runtime.snapshot`
  / `runtime.counters` is `[wave-13-extends]` per `page-overview.md`. Rendering
  it in the nav would be fabrication (PAGE-POLISH-PROCEDURE §1). Documented here,
  not silently dropped.
- **The mock's "Evaluation → Evaluations" cluster** is **not built** — D-064
  pins Evaluations as a post-V1 subsystem. The mock (2026-05-18) predates the
  decision; the current omission is correct.
- **The mock omits Playground** from the sidebar; we **keep** it in the
  Execution cluster — D-159 supersedes the mock (Playground is a Console-bound
  surface and a sidebar entry).
- **The mock's notification bell** (badge "3") and **theme toggle** (sun) are
  **omitted** (operator decision, 2026-05-30): no notifications Protocol feed
  exists, and `tokens.css` is dark-only (no light palette to toggle). Both are
  tracked as deferred chrome items below rather than rendered as dead controls.

## Goals

1. Sidebar at the mock's compact width via a dedicated `--size-nav` token (the
   sidebar must stop borrowing the detail-rail `--size-rail` width).
2. Per-item nav icons via **lucide-svelte** (the chosen icon library; RFC §10
   dependency addition — see Dependencies).
3. Two-line brand lockup (**Harbor** / **CONSOLE**) beside the lighthouse mark.
4. Active-state parity (accent pill + accent left-border + accent icon/label).
5. Top-bar chrome: hamburger sidebar-collapse toggle, the breadcrumb, a global
   ⌘K search launcher (`search.query`), an identity avatar + connection
   popover, and a help → docs link.
6. Consolidate the status bar into the chrome: the single global `AppStatusBar`
   is the only bottom bar; remove the page-local footer strips.
7. Tokens only; zero raw literals; viewport-locked shell; zero console errors;
   the four `PageState` branches still resolve through the chrome.

## Non-goals

- Light/dark theming (a full second token palette — its own wave).
- A notifications subsystem / bell feed (no Protocol source in V1).
- The sidebar runtime status card / counters (superseded by `page-overview.md`
  §12; counters live on the Overview canvas).
- The multi-runtime switcher dropdown body (D-091 `~/.harbor/console.yaml`
  fan-out) — the status-bar "Connected to <runtime>" chevron is rendered, but
  the full attached-runtime switcher list is each Settings/Overview wave's job;
  here it routes to Settings.
- Moving page titles/subtitles into the top bar — `PageHeader` (CONVENTIONS.md
  §3) stays the owner of per-page title + subtitle + actions.
- Refactoring any individual page's main content (each page has its own wave).

## Console consistency (CONVENTIONS.md §9 + PAGE-POLISH-PROCEDURE.md)

This wave is governed by `docs/design/console/CONVENTIONS.md` and verified
through `docs/design/console/PAGE-POLISH-PROCEDURE.md` (§3 wire-by-wire, §4
functional, §5 four-state, §6 shell/layout, §7 browser-truth, §8 ledger, §9
audit). The chrome:

- is **the** `(console)/+layout.svelte` app shell every page renders inside
  (§2) — no `/console/` URL prefix; the `(console)` group is layout-only (§1);
- uses `components/ui/` primitives (`AppStatusBar`) and adds new chrome
  components under `components/ui/` (the shell inventory), forking no existing
  primitive (§3);
- routes every inter-page link via the unprefixed form (`/overview`, `/tools`,
  …) and derives the breadcrumb label from the `NAV` constant (D-159) (§1);
- talks to the Runtime only through `HarborClient` + `connection.ts` — the
  search launcher adds a `client.search.query(...)` namespace method, no
  hand-rolled `fetch` (§6);
- routes disconnected state through `isDisconnected()` / `DISCONNECTED_TOOLTIP`
  (D-160) — search + avatar popover degrade to disabled-with-tooltip when no
  Runtime is attached (§4/§5);
- introduces a new **`--size-nav`** sizing token (the sidebar width) in
  `tokens.css` in-place, grouped under sizing — NOT a per-phase append block
  (§7). Reconciliation with §7's "exactly one rail-width token (`--size-rail`)":
  §7 forbids *variants of the same concept*; the nav sidebar width and the
  detail-rail width are **distinct surfaces** (the sidebar was incorrectly
  borrowing `--size-rail`, which is why it rendered 352px wide). `--size-rail`
  stays the single detail-rail token; `--size-nav` is the single nav-width
  token. No raw literals enter any `.svelte` file.

### Component inventory (mock → chrome)

| Region | Component / element | Mock | Treatment |
|---|---|---|---|
| Sidebar | Brand lockup | lighthouse + **Harbor** / **CONSOLE** | Two-line wordmark beside `harbor_logo.svg` |
| Sidebar | Cluster label `RUNTIME` | uppercase muted | kept |
| Sidebar | Cluster label `EXECUTION` | uppercase muted | kept |
| Sidebar | Cluster label `RESOURCES` | uppercase muted | kept |
| Sidebar | Cluster label `EVALUATION` | present | **omitted (D-064)** |
| Sidebar | Cluster label `SETTINGS` | uppercase muted | kept |
| Sidebar | Item: Overview (icon) | home/grid icon | lucide `LayoutDashboard` |
| Sidebar | Item: Live Runtime | icon | lucide `Activity` |
| Sidebar | Item: Sessions | icon | lucide `MessagesSquare` |
| Sidebar | Item: Tasks | icon | lucide `ListChecks` |
| Sidebar | Item: Agents | icon | lucide `Bot` |
| Sidebar | Item: Tools | icon | lucide `Wrench` |
| Sidebar | Item: Events | icon | lucide `Radio` |
| Sidebar | Item: Background Jobs | icon | lucide `Layers` |
| Sidebar | Item: Playground | (absent in mock) | lucide `FlaskConical` — **kept (D-159)** |
| Sidebar | Item: Flows | icon | lucide `Workflow` |
| Sidebar | Item: Memory | icon | lucide `Brain` |
| Sidebar | Item: MCP Connections | icon | lucide `Plug` |
| Sidebar | Item: Artifacts | icon | lucide `Package` |
| Sidebar | Item: Settings | icon | lucide `Settings` |
| Sidebar | Active state | accent pill + left-border + icon | parity |
| Sidebar | Width | compact ~200px | `--size-nav` (~13rem) |
| Sidebar | Status card (footer) | runtime stats card | **omitted** (page-overview §12; no source) |
| Top bar | Hamburger | collapse toggle | **built** (local UI state, persisted) |
| Top bar | Breadcrumb | `<runtime> / <Page>` | kept (derived from NAV) |
| Top bar | Search "⌘K" | global search | **built** → `search.query` launcher |
| Top bar | Bell (badge "3") | notifications | **omitted/deferred** (no feed) |
| Top bar | Help "?" | — | docs link |
| Top bar | Theme toggle | sun | **omitted/deferred** (dark-only) |
| Top bar | Avatar "AK" | user initials | **built** → identity + connection popover |
| Status bar | Connected · Protocol · Events · Console ver | bottom strip | global `AppStatusBar` (already real); remove page-local copies |

### Per-datum source map (PAGE-POLISH-PROCEDURE §3)

| Datum | Source (method/event) | Notes |
|---|---|---|
| Nav items / routes | static `NAV` constant | functional pass: each routes correctly (§4) |
| Active nav item | `$page.url.pathname` (client) | real, not faked |
| Breadcrumb label | `NAV` lookup on first segment (D-159) | real |
| Identity (tenant/user/session) | `connection.ts` `resolveConnection()` | real; session blank per D-171 |
| Avatar initials | `connection.identity.user` | real |
| Connection status dot | `connection !== null` (D-160) | real |
| Search results | `POST /v1/search/query` (`search.query`) | real; paginated; D-026 heavy rows by `Ref` |
| Status bar: runtime name | Console DB address book (operator-typed) | real (108a) |
| Status bar: Protocol version | `client.posture.info()` `protocol_version` | real (108a) |
| Status bar: Events Stream | `posture.info()` capability `events_subscribe` | real (108a) |
| Status bar: Console version | `VITE_CONSOLE_VERSION` build env | real (108a) |
| Bell badge count | — | NO SOURCE → omitted (finding) |
| Sidebar runtime stats | — (`runtime.snapshot` is wave-13-extends) | NO SOURCE → omitted (finding) |

## Ordered change list

1. **`tokens.css`** — add `--size-nav` (sizing group, in-place). Optionally a
   `--size-nav-collapsed` for the hamburger state.
2. **`package.json`** — add `lucide-svelte` (pinned). `npm i`; lockfile
   committed.
3. **`+layout.svelte` sidebar** — narrow to `--size-nav`; add a lucide icon per
   `NAV` item (extend `NavItem` with an `icon` component ref); tighten
   padding/gap to the compact mock density; two-line brand lockup; active-state
   icon coloring; collapsed mode (icons-only) driven by the hamburger.
4. **New `components/ui/TopBar.svelte`** (or inline in layout) — hamburger,
   breadcrumb, search launcher trigger, help link, identity avatar + popover.
5. **New `components/ui/GlobalSearch.svelte`** — ⌘K launcher; calls
   `client.search.query`; grouped results; navigates to the picked entity;
   disabled-with-tooltip when disconnected.
6. **`HarborClient`** — add `search` namespace (`query`, and the per-index
   methods as needed) targeting the Runtime's search routes.
7. **Status-bar consolidation** — confirm `AppStatusBar` is the only bottom bar;
   remove `overview/Footer.svelte` + `live-runtime/footer.svelte` renders and
   their imports; delete the now-unused components (or mark removed).
8. **Markdownlint pin** — add `markdownlint-cli2@0.13.0` as a dev dependency /
   Makefile target matching CI `markdownlint-cli2-action@v15`, so local↔CI
   MD029 can't drift (the v0.33-vs-v0.40 gap that bit v1.2.0).
9. **Tests** — Playwright chrome spec (icons present, compact width, search
   launcher round-trips, avatar popover, single status bar); unit spec for the
   `search` client method against a captured real `search.query` frame.

## Acceptance criteria

1. Sidebar renders at `--size-nav` (~200px) — measured ≤ ~14% of a 1920px
   viewport; every nav item shows its lucide icon; brand is the two-line lockup.
2. Active nav item matches the mock (accent pill + left-border + accent icon).
3. Clusters are Runtime / Execution / Resources / Settings (no Evaluation);
   Playground present in Execution; breadcrumb derives from NAV (D-159).
4. Hamburger toggles a collapsed (icons-only) sidebar; state persists across
   reload; the content region reflows (no overlap, no white bleed).
5. ⌘K opens the global search; typing a query calls `search.query` against the
   live Runtime; results are grouped and navigate correctly; disconnected →
   disabled-with-tooltip. Verified against real payload (§3).
6. Identity avatar shows real initials; the popover shows the resolved triple +
   base URL; disconnected state honest (D-160).
7. Exactly one bottom status bar (the global `AppStatusBar`) on every page; no
   page-local footer strip remains.
8. All four `PageState` branches still render through the chrome (Disconnected /
   Loading / Loaded / Error) — forced and seen (§5).
9. Viewport-locked: document does not scroll; only page regions scroll (§6).
10. Playwright: zero console errors on a clean authenticated load; hydration
    holds on reload (§7).
11. `svelte-check --fail-on-warnings` 0/0; `npm run lint` clean (tokens, no raw
    literals); `make drift-audit`; phase smoke; `make check-mirror`.
12. The §8 per-component ledger is produced and the §9 checkpoint audit is
    FAIL-free.

## Files added or changed

- `web/console/src/lib/tokens.css` (+`--size-nav`)
- `web/console/package.json` + `package-lock.json` (+lucide-svelte; +markdownlint-cli2 dev)
- `web/console/src/routes/(console)/+layout.svelte` (sidebar + top bar)
- `web/console/src/lib/components/ui/TopBar.svelte` (new)
- `web/console/src/lib/components/ui/GlobalSearch.svelte` (new)
- `web/console/src/lib/protocol/harbor.ts` (+`search` namespace)
- `web/console/src/routes/(console)/overview/+page.svelte` (drop page-local footer)
- `web/console/src/routes/(console)/live-runtime/+page.svelte` (drop page-local footer)
- `web/console/src/lib/components/overview/Footer.svelte` (removed)
- `web/console/src/lib/components/live-runtime/footer.svelte` (removed)
- `web/console/tests/app-shell-chrome.spec.ts` (new Playwright spec)
- `web/console/src/lib/protocol/harbor.search.test.ts` (new unit spec)
- `docs/plans/README.md` (status note), `README.md` (if a reader-facing change)

## Test plan

- **Unit:** `search` client method decodes a captured real `search.query`
  response (snake_case RPC body — PAGE-POLISH-PROCEDURE §3.3); grouping logic.
- **Playwright (`app-shell-chrome.spec.ts`):** compact width assertion; one
  icon per nav item; active-state class; hamburger collapse + persisted state;
  ⌘K opens search, query returns rows, navigation works, disconnected disables;
  avatar popover content; exactly one `[data-testid=connection-footer]`; zero
  console errors; hydration on reload.
- **Live verification (PAGE-POLISH-PROCEDURE §3–§7):** run continuously against
  the youtube validation agent on :18080 + Console live source on :18790.

## Smoke script additions

The chrome is Console-only (no new Runtime Protocol surface — `search.query`
already ships). No `scripts/smoke/phase-NN.sh` Runtime assertion is added; the
Playwright chrome spec is the gate. (If the `search` client method surfaces a
Runtime gap, that becomes a finding fixed in-wave per §17.6.)

## Coverage target

Frontend: `svelte-check` 0/0 + `npm run lint` clean + the new Playwright +
unit specs green. No Go package coverage delta (no Go change expected).

## Dependencies

- **lucide-svelte** — new `web/console` dependency. Per RFC §10 / CLAUDE.md §4.5
  rule 4, it is a **Svelte-only icon set** (not a competing component library;
  Skeleton stays the component library) — added with this rationale in the PR.
  Operator-approved (2026-05-30).
- Builds on 108a (the global `AppStatusBar`), D-159 (NAV), D-160 (disconnected
  predicate), the shipped `search.*` methods (Phase 72c).

## Risks / open questions

- **lucide-svelte bundle size** — tree-shaken per-icon imports keep it small;
  verify the production `npm run build` size is reasonable.
- **`--size-nav` reflow** — narrowing the sidebar must not break any page's
  `1fr var(--size-rail)` grid (those use the unchanged detail-rail token); spot
  check the table+rail pages.
- **Search route shape** — confirm the exact mount (`POST /v1/search/query` vs
  the control surface) by probing the live Runtime before wiring the client.
- **Collapsed-sidebar a11y** — icons-only mode needs `aria-label` / tooltip per
  item so it stays navigable.

## Glossary additions

- None anticipated (`--size-nav` is a token, documented in `tokens.css`).

## Pre-merge checklist

- [ ] `svelte-check --fail-on-warnings` 0/0
- [ ] `npm run lint` clean (no raw literals; tokens only)
- [ ] Playwright chrome spec + search unit spec green
- [ ] `make drift-audit` + phase smoke + `make check-mirror`
- [ ] §8 per-component ledger in the PR
- [ ] §9 checkpoint audit FAIL-free
- [ ] `docs/plans/README.md` / `README.md` updated if reader-facing
- [ ] lucide-svelte rationale in the PR description (RFC §10)

## Per-component / per-datum ledger (PAGE-POLISH-PROCEDURE §8 — verified live 2026-05-30)

Live env: youtube validation agent on :18080 (real LLM via OpenRouter, MCP
youtube, sqlite state + rolling_summary memory) + Console live vite source on
:18790. Zero console errors / warnings on a clean authenticated load.

| Component / datum | Source | Verified real (payload) | Functional | States | Result |
|---|---|---|---|---|---|
| Sidebar width | `--size-nav` token | 241px / 12.6% of 1920 (was 385/20.1%) | n/a | n/a | PASS |
| Nav items (14) + icons | `NAV` constant + lucide | 14 items, 14 svg icons | each routes (clicked → URL + active) | n/a | PASS |
| Brand lockup | static | "Harbor" / "CONSOLE" two-line | n/a | n/a | PASS |
| Active nav state | `$page.url.pathname` | active follows route (Overview→Tasks) | n/a | n/a | PASS |
| Breadcrumb label | `NAV` lookup (D-159) | "Tasks" derived, not lowercase | n/a | n/a | PASS |
| Hamburger collapse | local UI state | shell.collapsed, labels hidden, 14 icons | toggles + persists across reload (localStorage) | n/a | PASS |
| Global search rows | `POST /v1/control/search.query` | "youtube" → real task row (`status=complete`) + artifact ref (686KB JSON, D-026) | ⌘K opens; Enter/click → `/tasks`, `/sessions/<id>` | empty / error / disconnected | PASS |
| Identity avatar | `connection.identity` | initials "DE" from user `dev` | popover shows triple + base URL | muted "—" + red dot when disconnected | PASS |
| Avatar popover | `connection.ts` | Tenant dev · User dev · Session "(per-request)" (D-171) · Runtime URL | opens/closes; Settings link | "Not connected" branch | PASS |
| Status bar (single) | `posture.info` + Console DB (108a) | Connected · Protocol 0.1.0 · Events Stream: Live · Console dev | one bar only (0 page-local dups) | disconnected segment | PASS |
| Help link | static (`github.com/hurtener/Harbor#readme`) | external docs link | opens new tab | n/a | PASS |
| Bell / theme toggle | — (no source) | OMITTED (no notifications feed; dark-only) | n/a | n/a | DEFERRED (documented) |

Tests: `harbor-client.spec.ts` (+2: search route + captured-real-frame decode,
16/16); `app-shell-chrome.spec.ts` (5/5 vs `bin/harbor console` + runtime
fixture); full e2e suite 159 passed / 0 failed / 21 skipped (wave13 IA
cardinality + "sidebar lists the full 14-page IA in four clusters" green).
§17.6 fix bundled: `overview-page.spec.ts` stale `overview-footer` assertion
removed (it asserted the footer this wave consolidated away).

## Resolved decisions (operator, 2026-05-30)

1. **Scope = full shell chrome** (sidebar + top bar + bottom bar), not sidebar
   alone.
2. **Icons via lucide-svelte** (new Svelte-only dependency), not hand-authored
   inline SVGs.
3. **Bell + theme toggle omitted** (deferred) — no notifications feed; dark-only
   tokens.
4. **Global search built in full** — a ⌘K launcher calling `search.query` with
   grouped, navigable results.
5. **Status bar consolidated into the chrome** — the global `AppStatusBar` is
   the single bottom bar; per-page footer strips removed.
6. **Sidebar is too wide** (operator-flagged; measured 385px / 20.1%) — fixed
   via a dedicated `--size-nav` token to the mock's compact density.
