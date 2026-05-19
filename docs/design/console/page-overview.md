# Console page — Overview

**Slug:** `overview` &middot; **Sidebar cluster:** Runtime &middot; **Route:** `/console/overview`
**Mockup:** `docs/rfc/assets/console-overview-page.png` (canonical, 2026-05-18)

## 1. Purpose

The Overview is the operator's landing page on every fresh Console session — the one-screen answer to "what is the runtime doing right now, and does anything need me?" An operator opens the Console after a coffee break, after a paging alert, or after a deploy, and the Overview must surface live counters, the freshest activity, and any pending interventions in under one screen of scrolling so the operator can decide whether to drill into Live Runtime, Sessions, Tasks, or Agents next. It is a hub, not a workbench — every counter, card, and row is a link to a deeper page.

## 2. Where it sits in the IA

Overview is the first entry under the **Runtime** sidebar cluster (Runtime → Overview, Live Runtime). It is the default route on a fresh attach to a runtime: `harbor console` lands the operator here, not on Live Runtime. From Overview the operator drills into every other page — counters link to their respective list pages (Tasks, Sessions, Background Jobs, Events), the intervention queue links to Live Runtime or the Sessions page filtered to the paused session, recent activity rows link to per-entity detail. Breadcrumb is flat: `<runtime name> / Overview`.

## 3. Functionality matrix

- **Live counters bar — events/sec, tasks running, background jobs, MCP connections.** `[wave-13-extends]` Requires a `runtime.snapshot` Protocol method (or a `runtime.health` event topic) returning aggregate counts derived from `task.spawned`/`task.completed`/`task.cancelled` deltas, MCP `provider.state` rollups, and a sampled bus throughput counter. Wave 13 must schedule a `runtime.counters` projection — there is no shipped `runtime.snapshot` method today.
- **Recent activity feed — last N session opens, task completions, task failures, agent restarts.** `[shipped]` Subscribe to `events.EventBus` with an admin-claim filter narrowed to types `session.opened`, `task.completed`, `task.failed`, `agent.restarted`, `agent.registered`; render newest-first. Data flows through Phase 60 SSE transport (`/v1/events`) per D-078.
- **Intervention queue — pending pauses across all sessions in scope, with operator action affordances.** `[wave-13-extends]` Requires a `pause.list` Protocol method (or a `notification.intervention_required` topic) that returns a snapshot of unresolved `pause.requested` records keyed by `(tenant, user, session, run, PauseToken)`. Today the operator can only observe `pause.requested` and `tool.approval_requested` events as they flow past; there is no list-pending query.
- **Notifications center (bell icon, dropdown) — typed notifications with severity, scope, deep-link.** `[wave-13-extends]` Requires a `notification.*` Protocol event topic per Brief 11 §CC-3 and the D-062 binding rule. Source events stay at their original types; a runtime-internal mapper decides which subset (`governance.budget_exceeded`, `tool.auth_required`, `tool.approval_required`, `task.failed` above threshold, `agent.health` degraded, `auth.rejected`) is promoted to user-facing notifications.
- **Alerts strip — banner-shaped warnings when a runtime is degraded.** `[shipped]` Subscribe to `runtime.warning`, `runtime.error`, `bus.dropped`, `audit.redaction_failed`, `memory.health_changed`, `governance.budget_exceeded`, `governance.rate_limited` — render the most recent per-type when in the last 5 minutes.
- **Quick links — Live Runtime, Sessions, Tasks, Agents, Tools, Settings.** `[shipped]` Local UI navigation; no Protocol surface.
- **Runtime context chip — active runtime name, version, health.** `[shipped]` Read `types.VersionHandshake` from the negotiation entry point (Phase 59 / D-077) plus the most recent `runtime.warning` / `runtime.error` event count.
- **"Connected runtimes" footer — multi-runtime indicator with switcher.** `[shipped]` Client-side state holding the N persistent Protocol connections per D-091; per-runtime `VersionHandshake` rendered.
- **Global search trigger (Cmd-K) — opens the global-search palette.** `[wave-13-extends]` Requires `search.*` Protocol methods per Brief 11 §CC-4 (sessions, tasks, events high-cardinality; tools / agents / flows / MCP servers Console-side index). Wave 13 must schedule a `search.query` method and decide the runtime/Console split.
- **Cost rollup card — total cost in window, by tenant / by agent (when admin).** `[shipped]` Subscribe to `llm.cost.recorded` aggregated client-side; respect identity scope.
- **Audit ribbon — count of `audit.admin_scope_used` events in window.** `[shipped]` Subscribe to `audit.admin_scope_used`; render as a count + "View in Events" link.
- **No Priority field rendered on intervention or activity rows.** `[deferred]` D-065 dropped session-level priority from V1.
- **Saved-view chips — per-operator "my hub" layouts.** `[shipped]` Console-local; Console DB holds Console-local state only per D-061.

