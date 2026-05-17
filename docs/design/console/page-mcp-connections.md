# Console page — MCP Connections

**Slug:** `mcp-connections` &middot; **Sidebar cluster:** Resources &middot; **Route:** `/console/mcp-connections`
**Mockup:** TBD — this spec drives mockup authoring

## 1. Purpose

MCP Connections is the control plane for the runtime's Model Context Protocol (MCP) southbound surface — the configured MCP servers that supply tools / resources / prompts to Harbor's agents. The page answers: "which MCP servers is this runtime connected to?", "is `github-server` reachable?", "what tools does `outlook-server` expose, and which agents bind to them?", "configure OAuth for `notion-server` at the `ScopeUser` binding scope," "refresh discovery on `slack-server` because its tool list changed," "test the connection to `vector-db-server`." The page is operator-facing — agents/developers configure MCP servers in the runtime config, but operators monitor their health and tend to their OAuth bindings here.

## 2. Where it sits in the IA

MCP Connections sits third under the **Resources** cluster (Resources → Flows, Memory, MCP Connections, Artifacts). The operator reaches it from the sidebar, from an Agent detail's Tools-tab "Open MCP server" link, from a Tools page detail's "Source provenance" link (for MCP-sourced tools), or from the global search palette. Breadcrumb: `<runtime> / MCP Connections` (list) and `<runtime> / MCP Connections / <server-name>` (detail).

## 3. Functionality matrix

- **Servers list — registered MCP servers, with name, transport (stdio / http+sse), URL or command, state (Connected / Disconnected / Error), tool count, recent latency, error rate, OAuth binding count.** `[wave-13-extends]` Requires `mcp.list` Protocol method (NEW) returning per-server descriptor + live state. Today Phase 28 (MCP southbound driver — `Shipped`) backs `tools.ToolDescriptor` for MCP-sourced tools, but no Console-facing per-server list method.
- **Per-row metadata — name, transport, URL/command, state, last-discovery time, tool count, resource count, prompt count, recent latency, error rate, OAuth status.** `[wave-13-extends]` `mcp.list` payload.
- **Filters — state, transport, has-OAuth, has-recent-error.** `[wave-13-extends]` `mcp.list` query payload.
- **Free-text search.** `[shipped]` Console-side index per Brief 11 §CC-4 (MCP servers are slow-moving catalog data).
- **Per-server detail — header (name + state badge + transport + URL/command + last-discovery + tool/resource/prompt counts).** `[wave-13-extends]` `mcp.get` Protocol method (NEW).
- **Tools tab — list of tools exposed by this server (clicks deep-link to Tools page detail).** `[wave-13-extends]` Filtered `tools.list` (NEW) by MCP source.
- **Resources tab — list of resources the server exposes (per Phase 28 MCP spec: server-provided typed references).** `[wave-13-extends]` `mcp.resources` Protocol method (NEW).
- **Prompts tab — list of prompts the server exposes.** `[wave-13-extends]` `mcp.prompts` Protocol method (NEW).
- **OAuth bindings tab — per-server OAuth configurations: binding scope (`ScopeUser` / `ScopeAgent` per `auth.BindingScope`), token freshness, configured scopes.** `[shipped]` Subscribe to `tool.auth_required` / `tool.auth_completed` events filtered to the server; per-binding metadata derived from the `tools.ToolDescriptor` projection (Phase 30 / D-083). Token storage lives in `tools/auth`; Console never sees plaintext tokens (Phase 30 / D-083).
- **"Connect" / "Reconnect" / "Revoke" per binding.** `[shipped]` `tool.auth_required` event flow: opens the `AuthorizeURL` in popup; on callback, the runtime emits `tool.auth_completed` and the Console refreshes the row.
- **"Refresh discovery" button — re-runs the MCP server's `tools/list` + `resources/list` + `prompts/list` discovery.** `[wave-13-extends]` `mcp.refresh_discovery` Protocol method (NEW).
- **"Test connection" button — performs a ping or `tools/list` round-trip and surfaces the result.** `[wave-13-extends]` `mcp.test_connection` Protocol method (NEW).
- **`mcp.resource_updated` event indicator — when the server notifies of resource updates.** `[shipped]` Subscribe to `mcp.resource_updated` (`EventTypeMCPResourceUpdated`).
- **MCP-Apps content shapes inventory — which canonical content shapes (`string` / `ImageRef` / `AudioRef` / `LinkRef` / `EmbeddedRef`) the server returns; which `DisplayMode` (per D-062) it declares.** `[wave-13-extends]` `mcp.get` extended fields.
- **Raw-HTML opt-in toggle — for trusted MCP servers, allow the renderer to display raw HTML/SVG fragments without sanitisation.** `[wave-13-extends]` Brief 11 §"Open architectural questions" #8; default-deny, per-source trust toggle, audit when toggled. Requires a Console-local trust-toggle persistence + per-server audit event (NEW: `mcp.raw_html_trust_toggled`). Risky UX — Wave 13 may scope it explicitly.
- **Per-server policy detail — configured `ToolPolicy` (timeouts, retries, transport-reconnect behavior per D-037).** `[wave-13-extends]` `mcp.get` extended fields.
- **No Priority field rendered.** `[deferred]` D-065 invariant preserved.
- **Saved filter chips.** `[shipped]` Console-local per D-061.

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page, list mode):
  - Row 1 — filter bar + saved-filter chips + search box.
  - Row 2 — servers table (virtualised).
