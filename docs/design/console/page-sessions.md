# Console page — Sessions

**Slug:** `sessions` &middot; **Sidebar cluster:** Execution &middot; **Route:** `/console/sessions`
**Mockup:** `docs/rfc/assets/console-sessions-page.png` (canonical, 2026-05-18)

## 1. Purpose

Sessions is the past-and-active durable record of every Harbor execution the operator has access to. Where Live Runtime is present-tense (one session, steered now), Sessions is investigative: "find me every session that failed in the last hour that called the `web_search` tool," "show me the longest-running session for tenant X," "replay the trajectory of session `01HXR…` step-by-step so I can debug why the planner gave up." Every session card is a doorway: clicking opens the per-session detail (trajectory, events, artifacts, control history); a "Continue" action re-opens it in Live Runtime; a "Clone" action seeds a fresh run with the same inputs; a "Convert to Evaluation" action stages it for the post-V1 Evaluations subsystem (D-064). The page is a list + detail surface, virtualised for high-cardinality tenants.

## 2. Where it sits in the IA

Sessions is the first entry under the **Execution** cluster (Execution → Sessions, Tasks, Agents, Tools, Events, Background Jobs). The operator typically reaches it from the global search palette, the Overview's recent activity feed, the Tasks page's "parent session" link, or the Agents page's "Recent sessions" sub-panel. From a session detail, the operator drills into the per-run trajectory, the per-session artifacts, the per-session event log, and back out to the Agent or Tools that ran in the session. Breadcrumb: `<runtime> / Sessions` (list) and `<runtime> / Sessions / <session-id>` (detail).

## 3. Functionality matrix

