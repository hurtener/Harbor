# Console page — Tools

**Slug:** `tools` &middot; **Sidebar cluster:** Execution &middot; **Route:** `/console/tools`
**Mockup:** `docs/rfc/assets/console-tools-page.png` (canonical, 2026-05-18)

## 1. Purpose

Tools is the registered-tool-catalog browser. The operator opens it to answer: "what tools does this runtime have?", "what's the schema of the `web_search` tool?", "which MCP server provides the `git_diff` tool?", "what side-effect class is `delete_user`?", "how often is `summarize` called per session?", "what was the last invocation of `vector_search` and did it fail?". The page is a filterable catalog list, with a per-tool detail that surfaces the full descriptor (schema, examples, transport, policy, source provenance), recent invocations, and — for developers — a "Try this tool" form that invokes the tool directly with hand-crafted args (developer-scope gated; identity-propagated; emits canonical events).

## 2. Where it sits in the IA

Tools sits fourth under the **Execution** cluster (Execution → Sessions, Tasks, Agents, Tools, Events, Background Jobs). The operator reaches it from the global search palette, from an Agent detail's Tools-tab link, from an MCP Connections page server's "View tools" link, from a Task detail's tool-invocation row link, or directly from the sidebar. Breadcrumb: `<runtime> / Tools` (list) and `<runtime> / Tools / <tool-name>` (detail).

## 3. Functionality matrix

- **Catalog list — registered tools across all sources / transports for the runtime.** `[wave-13-extends]` Requires `tools.list` Protocol method (NEW) returning a flat projection of `tools.ToolDescriptor` (name, description, source, transport, side-effect class, schema-hash, examples-count, last-invoked).
- **Per-row metadata — name, source (e.g. agent / mcp-server / flow), transport (`TransportKind` = in-process / http / mcp / a2a / flow), side-effect class (read / write / external / destructive), schema hash, examples count, last-invoked timestamp, invocation count in window, error rate.** `[wave-13-extends]` `tools.list` payload (the per-row stats are aggregated client-side from `tool.invoked` / `tool.completed` / `tool.failed` event streams).
- **Filters — source, transport, side-effect class, loading mode, tag, has-OAuth, has-recent-error.** `[wave-13-extends]` `tools.list` query payload.
- **Free-text search.** `[shipped]` Console-side index per Brief 11 §CC-4 recommendation (tools are slow-moving catalog data).
- **Per-tool detail — full descriptor: name, description, source provenance, full schema, examples, configured policy (`ToolPolicy` per D-024), required auth (OAuth user-bound / agent-bound / none), MCP server origin (when transport = mcp), A2A peer origin (when transport = a2a).** `[wave-13-extends]` `tools.get` Protocol method (NEW).
- **Per-tool recent invocations — invocations across all sessions in scope, with per-row session id, task id, status (completed / failed), latency, identity (truncated).** `[shipped]` Subscribe to `tool.invoked` (`tools.ToolInvokedPayload`), `tool.completed` (`tools.ToolCompletedPayload`), `tool.failed` (`tools.ToolFailedPayload`), `tool.invalid_args` (`tools.ToolInvalidArgsPayload`), `tool.policy_exhausted` (`tools.ToolPolicyExhaustedPayload`) filtered to the tool name. Backfill via Phase 57 durable log per D-074.
- **Per-tool error breakdown — invalid-args count, policy-exhausted count, transport-error count, source-error count in window.** `[shipped]` Same event streams aggregated client-side.
- **"Try this tool" form — developer-scope gated; renders schema-driven form; invokes tool through the same path the planner uses; emits canonical events.** `[deferred-post-V1]` Requires a `tools.invoke` Protocol method that wraps the catalog's `Invoke` behind the same `ToolPolicy` reliability shell and identity propagation. Brief 11 §"Tools view" + §"Playground / direct interaction" PG-1..PG-7 constraints apply: not a side channel, full audit, ceilings honored. **V1 deferral (D-132 / Wave 13 §17.5 W4):** `tools.invoke` is NOT shipped at V1 — the Wave 13 Tools page surfaces the affordance as a disabled-with-tooltip "Try this tool" button (`data-testid="tools-try-tool"`) that names the deferral, rather than silently omitting it (CONVENTIONS.md §5, CLAUDE.md §13). The `tools.invoke` method + the live form land post-V1.
- **OAuth status badge per tool — when the tool requires OAuth, render "Connected" / "Reconnect required" / "Not connected" with deep-link to the binding configurator (lives in Agents or MCP Connections per source).** `[shipped]` Derived from `tool.auth_required` (`auth.ToolAuthRequiredPayload`) and `tool.auth_completed` events plus the binding scope from the tool's `auth.BindingScope`.
- **Approval-gate badge — when the tool's `ToolPolicy` requires HITL approval per D-086.** `[wave-13-extends]` Derived from the `tools.ToolDescriptor` projection (`tools.get` NEW) and the historical `tool.approval_requested` / `tool.approved` / `tool.rejected` events (shipped).
- **MCP-Apps content shapes inventory — when the tool's transport is mcp, list which canonical content shapes the tool returns (`string` / `ImageRef` / `AudioRef` / `LinkRef` / `EmbeddedRef`).** `[wave-13-extends]` `tools.get` extended fields; mockup may gesture as a "renders as: …" mini-summary.
- **MCP `DisplayMode` indicator — `inline` / `fullscreen` / `pip` per D-062.** `[wave-13-extends]` `tools.get` returns the `DisplayMode` declared in `internal/protocol/types/`.
- **Source-provenance link — for MCP-sourced tools, deep-link to MCP Connections page detail; for HTTP / A2A tools, render the configured URL/peer.** `[shipped]` Local navigation.
- **No Priority field rendered.** `[deferred]` D-065 — session-level priority dropped from V1. (Tools page does not surface session/task lists, but the carve-out is noted to maintain the cross-page invariant.)
- **Saved filter chips (e.g. "MCP tools with recent errors").** `[shipped]` Console-local per D-061.

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page, list mode):
  - Row 1 — filter bar + saved-filter chips + search box.
  - Row 2 — tools table (virtualised).
