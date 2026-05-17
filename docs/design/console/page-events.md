# Console page — Events

**Slug:** `events` &middot; **Sidebar cluster:** Execution &middot; **Route:** `/console/events`
**Mockup:** TBD — this spec drives mockup authoring

## 1. Purpose

Events is the runtime's event-bus stream as a full-screen, query-driven investigative surface. Where Live Runtime shows the slice scoped to one session and Sessions shows the slice scoped to one session-detail, Events shows the full firehose with a powerful query / filter / save-view shape. The operator opens it when investigating across sessions ("every `tool.failed` in the last hour"), debugging a regression ("show every `planner.repair_exhausted` after the deploy"), or sampling for anomalies ("rate-over-time of `governance.budget_exceeded`"). The page is the page-mcp-connections / page-tasks counterpart for the event taxonomy: list with filter + per-event detail + export.

## 2. Where it sits in the IA

Events sits fifth under the **Execution** cluster (Execution → Sessions, Tasks, Agents, Tools, Events, Background Jobs). It is reached from the sidebar, from any other page's deep-link "filter Events by this type / this session / this task," from the Overview's audit ribbon, and from the global search palette. Breadcrumb: `<runtime> / Events` (default filter = last 30 min, all types).

## 3. Functionality matrix

- **Event timeline — virtualised time-ordered list, newest-first by default (oldest-first toggle for replay analysis).** `[shipped]` Subscribe to `events.EventBus` via Phase 60 SSE (`/v1/events`); backfill via Phase 6 `events.Replayer` (`ring buffer + cursor`); durable backfill via Phase 57 `durable` driver per D-074.
- **Filter bar — event type (multi-select from the canonical registry), identity scope (tenant / user / session / run), time range, free-text search across redacted payloads.** `[shipped]` Filters compile into `events.Filter` (identity + types + run). Time-range bounded subscription + Replayer Replay-from-cursor.
- **Type-multiselect picker — full canonical event taxonomy, grouped by source.** `[shipped]` Render `events.EventTypes()` (`internal/events/events.go::EventTypes`) — the exhaustive registry. At V1 the registry contains: `runtime.error`, `runtime.warning`, `bus.dropped`, `bus.subscription_idle_closed`, `audit.redaction_failed`, `audit.admin_scope_used`, `governance.budget_exceeded`, `governance.rate_limited`, `governance.maxtokens_exceeded`, `runtime.run_cancelled`, `task.spawned`, `task.started`, `task.paused`, `task.resumed`, `task.completed`, `task.failed`, `task.cancelled`, `task.prioritised`, `task.group_created`, `task.group_sealed`, `task.group_resolved`, `task.group_cancelled`, `task.patch_applied`, `task.patch_rejected`, `task.background_acknowledged`, `tool.invoked`, `tool.completed`, `tool.failed`, `tool.invalid_args`, `tool.policy_exhausted`, `tool.auth_required`, `tool.auth_completed`, `tool.approval_requested`, `tool.approved`, `tool.rejected`, `mcp.resource_updated`, `llm.image.materialized`, `llm.context_leak`, `llm.context_window_exceeded`, `llm.cost.recorded`, `llm.mode_downgraded`, `llm.retry_with_feedback`, `memory.identity_rejected`, `memory.health_changed`, `memory.recovery_dropped`, `distributed.bus_envelope`, `planner.decision`, `planner.finish`, `planner.error`, `planner.repair_exhausted`, `planner.max_steps_exceeded`, `trajectory.compressed`, `trajectory.compression_failed`, `pause.requested`, `pause.resumed`, `agent.registered`, `agent.restarted`, `agent.health`, `agent.drained`, `agent.deregistered`, `agent.paused`, `agent.restart_requested`, `agent.force_stopped`, `control.received`, `control.applied`, `control.rejected`, `flow.budget_exceeded`, `session.opened`, `session.touched`, `session.closed`, `session.gc_reaped`, `skill.upserted`, `skill.deleted`, `skill.pack_overwrite_refused`, `skill.search_executed`, `skill.identity_rejected`, `skill.proposed`, `auth.rejected`, `dev.draft.created`, `dev.draft.updated`, `dev.draft.previewed`, `dev.draft.saved`, `dev.draft.discarded`, `dev.hot_reload.triggered`, `dev.hot_reload.completed`.
- **Per-row format — `HH:MM:SS  [severity]  event_type  identity  source_descriptor`.** `[shipped]` Rendered from `events.Event` shape (`Type`, `Identity`, `OccurredAt`, `Sequence`, `Payload`).
- **Per-event detail — expand row to show typed payload (post-redaction).** `[shipped]` `events.Event.Payload` — either a `SafePayload` typed shape or a `events.RedactedMap` for non-Safe payloads.
- **Saved / shared filter views.** `[shipped]` Console DB holds Console-local state only per D-061; saved views (filter + time range + type set) are Console-local.
- **Export to JSONL / CSV (per-row, post-redaction).** `[shipped]` Client-side aggregation of the subscription's events.
- **Rate-over-time aggregation chart — per-type bucketed rate (line / area chart) over the selected time range.** `[shipped]` Client-side aggregation; for large windows, backfill via Phase 57 durable log.
- **Live pulse indicator.** `[shipped]` Visual pulse on subscription receipt.
- **Pause Console updates — halt local rendering without disconnecting the subscription.** `[shipped]` Local UI state.
- **Reconnect-with-cursor — survives transient disconnects gap-free via `events.Cursor` + `events.Replayer.Replay`.** `[shipped]` Phase 6 replay surface; Phase 57 durable replay when subscription window exceeds ring.
- **Cross-tenant fan-in (admin-only) — render every tenant's events in the runtime.** `[shipped]` `events.Filter.Admin = true` (Phase 05 trust-based; Phase 61 verifies via `auth.ScopeAdmin` / `auth.ScopeConsoleFleet`). Emits `audit.admin_scope_used` on submit.
- **"Show only events whose payload references session/task/agent X" deep-filter.** `[shipped]` Filter compiles to `events.Filter` with `Tenant`/`User`/`Session`/`Run` set; for agent-scoped filter, client-side payload match (since `agent_id` is NOT in the isolation tuple per D-059 and `events.Filter` does not key on it).
- **`bus.dropped` indicator strip — when the subscription buffer overflowed, a banner shows the dropped Sequence range.** `[shipped]` Subscribe to `bus.dropped` (`EventTypeBusDropped`); render the indicator on first drop in window.
- **Audit-only mode — restrict to `audit.*` types.** `[shipped]` Preset filter.
- **Search index over event payloads (high-cardinality).** `[wave-13-extends]` Brief 11 §CC-4 recommendation — events are high-cardinality, so runtime-side via a `search.events` Protocol method (NEW).
- **No Priority field rendered.** `[deferred]` D-065 — invariant preserved (Events page does not surface session/task cards, but the carve-out is noted).

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page):
  - Row 1 — filter bar + saved-view chips + time-range picker + admin-fan-in toggle (gated).
  - Row 2 — type-multiselect picker (chip-grouped by source: `runtime.*`, `task.*`, `tool.*`, `planner.*`, `agent.*`, `memory.*`, `llm.*`, `governance.*`, `session.*`, `pause.*`, `control.*`, `audit.*`, `skill.*`, `mcp.*`, `flow.*`, `distributed.*`, `dev.*`, `auth.*`, `bus.*`, `trajectory.*`).
  - Row 3 — rate-over-time chart (collapsible).
  - Row 4 — event timeline (virtualised; row-expand to per-event detail).