- **List view — sessions table with virtualised scroll, default newest-first.** `[wave-13-extends]` Requires a `sessions.list` Protocol method (NEW; Phase 73 currently spec'd only for `sessions.inspect` per master plan). The method must accept filters + cursor pagination and return per-session metadata.
- **Per-row metadata: session id, agent (agent_id resolved to name), user (email/identifier), status (Running / Paused / Completed / Failed), started, duration, tasks count, events count, total-cost, total-tokens.** `[wave-13-extends]` Backed by `sessions.list` + `sessions.inspect` (NEW). Per-row cost is derived from `llm.cost.recorded` aggregation; token counts come from the same.
- **Filter bar — status, agent, user, tenant (admin-only), started-in-window, has-pending-intervention, has-failed-task, cost-above-threshold.** `[wave-13-extends]` Filters compile to the `sessions.list` method's query payload.
- **Free-text search (across session id, agent name, user, recent event content).** `[wave-13-extends]` Requires `search.sessions` Protocol method (NEW per Brief 11 §CC-4 — sessions are high-cardinality, runtime-side per the brief's recommendation).
- **Bulk actions — Cancel selected, Pause selected (control-claim gated).** `[shipped]` Invokes `cancel` / `pause` Protocol methods per row (D-072: the ten canonical task-control methods). The bulk wrapping is local UI state — the server takes one method call per session.
- **Session detail header — full session id + copy, status, started, duration (live for active sessions), tasks count, events count, agent, user, tenant.** `[wave-13-extends]` `sessions.inspect` Protocol method (Phase 73 `Pending`).
- **Per-session trajectory tab — chronological planner steps (decision → tool → result → decision …) for the session's primary run.** `[wave-13-extends]` Requires `state.list_trajectories` + `state.load_planner_checkpoint` Protocol methods (Phase 73 acceptance criteria mention these). Renders against the same per-task structured-log retrieval Live Runtime uses.
- **Per-session events tab — full filtered event log, scoped to this session.** `[shipped]` Subscribe via Phase 60 SSE `/v1/events` filtered to the session's `(tenant, user, session)`; replay-from-cursor via Phase 6 `events.Replayer`; durable-log fallback via Phase 57 driver per D-074. Renders the same event taxonomy listed in page-events.md.
- **Per-session artifacts tab.** `[wave-13-extends]` `artifacts.list` (NEW Phase 73 method) filtered to session; per-artifact rows match page-artifacts.md.
- **Per-session control history.** `[shipped]` Subscribe to `control.received`, `control.applied`, `control.rejected` events filtered to session; newest-wins per session per D-071.
- **Per-session interventions log — completed and pending pauses with their resolution.** `[shipped]` Subscribe to `pause.requested` + `pause.resumed` + `tool.approval_*` + `tool.auth_*` events filtered to session.
- **"Continue this session in Live Runtime" action — re-attach to the same session id in the Live Runtime workbench.** `[shipped]` Local navigation; Live Runtime page consumes the same session id.
- **"Clone session" — seed a fresh `start` request with the original session's input + the same agent.** `[shipped]` Invokes `start` Protocol method (`types.StartRequest`) with `Query` and `Description` copied; new session id is minted by the runtime.
- **"Convert to Evaluation" — stage this session as an eval case.** `[deferred]` D-064: Evaluations is post-V1; the action is mocked-as-future on the mockup but not in V1.
- **"Replay trajectory" — step-by-step playback of the session's `state.history` slice.** `[wave-13-extends]` `state.history` Protocol method (Phase 73 `Pending`); UI is Console-local.
- **"Share read-only link" — generate a scoped read-only URL.** `[deferred]` Brief 11 §"Open architectural questions" #10; requires a session-share-token Protocol primitive; post-V1.
- **Export transcript (Markdown / JSONL).** `[wave-13-extends]` Client-side aggregation of `state.history` (NEW Phase 73 method) + chat messages — depends on Wave 13's state inspection surface.
- **Cancel an active session (cancel every live task).** `[shipped]` Invoke `cancel` per active task in the session; or a future `sessions.cancel` convenience method (consider `[wave-13-extends]` if added).
- **No Priority field on session cards or detail header.** `[deferred]` D-065 dropped session-level priority from V1.
- **Saved filter chips (e.g. "failed in last hour").** `[shipped]` Console DB holds Console-local state only per D-061; saved filters are Console-local.
- **Cross-tenant session list (admin-only).** `[shipped]` Filter bar's tenant facet is rendered only when the JWT carries `auth.ScopeAdmin`; submission elevates the subscription / list query.

## 4. Page anatomy

- **Sidebar** (shared): runtime switcher + cluster nav + footer counter strip.
- **Top bar** (shared): breadcrumb + global search + notifications + identity scope chip.
- **Main canvas** (per-page, list mode):
  - Row 1 — filter bar + saved-filter chips + search box.
  - Row 2 — sessions table (virtualised, virtual rows for cursor-paged loads).
- **Main canvas** (per-page, detail mode):
  - Row 1 — session detail header card (id + copy + status + started + duration + tasks/events/cost summary).
  - Row 2 — tab strip: Trajectory | Events | Artifacts | Control History | Interventions.
  - Row 3 — selected tab content (full canvas).
- **Right rail** (per-page, detail mode): Agent card (name + planner + model + memory strategy summary; "Open Agent" link), User card, Recent runs in this session.
- **Bottom dock** (per-page): empty; trajectory replay player slides up from the bottom when the operator clicks "Replay trajectory".
- **Footer** (shared): micro-counters.

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Sessions table | `sessions.list` (NEW Phase 73 method) | Click row → detail; bulk-select → bulk action row; sort by column (local UI state) | `[wave-13-extends]` |
| Filter bar | local UI state compiled into `sessions.list` query | Apply / Clear (re-invokes `sessions.list`) | `[wave-13-extends]` |
| Saved-filter chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Search box | `search.sessions` (NEW per Brief 11 §CC-4) | Submit → `search.sessions` | `[wave-13-extends]` |
| Session detail header | `sessions.inspect` (NEW Phase 73 method) | Copy id, click status badge (no-op) | `[wave-13-extends]` |
| Trajectory tab | `state.list_trajectories` + `state.load_planner_checkpoint` (Phase 73 acceptance) | Click step → expand decision detail; "Replay" → opens trajectory player | `[wave-13-extends]` |
| Events tab | `events.EventBus` subscription filtered to session; `events.Replayer` for backfill | Filter by type / search; export JSONL | `[shipped]` |
| Artifacts tab | `artifacts.list` (NEW Phase 73 method) filtered to session | Click artifact → Artifacts page preview | `[wave-13-extends]` |
| Control History tab | `control.received` / `control.applied` / `control.rejected` events | Click row → show payload (post-redaction) | `[shipped]` |
| Interventions log | `pause.requested` / `pause.resumed` / `tool.approval_*` / `tool.auth_*` events | "View" → expand; "Resume" (if still pending) → `resume` method | `[shipped]` |
| "Continue this session" | `harbor` URL routing | Local navigation to `/console/live-runtime?session=<id>` | `[shipped]` |
| "Clone session" | `start` Protocol method | Submit `types.StartRequest` with cloned `Query` / `Description` | `[shipped]` |
| "Cancel session" | `cancel` Protocol method per live task | Submit `types.ControlRequest` for each active run | `[shipped]` |
| Bulk-action toolbar | `cancel` / `pause` Protocol methods per row | Invoke methods sequentially | `[shipped]` |
| Replay-trajectory player | `state.history` (NEW Phase 73 method) | Play / pause / step / scrub (local UI state only) | `[wave-13-extends]` |
| Agent card (right rail) | `agents.get` (NEW; see page-agents.md) | "Open Agent" → Agents page detail | `[wave-13-extends]` |
| Recent runs in session | `state.list_trajectories` (Phase 73) | Click → trajectory tab focused on that run | `[wave-13-extends]` |

## 6. Controls + actions

- **Toolbar:** filter bar (status / agent / user / tenant / window / has-intervention / has-failed-task / cost), saved-filter chips, search box, bulk-select toggle.
- **Row-action (list):** Open detail (Enter); Continue in Live Runtime; Clone; Cancel; (multi-select) bulk Cancel / Pause.
- **Panel-action (detail tabs):** Trajectory step expand; Events filter / export; Artifacts open / download; Control history row expand; Interventions Resume.
- **Keyboard shortcuts:** `j` / `k` next / previous row; `Enter` open detail; `Esc` back to list; `Cmd-K` global search; `g s` go to Sessions.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty list | No sessions yet for the scope | Empty-state illustration + "Start your first session in Live Runtime" CTA | Click CTA → Live Runtime |
| Filtered empty | Filters / search yield zero rows | "No sessions match these filters" + "Clear filters" button | Click "Clear filters" |
| Initial loading | `sessions.list` in flight | Skeleton rows (8) + skeleton filter bar | Auto-resolves |
| Pagination loading | Cursor next-page in flight | Spinner at bottom; existing rows stay | Auto-resolves |
| Protocol error — `CodeInvalidRequest` | Malformed filter combo | Inline error on the filter bar: "Invalid filter — adjust and retry" | Adjust filter |
| Protocol error — `CodeIdentityRequired` | Identity scope dropped | Banner + redirect to Settings | Re-attach runtime |
| Protocol error — `CodeAuthRejected` | JWT expired | Banner: "Authentication expired — re-enter passphrase" | Re-auth |
| Protocol error — `CodeScopeMismatch` | Operator submitted a tenant facet without `admin` | Hide the facet; on submission, surface the error inline | Request elevated scope |
| Protocol error — `CodeNotFound` on detail | Session id in URL does not exist | Empty-state: "Session not found"; "Back to list" link | Navigate back |

## 8. Multi-tenant / multi-runtime nuances

Sessions is the page where cross-tenant viewing matters most. Default scope renders only sessions whose `(tenant, user)` matches the operator's JWT — every list call carries the triple, and the runtime's `sessions.list` `WHERE`-clauses by tenant per CLAUDE.md §6. When `admin` is held, the tenant facet appears and the operator can scope by tenant or unscope entirely; the runtime emits `audit.admin_scope_used` on every cross-tenant query per Phase 05's audit invariant. In multi-runtime mode, switching runtimes via the sidebar switcher per D-091 re-handshakes against the new runtime and refreshes the list — no fleet-aggregate cross-runtime list in V1 (Brief 11 §CC-1 + D-091 puts the fleet aggregator post-V1 wrapper around the per-runtime list).

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — sufficient to list / inspect / continue / clone the operator's own sessions.
- `admin` (`auth.ScopeAdmin`) — required to render the tenant facet, to list sessions outside one's own `(tenant, user)`, to see the user facet across other users in the same tenant.
- `console:fleet` (`auth.ScopeConsoleFleet`) — required to extend the list across multiple runtimes once the post-V1 fleet aggregator lands.
- **Control-plane verbs (bulk Cancel, bulk Pause, Resume on Interventions, Cancel-session)** require the more-elevated control claim per D-066.

## 10. Out of V1 (deferred)

- **"Convert to Evaluation" action.** D-064 — Evaluations is post-V1.
- **"Share read-only link" / session-share token.** Brief 11 §"Open architectural questions" #10 — post-V1.
- **Priority field on cards / detail.** D-065 dropped from V1.
- **Cross-runtime session aggregate list.** D-091 — Console-side aggregator is V1; cross-runtime list is post-V1 wrapper.
- **Drift mode (fork past message, replay).** Brief 11 §PG-5 — post-V1.

## 11. References

- Brief 11 §"Sessions view", §"Per-task detail pane" (the bottom-dock detail is shared with this page), §CC-2 (identity-aware UI), §CC-4 (search).
- Brief 12 §"The two-surface model".
- RFC-001-Harbor.md §5.2 (state snapshots row), §6.9 (Sessions and SessionManager), §6.13 (event bus), §7 (Console).
- Decisions: D-008 (sessions = longer-lived multi-turn conversations), D-061 (Console DB local-only), D-062 (Live Runtime ≠ Sessions), D-064 (Evaluations post-V1), D-065 (no session priority), D-066 (control claim), D-074 (durable event log).
- Phase plan: phase 8 (SessionRegistry — `Shipped`), phase 57 (durable event log — `Shipped`), phase 73 (Console state inspection — `Pending`).
- Glossary terms used: `Console`, `Live Runtime`, `Runtime lens`, `Scope claim`, `Fleet control / fleet observation`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-sessions-page.png` against §3-§7.

### Refinements to §4 page anatomy

- **Top sub-header strip** between breadcrumb and table: filter chips — `Saved filters` dropdown + `Status: All|Running|Paused|Failed|Completed` + `Identity` (tenant/user picker; admin-only fan-out) + `Tenants: All` (admin) + `Date range: Last 24h` + `More filters` + free-text search box + `Refresh` + `Sort By`. Bulk-action toolbar appears when rows are checked.
- **Main table** with columns: checkbox / session id (truncated, hover-to-reveal full) / status badge / agent name / user / started / last activity / events count / cost / row-action menu (⋯). Row-action menu items: Open in Live Runtime, View Trajectory, Clone, Export transcript, Cancel (control-scope-gated), Convert to Evaluation (disabled — D-064).
- **Right rail (Session Summary card)**: id, status, started, duration, events, tasks, agent, user, tenant, cost, last activity. Below: **Recent Interventions** card (per-intervention with reason + outcome + countdown) + **Recent Artifacts** card (mime icon + filename + size + age, matches Live Runtime pattern).
- **Bottom dock when a row is expanded** (or via Open / drilldown): tabs **Trajectory | Events | Cost History | Control History | Interventions**. Trajectory tab renders the planner-step timeline; Events tab is a filtered Event Stream; Cost History tab shows the per-step cost rollup; Control History tab shows the `control.received` / `control.applied` / `control.rejected` audit; Interventions tab shows the complete pause/approval history for the session.
- **Footer**: `Connected to <runtime> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`.

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Bulk-action toolbar (visible when rows selected) | local UI state | Bulk Cancel / Bulk Clone / Bulk Export (cancel is control-scope-gated; iterates `cancel` per row) | `[wave-13-extends]` (`cancel` per row is shipped; bulk wrapping is local UI) |
| Recent Interventions card (right rail) | `pause.requested` / `pause.resumed` / `tool.approval_requested` / etc. events filtered to the session | Click → bottom-dock Interventions tab | `[shipped]` |
| Recent Artifacts card (right rail) | `artifacts.list` (NEW Phase 73 method) filtered to session, sorted DESC, capped to 3-5 | Click → Artifacts page filtered to session | `[wave-13-extends]` |
| Cost History tab (bottom dock) | `llm.cost.recorded` events aggregated per step | Local UI state (chart vs table toggle) | `[shipped]` |
| Control History tab (bottom dock) | `control.received` / `control.applied` / `control.rejected` events | Click → bottom-dock Event Stream filtered to control.* events | `[shipped]` |
| Convert-to-Evaluation row action | (disabled in V1) | (no action — D-064 deferral; tooltip explains) | `[deferred]` |

### No mockup violations of binding carve-outs

- **D-065** — no Priority column anywhere. Confirmed.
- **D-061** — saved filters live in Console DB; session/intervention/artifact data sources from Protocol.
- **D-066** — Cancel (and bulk-cancel) row actions gated on control-scope.
- **D-064** — Convert-to-Evaluation is visible but disabled with tooltip; mockup respects.