- **Main canvas** (per-page, detail mode):
  - Row 1 — tool detail header (name + source + transport + side-effect badge + OAuth badge + Approval badge).
  - Row 2 — tab strip: Schema | Examples | Policy | Source provenance | Recent invocations | "Try this tool" (developer-scope only).
  - Row 3 — selected tab content (full canvas).
- **Right rail** (per-page, detail): Statistics card (invocations / error rate / p50/p95 latency in window) + Source-provenance card.
- **Bottom dock** (per-page): empty.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Tools table | `tools.list` (NEW) | Click row → detail; sort (local UI state) | `[wave-13-extends]` |
| Filter bar | local UI state → `tools.list` query | Apply / Clear | `[wave-13-extends]` |
| Search box | Console-side index (Brief 11 §CC-4) | Submit | `[shipped]` |
| Saved-filter chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Tool detail header | `tools.get` (NEW) | Copy name; click source → source detail page | `[wave-13-extends]` |
| Schema tab | `tools.get` returns full schema (JSON Schema per `santhosh-tekuri/jsonschema`) | Copy schema (local) | `[wave-13-extends]` |
| Examples tab | `tools.get` returns examples | Copy example | `[wave-13-extends]` |
| Policy tab | `tools.get` returns `ToolPolicy` projection (retries, timeouts, side-effect class, approval requirement) | none in V1 | `[wave-13-extends]` |
| Source provenance tab | `tools.get` (for MCP / A2A: deep-link to source page) | Click link → source detail | `[wave-13-extends]` |
| Recent invocations tab | `tool.invoked` / `tool.completed` / `tool.failed` / `tool.invalid_args` / `tool.policy_exhausted` events filtered to tool | Click row → parent task detail | `[shipped]` |
| "Try this tool" form | `tools.invoke` (NEW) | Submit → invokes the tool through `ToolPolicy` shell; renders streaming events | `[wave-13-extends]` |
| OAuth status badge | `tool.auth_required` / `tool.auth_completed` events + `tools.get` (NEW) binding scope | Click → page-mcp-connections.md or page-agents.md for binding | `[wave-13-extends]` |
| Approval-gate badge | `tools.get` (NEW) + `tool.approval_requested` / `tool.approved` / `tool.rejected` events | none (informational) | `[wave-13-extends]` |
| Statistics card | event aggregation client-side | none | `[shipped]` |