## 4. Page anatomy

- **Sidebar** (shared): runtime switcher chip + cluster nav (Runtime / Execution / Resources / Evaluation / Settings) + footer counter strip.
- **Top bar** (shared): breadcrumb + global search trigger (Cmd-K) + notifications bell + help + user menu + identity scope chip.
- **Main canvas** (per-page):
  - Row 1 — runtime context chip (left) + alerts strip (right).
  - Row 2 — live counters bar (four cards: events/sec, tasks running, background jobs, MCP connections).
  - Row 3 — two-column split: intervention queue (left, 60% width) + cost rollup card (right, 40%).
  - Row 4 — recent activity feed (full width, virtualised).
  - Row 5 — quick links grid (six tiles).
- **Right rail** (per-page): empty on this page (the rail is reserved for context detail; the Overview's job is the canvas).
- **Bottom dock** (per-page): empty.
- **Footer** (shared): `Events / sec`, `Tasks Running`, `Background Jobs`, `MCP Connections` micro-counters.

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Runtime context chip | `types.VersionHandshake` + recent `runtime.error` count | Open Settings → Connected Runtimes | `[shipped]` |
| Alerts strip | `runtime.warning`, `runtime.error`, `bus.dropped`, `audit.redaction_failed`, `memory.health_changed`, `governance.budget_exceeded`, `governance.rate_limited` events | Click → Events page filtered to that type | `[shipped]` |
| Live counters bar | `runtime.counters` snapshot method (NEW) | Click counter → its detail page (Tasks list filtered to Running, etc.) | `[wave-13-extends]` |
| Intervention queue | `pause.list` snapshot method (NEW) + live `pause.requested` / `pause.resumed` / `tool.approval_requested` / `tool.approved` / `tool.rejected` deltas | Approve / Reject (invokes `approve` / `reject` Protocol methods); Resume (invokes `resume`); Open in Live Runtime | `[wave-13-extends]` |
| Cost rollup card | `llm.cost.recorded` events aggregated client-side | Open Settings → Governance (cost ceilings) | `[shipped]` |
| Recent activity feed | `session.opened`, `task.completed`, `task.failed`, `agent.registered`, `agent.restarted` events | Click row → that entity's detail page | `[shipped]` |
| Notifications dropdown | `notification.*` Protocol topic (NEW) | Snooze / Dismiss / Mute trigger (local UI state only); Open deep-link | `[wave-13-extends]` |
| Audit ribbon | `audit.admin_scope_used` events | Click → Events page filtered to type | `[shipped]` |
| Quick-links grid | local navigation | Local UI state only | `[shipped]` |
| Saved-view chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Global search trigger | `search.*` Protocol methods (NEW) | Invokes `search.query` | `[wave-13-extends]` |

## 6. Controls + actions

- **Toolbar (top bar):** Cmd-K opens global search; bell opens notifications dropdown; help opens Console-local docs; user menu opens Settings; identity scope chip shows the operator's `(tenant, user, session)` triple and any `admin` / `console:fleet` claim.
- **Row-action (intervention queue):** per-row Approve / Reject / Resume buttons (gated on the run's scope); per-row "Open in Live Runtime" link.
- **Panel-action:** counter cards are clickable (deep-link to their detail page); recent activity rows are clickable; alerts banners are clickable.
- **Keyboard shortcuts:** `Cmd-K` global search; `g o` go to Overview; `g l` go to Live Runtime; `g s` Sessions; `g t` Tasks; `g a` Agents; `j` / `k` next / previous row in the activity feed; `Enter` drill into selection; `Esc` close any open panel; per Brief 11 §CC-5.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Page empty | Fresh runtime, no events yet | "Waiting for runtime activity" placeholder + visible counter zeros + quick-links grid | Wait (auto-refresh on first event) |
| Initial loading | Subscription not yet open | Skeleton placeholders for counters, intervention queue, activity feed | Auto-resolves on Protocol handshake |
| Protocol error — handshake fails | `VersionHandshake` rejected or transport down | Banner: "Cannot reach runtime — check `harbor console.yaml`" + retry button | Retry connection (re-handshake) |
| Protocol error — `CodeAuthRejected` | JWT invalid / expired | Banner: "Authentication failed — re-enter passphrase or update token" + open Settings | Re-enter WebCrypto passphrase per D-091 |
| Protocol error — `CodeIdentityRequired` | No identity scope on subscription | Banner: "Identity required — fix `~/.harbor/console.yaml`" | Open Settings → Connected Runtimes |
| Unauthorized — cross-tenant counters | Operator lacks `admin` / `console:fleet` | Counters render scoped to the operator's `(tenant, user)` only; cross-tenant cost rollup hidden | Request elevated scope from admin |

## 8. Multi-tenant / multi-runtime nuances

The Overview's counters and cost rollup are the page most affected by scope. In single-tenant operator scope, every counter and feed row is scoped to the operator's `(tenant, user)` slice by default; the intervention queue lists only this operator's pending pauses. When the operator carries `admin` (or, for cross-runtime fleet view, `console:fleet`) the counters and the cost rollup fan out across tenants — the page renders an explicit "Elevated view" badge in the runtime context chip per Brief 11 §CC-2 so the operator knows what they are looking at. In multi-runtime mode (the operator has attached N runtimes via `harbor console`'s `~/.harbor/console.yaml` per D-091), the runtime switcher at the top of the sidebar picks the active runtime context; a future "All Runtimes" fleet mode (Console-side aggregator per D-091) aggregates each card across the attached set.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple from the JWT — sufficient to render the operator's own counters, cost rollup, intervention queue, and activity feed.
- `admin` (`auth.ScopeAdmin`) — required to see cross-tenant rollups in counters / cost / activity feed; required to render the audit ribbon (admin-scope-used events are themselves admin-scoped).
- `console:fleet` (`auth.ScopeConsoleFleet`) — required for the multi-runtime fleet-aggregate mode.
- **Control-plane verbs (Approve / Reject / Resume on intervention rows)** require the more-elevated control claim per D-066; the Console hides the buttons when the JWT lacks it, and the Protocol re-checks server-side (`CodeScopeMismatch` on submission).

## 10. Out of V1 (deferred)

- **Personal dashboards (configurable counter / card layouts).** Console-local state, possible without runtime work; deferred to keep V1 surface lean.
- **Cross-runtime fleet aggregator (the "All Runtimes" mode).** Console-side per D-091 — Wave 13 may scope it as a follow-up; not a blocker.
- **Notification routing config (email / Slack / web-push from Console).** Brief 11 §CC-3 mentions; lives in Settings, not Overview; mockup may gesture as "future."
- **Evaluations entry tile.** D-064: Evaluations is post-V1; the quick-links grid does NOT include it.

## 11. References

- Brief 11 §"Layout decomposition" (sidebar / top bar / main viewport / bottom dock), §CC-1 (multi-runtime), §CC-2 (identity-aware UI), §CC-3 (notifications), §CC-4 (search), §CC-5 (keyboard nav).
- Brief 12 §"The two-surface model" (Console deployed via `harbor console`), §"`harbor console` subcommand — what the future phase delivers".
- RFC-001-Harbor.md §5 (Protocol), §7 (Console layer), §7.1 (runtime-lens principle), §7.2 (information architecture).
- Decisions: D-002 (Console-as-Protocol-client), D-061 (Console DB is local-only), D-062 (14-page IA), D-065 (no session priority in V1), D-066 (fleet control elevated scope), D-091 (Console deployment posture).
- Phase plan: phase 72 (Console subscription protocol surface — pending re-decomposition).
- Glossary terms used: `Console`, `Console DB`, `Live Runtime`, `Runtime lens`, `Scope claim`, `Fleet control / fleet observation`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-overview-page.png` against §3-§7 above. The agent-authored spec (sections 1-11) is the v1.0 surface; this section adds visual specifics the mockup made concrete.

### Refinements to §4 page anatomy

- **Sub-header strip** between the top bar and the counter row, showing per-subsystem health chips: `Asset connected: <N> of <M>`, `Memory disk space ok`, `MCP service offline` (alert chip). This is the operator's at-a-glance posture answer — separate from the alerts strip in row 1. Renders chip-shape (not banner-shape). `[wave-13-extends]` — requires the `runtime.health` snapshot method (already noted in spec).
- **Counter row carries sparklines.** Each of the 4 cards (Events/min, Tasks Running, Background Jobs, MCP Connections) renders a mini-sparkline showing the trend over the selected time window. Click → drill-down page (Tasks → `/console/tasks`, Background Jobs → `/console/background-jobs`, MCP → `/console/mcp-connections`, Events/min → `/console/events`).
- **Cost rollup card (right column)** renders a per-agent breakdown by default (Research Agent / Support Agent / Code Reviewer / each with $X.XX). Per-tenant view is the admin elevation (the spec's existing wording was "by tenant / by agent (when admin)" — refine to "by-agent default, per-tenant on admin").
- **Quick Links grid** is a 2x3 of 6 navigation tiles: Sessions / Tasks / Background Jobs / Agents / Tools / Settings. Settings tile is the entry point to `/console/settings`. Each tile carries an icon + page name + one-liner description.
- **Top bar adds a `+ New` button** (right of the search box). Opens a quick-create dropdown menu: "New session", "Open Playground", "Run flow", "Add MCP server", "Connect runtime". Each menu entry deep-links into the relevant page's create flow. `[wave-13-extends]` for the menu surface; each individual create flow is owned by its page's spec.
- **Footer carries the active runtime + Protocol version + Events Stream connection state + Console version**: `Connected to <runtime-name> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`. Replaces the spec's "footer counter strip" — counters live in the main canvas, footer carries connection posture.

### Refinements to §3 functionality matrix

- **Events/min units** — the mockup uses per-minute aggregation, not per-second. Update bullet 1 from `events/sec` to `events/min` (matches the operator's at-a-glance reading; per-second buckets are too noisy for a counter card and stay in the Events page's rate chart).
- **Interventions panel renders 3 columns of action affordances** — Approve / Reject per row, plus a "View" link that deep-links into the paused session's Live Runtime page (one-click resolution from the hub). The spec's bullet covers this implicitly via the "operator action affordances" phrase; explicit here.
- **Recent activity rows render a typed-event icon + session-id chip + free-text description + relative timestamp**. The bullet is `[shipped]` per spec; the rendering shape is canonical.

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Health sub-header strip | `runtime.health` snapshot method (NEW) + per-driver `*.health_changed` events filtered | Click chip → relevant detail page (MCP → MCP Connections, Memory → Memory, etc.) | `[wave-13-extends]` |
| `+ New` quick-create menu | local navigation | Menu items deep-link to their page's create flow | `[wave-13-extends]` |
| Counter-card sparkline | local UI state (windowed event-rate aggregation) | hover → tooltip with last bucket value | `[shipped]` (subscription-derived; no new Protocol method) |

### Refinements to §7 states

- The "Page empty" state should still render the counter sparklines as flat-zero rather than hide them — operator gets "the runtime is up but quiet" rather than "the page didn't load."
- The "Initial loading" skeleton mirrors the 4-card row + the 6-tile quick-links grid so the operator sees the page-shape before data arrives.

### No mockup violations of binding carve-outs

- D-065 (no session priority): mockup honors — no priority field anywhere on the page.
- D-061 (Console DB local-only): mockup honors — every counter / feed / panel sources from Protocol events or snapshots, no Console-side runtime-entity mirroring.
- D-091 (multi-runtime deployment): mockup honors — footer's "Connected to <runtime>" + runtime context chip in top bar.
- D-066 (control-scope on intervention verbs): mockup shows Approve / Reject buttons; the spec's existing rule that these are gated on control-scope claim is preserved (the buttons are hidden when the JWT lacks the scope; mockup shows the elevated-operator view).