- **Right rail** (per-page): when a row is expanded, the typed payload renders here (full-payload view); otherwise the rail shows the current subscription status (cursor sequence, dropped count, replay-window).
- **Bottom dock** (per-page): export progress strip when a JSONL / CSV export is in flight.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Event timeline | `events.EventBus.Subscribe` via Phase 60 `/v1/events` SSE | Click row → expand into right rail; sort toggle (local UI state) | `[shipped]` |
| Filter bar | local UI state → `events.Filter` payload | Apply / Clear (re-opens subscription) | `[shipped]` |
| Type-multiselect picker | `events.EventTypes()` | Toggle per type | `[shipped]` |
| Saved-view chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Time-range picker | local UI state | Set range (re-opens subscription with cursor backfill) | `[shipped]` |
| Admin-fan-in toggle | `events.Filter.Admin` (gated on `auth.ScopeAdmin`) | Toggle on → reopen as admin fan-in (emits `audit.admin_scope_used` server-side) | `[shipped]` |
| Rate-over-time chart | client-side aggregation of subscription events | Click bucket → narrow time range | `[shipped]` |
| Per-event detail (right rail) | `events.Event.Payload` (typed or `RedactedMap`) | Copy as JSON (local) | `[shipped]` |
| Subscription status (right rail) | `events.Cursor` + `bus.dropped` events | Reconnect-from-cursor (local UI control) | `[shipped]` |
| Export to JSONL / CSV | client-side aggregation of subscription window | Submit → file download | `[shipped]` |
| Search box | `search.events` (NEW per Brief 11 §CC-4) | Submit | `[wave-13-extends]` |
| `bus.dropped` indicator strip | `bus.dropped` events | Click → expand to show dropped Sequence range | `[shipped]` |
| Pause-updates toggle | local UI state | Toggle on / off | `[shipped]` |