## 6. Controls + actions

- **Toolbar:** filter bar + saved-filter chips + search box.
- **Row-action (list):** click → detail; copy name.
- **Tab-action (detail):** copy / open-source / drill into invocation.
- **"Try this tool":** submit invocation (developer-scope only); rendered streaming results.
- **Keyboard shortcuts:** `g T` (capital T) Tools (or operator-rebindable per Brief 11 §CC-5); `j` / `k`; `Enter` open detail; `Esc` back.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty catalog | No tools registered | Empty-state: "No tools registered — configure agents to attach tools" + docs link | Visit docs |
| Filtered empty | Filters yield zero | "No tools match these filters" + Clear | Clear |
| Initial loading | `tools.list` in flight | Skeleton rows | Auto |
| Protocol error — `CodeNotFound` on detail | Tool name unknown (perhaps just unregistered) | "Tool not found"; back link | Back |
| Protocol error — `CodeScopeMismatch` on "Try this tool" | Operator submitted invocation without developer scope | Inline error on the form | Request elevated scope |
| Protocol error — `CodePayloadInvalid` on "Try this tool" | Args failed JSON Schema validation | Inline schema-aware errors per field | Adjust args |
| Protocol error — `CodeIdentityRequired` / `CodeAuthRejected` | Identity / auth dropped | Banner + recover | Re-attach |
| OAuth not connected — "Try this tool" attempted | Tool requires OAuth, none configured | Inline: "OAuth required — Connect first" + deep-link to binding | Connect |

## 8. Multi-tenant / multi-runtime nuances