- **Main canvas** (per-page, detail mode):
  - Row 1 — server detail header (name + state badge + transport + URL/command + last-discovery + tool/resource/prompt counts + "Refresh discovery" / "Test connection" buttons).
  - Row 2 — tab strip: Tools | Resources | Prompts | OAuth bindings | Policy.
  - Row 3 — selected tab content.
- **Right rail** (per-page, detail): Recent events card (`mcp.resource_updated`, `tool.auth_required`, transport-error subset of `tool.failed`) + per-binding-scope status summary.
- **Bottom dock** (per-page): empty.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Servers table | `mcp.list` (NEW) | Click row → detail | `[wave-13-extends]` |
| Filter bar / search | local UI state + Console-side index | Apply / Submit | `[shipped]` |
| Saved-filter chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Server detail header | `mcp.get` (NEW) | Copy name; "Refresh discovery" → `mcp.refresh_discovery`; "Test connection" → `mcp.test_connection` | `[wave-13-extends]` |
| Tools tab | filtered `tools.list` (NEW) by MCP source | Click tool → Tools page detail | `[wave-13-extends]` |
| Resources tab | `mcp.resources` (NEW) | Click resource → preview (renderer dispatched) | `[wave-13-extends]` |
| Prompts tab | `mcp.prompts` (NEW) | Click prompt → preview | `[wave-13-extends]` |
| OAuth bindings tab | per-server bindings from `mcp.get` + live `tool.auth_required` / `tool.auth_completed` events | Connect / Reconnect / Revoke per binding | `[shipped]` |
| Policy tab | `mcp.get` returns `ToolPolicy` projection | none in V1 | `[wave-13-extends]` |
| Recent events card (right rail) | `mcp.resource_updated` + filtered `tool.auth_required` + transport-error subset of `tool.failed` | Click → Events page filtered | `[shipped]` |
| Per-binding-scope status (right rail) | `auth.BindingScope` from `mcp.get` + live event status | Click → OAuth bindings tab | `[shipped]` |
| "Refresh discovery" button | `mcp.refresh_discovery` (NEW) | Submit | `[wave-13-extends]` |
| "Test connection" button | `mcp.test_connection` (NEW) | Submit | `[wave-13-extends]` |
| Raw-HTML opt-in toggle | Console DB (per-server trust) + audit event (NEW: `mcp.raw_html_trust_toggled`) | Toggle on/off (admin-gated, audited) | `[wave-13-extends]` |

## 6. Controls + actions