## 6. Controls + actions

- **Toolbar:** filter bar; type-multiselect (chip-grouped); time-range picker; admin-fan-in toggle (gated); saved-view chips; Pause-updates toggle; Export menu.
- **Row-action:** click row → expand right-rail detail; right-click → "Filter to this type" / "Filter to this session" / "Filter to this task / run" / "Open parent session in Live Runtime."
- **Panel-action (right rail detail):** Copy / Copy as JSONL / Open parent entity.
- **Keyboard shortcuts:** `g e` Events; `j` / `k` next / previous; `Enter` expand; `Esc` collapse; `Space` Pause-updates toggle; `/` focus search; `r` reconnect-from-cursor.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty timeline | Filters / scope yield no events in window | Empty-state: "No events match these filters in this window" + Clear / widen window | Clear or extend |
| Initial loading | Subscription opening / replay backfill in flight | Skeleton rows + "Loading <N> historical events" strip | Auto |
| Bus dropped | Subscription buffer overflowed | Strip: "<X> events dropped between Sequence <a..b>"; replay-from-cursor button | Reconnect-from-cursor |
| Cursor too old | Window > ring retention | Banner: "Older window — using durable backfill" (when Phase 57 driver active) or "Older window unavailable — narrow time range" | Narrow or rely on durable backfill |
| Protocol error — `CodeIdentityRequired` | Identity / scope dropped | Banner + recover | Re-attach |
| Protocol error — `CodeAuthRejected` | JWT expired | Banner + re-auth | Re-enter passphrase |
| Protocol error — `CodeScopeMismatch` | Operator toggled admin-fan-in without scope | Toggle reverts; inline error | Request elevated scope |
| Protocol error — `CodeInvalidRequest` | Malformed filter (e.g. bad Run id) | Inline error on filter bar | Adjust |

## 8. Multi-tenant / multi-runtime nuances

Events is the page where the admin / cross-tenant elevated subscription is most exercised. Default scope rejects subscriptions that elide the identity triple (Phase 05's `ErrIdentityScopeRequired`). When `admin` (or `console:fleet` per Brief 11 §CC-2) is held, the operator can toggle admin-fan-in; the runtime emits `audit.admin_scope_used` on subscribe so admin-scope use is retroactively detectable. In multi-runtime mode, the runtime switcher swaps the entire subscription — events are per-runtime; a post-V1 cross-runtime aggregator would merge feeds client-side per D-091.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — required; the runtime rejects elided identity per `events.ErrIdentityScopeRequired`. No identity-downgrading knob (CLAUDE.md §6 rule 9).
- `admin` (`auth.ScopeAdmin`) — required for cross-session / cross-tenant fan-in. Verified at Phase 60 transport edge per D-079.
- `console:fleet` (`auth.ScopeConsoleFleet`) — required for cross-runtime fan-in (post-V1 aggregator).
- No control-plane verbs on this page (the page is read-only; no Approve / Reject / Pause buttons here — those live on Live Runtime, Tasks, Agents).

## 10. Out of V1 (deferred)

- **Save / share filtered views as URL-encoded links (`/console/events?view=…`).** Shareability requires a Console-side share-token or URL-state encoding; saved views are local per D-061; URL-encoded view-share is a UX polish that can land in V1 as Console-only without a Protocol change.
- **Cross-runtime aggregator.** D-091 — post-V1.
- **Anomaly detection / alert rules.** Post-V1; would need a `notification.*` rule engine that goes beyond Brief 11 §CC-3's basic mapper.
- **Per-event traceparent rendering as an OTel deep-link.** D-073 lands the traceparent carrier; surfacing it as a "View trace in OTel" link is post-V1.

## 11. References

- Brief 11 §"Events view", §LR-5 (Event Stream — shared component), §CC-3 (notifications — distinct from events), §CC-4 (search — events are high-cardinality runtime-side).
- Brief 12 §"The two-surface model".
- RFC-001-Harbor.md §5.2 (streaming events row), §6.13 (typed event bus), §7 (Console).
- Decisions: D-028 (event bus surface), D-029 (`Replay` returns `[]Event`), D-061 (Console DB local-only), D-065 (no session priority — invariant), D-073 (OTel traces), D-074 (durable event log), D-079 (Protocol auth + scope claims).
- Phase plan: phase 05 (event taxonomy + InMem EventBus — `Shipped`), phase 06 (Bus replay + cursor — `Shipped`), phase 57 (durable event log driver — `Shipped`), phase 60 (Protocol wire transport — `Shipped`), phase 72 (Console subscription protocol surface — `Pending`).
- Glossary terms used: `Console`, `Runtime lens`, `Scope claim`, `Fleet control / fleet observation`, `Protocol`, `Deprecation window`.