The tools catalog is per-runtime — every runtime has its own registered set; multi-runtime mode swaps the catalog when the operator switches runtime. Tools themselves are tenant-agnostic at the catalog level (they're descriptors), but their per-tool *invocation* statistics and recent-invocations list are tenant-scoped: a non-admin operator sees only their own tenant's invocations of the tool; with `admin` the recent-invocations list and statistics fan out across tenants (with `audit.admin_scope_used` emitted on the server). The "Try this tool" form's invocation runs under the operator's identity per Brief 11's PG constraints — no anonymous invocation; no ceiling bypass.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — list / inspect tools; see invocations scoped to one's tenant.
- `admin` — fan-in recent invocations across tenants; visibility into private agent-scoped tools.
- `console:fleet` — post-V1 cross-runtime aggregator.
- **Developer-scope (admin in V1)** — render and use the "Try this tool" form. Tools invocation through the Console honours all the §13 PG-* constraints (identity mandatory; audit uniform; cost uniform; policy honored).

## 10. Out of V1 (deferred)

- **Editing tool descriptors (registering / unregistering tools from the Console).** Tool registration is a runtime configuration concern (config + scaffolding), not a Console action; deferred.
- **Cross-runtime tools catalog aggregator.** D-091 — post-V1.
- **Per-tool cost dashboards (per-tool spend totals).** Aggregation possible client-side; deeper dashboards deferred to post-V1 cost subsystem expansion.
- **Bespoke per-MCP-server tool renderers.** Forbidden per Brief 11 §PG-3 — all MCP-Apps content flows through the canonical renderer registry.

## 11. References

- Brief 11 §"Tools view", §PG-3 (MCP-Apps compliance — relevant to MCP-sourced tools).
- Brief 12 §"The two-surface model".
- RFC-001-Harbor.md §6.4 (Tool catalog and transports), §7 (Console).
- Decisions: D-024 (`ToolPolicy` reliability shell), D-036 (HTTP tool driver), D-037 (MCP southbound driver), D-038 (A2A southbound driver), D-061 (Console DB local-only), D-062 (MCP-Apps `DisplayMode`), D-065 (no session priority — invariant), D-083 (tool-side OAuth — `auth.BindingScope`), D-086 (tool-side approval gates), D-090 + D-095 (tool catalog OAuth + approval wiring).
- Phase plan: phase 26 (Tool catalog core — `Shipped`), phase 27 (HTTP driver — `Shipped`), phase 28 (MCP driver — `Shipped`), phase 29 (A2A driver — `Shipped`), phase 30 (tool-side OAuth — `Shipped`), phase 31 (tool-side approval — `Shipped`), phase 64a (tool catalog OAuth + approval wiring — `Shipped`), phase 73 (state inspection — `Pending`).
- Glossary terms used: `Console`, `Runtime lens`, `auth.BindingScope`, `tool.approval_requested`, `tool.auth_required`, `DisplayMode`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-tools-page.png` against §3-§7.

### Refinements to §4 page anatomy

- **Sub-header strip (above the catalog table).** Saved-filter chips on the left (`Saved filters`, `Active only`, `Approval required`, `OAuth bound`, `Recent failures`) followed by faceted filter chips (`Scope`, `Transport`, `OAuth status`, `Approval policy`, `Reliability tier`) and a free-text `Search tools…` input. Right side carries a `Type ▾` quick-filter, `Refresh`, and `Export ▾` (CSV / JSON of the currently-filtered catalog rows — Console-local action; no Protocol mutation).
- **Catalog table (top-left, primary surface).** Columns in mockup order: checkbox / **Name** / **Version** / **Scope** (`tenant` / `agent` / `session`) / **Transport** badge (`in-proc` / `HTTP` / `MCP` / `A2A`) / **OAuth status** chip (`Bound` / `Required` / `n/a` / `Expired`) / **Approval policy** chip (`auto` / `gated` / `denied`) / **Reliability tier** chip / **Last used** (relative timestamp) / **Owner** / row-action menu (`⋯`). Bulk-action toolbar appears when ≥1 row is selected (`Set approval policy`, `Revoke OAuth bindings`, `Export selection`); every bulk action requires the `tools.admin` control-scope claim per D-066.
- **Right rail — Tool overview card (top).** Surfaces aggregate counters for the currently filtered view: `Total tools`, `Active`, `Pending approval`, `Awaiting OAuth`. Mini-sparklines beside `Active` and `Pending approval` plot the last 24 h.
- **Right rail — Status + Error rate card (middle).** Per-tool error-rate gauge (last 1 h / 24 h / 7 d toggle) plus a status pill (`Healthy` / `Degraded` / `Offline`) for the row currently selected in the catalog. Drives the bottom-dock Recent invocations tab when the operator clicks through.
- **Right rail — Content size + display mode card (bottom).** For the selected tool: distribution of recent invocation result sizes vs the heavy-content threshold (RFC §6.5 / D-026), and the negotiated `DisplayMode` for any MCP-Apps content the tool emits. Read-only.
- **Bottom-left — Selected tool detail panel.** Tabbed: **Manifest** (read-only JSON of the registered descriptor — transport, version, scopes, OAuth binding scope, approval policy, reliability shell) / **Inputs** (JSON Schema + example payloads) / **Outputs** (JSON Schema + heavy-content threshold + `DisplayMode` matrix when MCP-Apps capable) / **Recent invocations** (last N invocations with `run_id`, `session_id`, identity triple, duration, outcome — links into the session's bottom dock) / **Approval** (current policy, pending approval requests for this tool with the same Approve / Reject controls as the session-level intervention card — gated by `tools.approve` control-scope claim).
- **Bottom-right — Run history + Recent events panel.** Recent invocation timeline (success / failure / OAuth-pending / approval-pending icons) and a scrollable Recent-events list filtered to `tool.*` events for the selected tool. Both default to last 15 min; selector toggles to 1 h / 24 h / 7 d.
- **Footer.** `Connected to <runtime> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>` — matches the Overview footer (D-091, D-093).

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Saved-filter chips | Console-local saved views (D-061) | Select / pin / unpin a saved view | `[Console-local]` (D-061) |
| Faceted filter chips (Scope / Transport / OAuth status / Approval policy / Reliability tier) | `tools.list` response fields | Toggle facet | `[wave-13-extends]` (`tools.list` filter params) |
| Export ▾ (CSV / JSON of filtered catalog) | Already-loaded `tools.list` page | Client-side export of current page | `[Console-local]` (D-061; no Protocol mutation) |
| Tool overview counter card | `tools.list` aggregate + sparkline data from `tool.*` event stream | None (read-only) | `[wave-13-extends]` (`tools.list` returns aggregates) |
| Status + Error-rate card | `tool.invoked` / `tool.failed` event aggregates over selectable window | Toggle window (1 h / 24 h / 7 d) | `[wave-13-extends]` (`tools.metrics` or event-aggregate Protocol method TBD) |
| Content size + display mode card | Per-tool result-size histogram (RFC §6.5 / D-026) + negotiated `DisplayMode` snapshot | None (read-only) | `[wave-13-extends]` (`tools.content_stats` method TBD) |
| Manifest tab (read-only JSON of registered descriptor) | `tools.describe` / `tools.get` response | Copy-to-clipboard | `[wave-13-extends]` (`tools.describe` method) |
| Recent invocations tab (per-tool, links to session bottom dock) | `tool.invoked` event stream filtered by `tool_id` | Click row → navigate to `/sessions/<id>?dock=events` | `[wave-13-extends]` (event-stream filter by `tool_id`) |
| Per-tool Approval tab (lists pending approvals for this tool, Approve / Reject buttons) | `tool.approval_requested` events filtered by `tool_id` | Approve (`approve`) / Reject (`reject`) | `[shipped]` (`approve` + `reject` — Phase 54) |
| Bulk `Set approval policy` action | Selected row IDs | Apply new policy | `[wave-13-extends]` (`tools.set_approval_policy` admin method TBD) |
| Bulk `Revoke OAuth bindings` action | Selected row IDs | Revoke all bindings | `[wave-13-extends]` (`tools.revoke_oauth` admin method TBD) |
| Run history strip (last N invocations of selected tool with status icons) | `tool.*` event stream filtered by `tool_id` | Hover for tooltip; click for jump-to-session | `[wave-13-extends]` (event-stream filter) |

### No mockup violations of binding carve-outs

- **D-061 (Console DB local-only).** Saved-filter chips, Export ▾, and any pin/sort preference are Console-local state; the mockup never persists a Protocol-mutating shadow of tools (no edit / register / unregister UI), satisfying §10's "Editing tool descriptors" carve-out.
- **D-062 (MCP-Apps `DisplayMode` is Protocol-level).** The Content size + display mode card surfaces the negotiated `DisplayMode` for MCP-Apps content; the Console never renders MCP-sourced tool output through a bespoke renderer — the canonical renderer registry (`web/console/src/lib/chat/renderers/`) is the only path.
- **D-065 (no session-level priority).** The mockup carries tool-level metadata only (scope, transport, approval policy, reliability tier). No session-priority field appears.
- **D-066 (control-scope claims).** Approve / Reject and every bulk admin action are gated by the appropriate claim (`tools.approve`, `tools.admin`) and degrade to disabled-with-tooltip when missing; observation (catalog, status, run history) requires only the read scope.
- **D-091 (`harbor console` deployment).** The footer's Protocol version + Console version + connected-runtime fields confirm the Console is talking to a Runtime over the Protocol — no embedded `harbor dev` path.
- **§13 forbidden practices.** No hand-rolled renderers for MCP-sourced tool output; no `DisplayMode` toggle outside the canonical registry; no Console-side mutation of identity-scoped fields; no parallel implementations of approval (the Approve / Reject buttons here invoke the same `approve` / `reject` Protocol methods as the session/task views).