- **Toolbar:** filter bar + saved-filter chips + search box.
- **Row-action (list):** click → detail; right-click → Test connection / Refresh discovery (control-claim).
- **Header-action (detail):** "Refresh discovery" / "Test connection" / Copy name.
- **Tab-action (OAuth bindings):** Connect / Reconnect / Revoke per binding (scoped on `auth.BindingScope`: `ScopeUser` bindings are user-self-service; `ScopeAgent` bindings are admin-targeted).
- **Tab-action (Resources / Prompts):** Click → preview.
- **Keyboard shortcuts:** `g M` MCP Connections (operator-rebindable per Brief 11 §CC-5); `j` / `k`; `Enter` open detail; `Esc` back; `r` Refresh discovery (control-claim gated).

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty list | No MCP servers configured | Empty-state: "No MCP servers configured — add servers in your config and restart" + docs link | Visit docs |
| Filtered empty | Filters yield zero | "No servers match these filters" + Clear | Clear |
| Initial loading | `mcp.list` in flight | Skeleton rows | Auto |
| Server disconnected | State = Disconnected (transport down) | Red badge on row + "Test connection" affordance | Test / Refresh / Investigate config |
| Server error state | State = Error (recent transport-error events) | Yellow badge + error excerpt + "View errors" → Events page filtered | Investigate |
| Protocol error — `CodeNotFound` on detail | Server name unknown (perhaps just removed from config) | "Server not found"; back link | Back |
| Protocol error — `CodeScopeMismatch` on Refresh / Test | Operator submitted without control claim | Inline error | Request elevated scope |
| Protocol error — `CodeIdentityRequired` / `CodeAuthRejected` | Identity / auth dropped | Banner + recover | Re-attach |
| OAuth flow rejected | User cancelled or upstream returned error | Inline error on binding row; Retry button | Retry |

## 8. Multi-tenant / multi-runtime nuances

MCP servers are configured per-runtime; multi-runtime mode swaps the entire list when the runtime switcher changes. Within a runtime, the MCP catalog itself is tenant-agnostic, but OAuth bindings split by `auth.BindingScope`: `ScopeUser` bindings are per-Harbor-user (each user manages their own connection; the upstream sees the user); `ScopeAgent` bindings are admin-targeted (an admin configures the agent's service-account-style credentials, the upstream sees the agent). Non-admin operators see only their own `ScopeUser` bindings; admins see and edit `ScopeAgent` bindings.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — list / inspect MCP servers; manage own `ScopeUser` OAuth bindings.
- `admin` (`auth.ScopeAdmin`) — manage `ScopeAgent` OAuth bindings; configure raw-HTML trust toggle; widen the recent-events scope across tenants.
- `console:fleet` (`auth.ScopeConsoleFleet`) — post-V1 cross-runtime aggregator.
- **Control-plane verbs (Refresh discovery / Test connection)** require the control-scope claim per D-066 — these mutate the runtime's view of upstream state and emit events.

## 10. Out of V1 (deferred)

- **Adding / removing MCP servers from the Console.** MCP server registration is runtime configuration (yaml + restart); Console is inspector only.
- **Per-tool MCP-Apps renderer customization.** Forbidden per Brief 11 §PG-3 — all MCP-Apps content flows through the canonical renderer registry.
- **Cross-runtime MCP catalog aggregator.** D-091 — post-V1.
- **Per-server scheduled health checks / alerting.** Post-V1; Wave 13 may surface basic health events; alerting is post-V1.
- **Priority field rendered anywhere.** D-065 invariant preserved.

## 11. References

- Brief 11 §"MCP Connections view", §PG-3 (MCP-Apps compliance), §"Open architectural questions" #8 (raw-HTML opt-in).
- Brief 12 §"The two-surface model".
- RFC-001-Harbor.md §6.4 (Tool catalog and transports), §7 (Console).
- Decisions: D-037 (MCP southbound driver wraps `github.com/modelcontextprotocol/go-sdk@v1.6.0`; transport-reconnect via `ToolPolicy`), D-061 (Console DB local-only), D-062 (MCP-Apps `DisplayMode`), D-065 (no session priority — invariant), D-066 (control claim), D-083 (tool-side OAuth — `auth.BindingScope`), D-090 + D-095 (tool catalog OAuth + approval wiring).
- Phase plan: phase 28 (MCP southbound driver — `Shipped`), phase 30 (tool-side OAuth — `Shipped`), phase 64a (tool catalog OAuth + approval wiring — `Shipped`), phase 73 (state inspection — `Pending`).
- Glossary terms used: `Console`, `Runtime lens`, `auth.BindingScope`, `tool.auth_required`, `DisplayMode`, `Scope claim`, `Fleet control / fleet observation`.
