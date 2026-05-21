# Phase 73k — Console MCP Connections page

## Summary

Phase 73k lands the Console **MCP Connections page** — the operator
control plane for Harbor's MCP southbound surface (Phase 28). The
phase bundles (a) the per-page `[wave-13-extends]` Protocol additions
(`mcp.servers.list` + `get` + `resources` + `prompts` +
`refresh_discovery` + `probe` + `health` + `bindings.list` + `policy`
read methods plus admin verbs `refresh_binding` + `revoke_binding`),
(b) the SvelteKit page implementation, (c) the per-page Playwright
spec, and (d) the new audit event `mcp.raw_html_trust_toggled` for
the per-server raw-HTML opt-in toggle. OAuth Connect / Reconnect /
Revoke on the OAuth & Auth tab is a pure consumer of the **shipped**
`tool.auth_required` / `tool.auth_completed` event flow (D-083) —
the page emits NO parallel binding-state machine. The page is a
trivial §13 primitive-with-consumer closure: it IS the consumer of
every Protocol method it introduces, end-to-end, in the same PR.

## RFC anchor

- RFC §6.4
- RFC §7

## Briefs informing this phase

- brief 11
- brief 09
- brief 03

## Brief findings incorporated

- **brief 11 §"MCP Connections view"** — the operator control plane
  for the shipped Phase 28 southbound surface needs servers list,
  per-server detail (transport / state / last-discovery / counts /
  recent latency / error rate), refresh-discovery + test-connection
  verbs, view-tools / view-resources / view-prompts tabs, and an
  OAuth tab anchored on `auth.BindingScope`. Phase 73k ships exactly
  that surface — every functionality bullet maps to either a new
  `[wave-13-extends]` Protocol method or a shipped event the page
  subscribes to.
- **brief 11 §"Open architectural questions" #8 (raw-HTML opt-in)** —
  "default-deny; explicit per-source trust toggle in MCP Connections
  view, with a warning banner when enabled. Audit when toggled."
  Phase 73k implements this verbatim: the trust state lives in
  Console-local DB (D-061 carve-out for Console-side preferences),
  the toggle is admin-gated, and toggling emits the new audit event
  `mcp.raw_html_trust_toggled` so a runtime-side audit consumer
  records the change against the operator identity. Default-deny is
  enforced by the canonical renderer registry (D-062) which never
  surfaces raw HTML/SVG unless the per-server trust flag is set.
- **brief 11 §PG-3 ("Full MCP-Apps compliance")** — "Forbidden:
  bespoke per-MCP-server renderers — every MCP server's output flows
  through the canonical content shapes Phase 28 normalises." Phase 73k
  surfaces MCP-Apps content in the Resources / Prompts tabs and the
  events-card preview EXCLUSIVELY through the canonical renderer
  registry at `web/console/src/lib/chat/renderers/` (D-062). No
  per-page or per-server renderer escape hatch.
- **brief 09 §"BindingScope is declared, not inferred"** — the OAuth
  & Auth tab surfaces `auth.BindingScope` per binding principal. The
  shape comes verbatim from Phase 30's typed `auth.BindingScope`
  enum + the shipped `tool.auth_required` / `tool.auth_completed`
  event payloads (D-083). Phase 73k does not infer scope from any
  other field; it reads what the runtime declares.
- **brief 03 §1 / §4 (MCP cross-transport unification)** — Phase 28
  already normalises MCP `TextContent | ImageContent | AudioContent
  | ResourceLink | EmbeddedResource` into `ToolResult.Value` shapes.
  Phase 73k's Resources / Prompts tabs render those normalised
  shapes through the canonical renderer registry; no MCP-transport
  knowledge leaks into the Console.

## Findings I'm departing from (if any)

None.

## Goals

- Ship the MCP Connections page at `/console/mcp-connections` and
  `/console/mcp-connections/<server>` with the page anatomy from
  `docs/design/console/page-mcp-connections.md` §4 plus §12 — the
  sub-header strip, the servers table, the per-server detail panel
  (Tools, Resources, Prompts, OAuth & Auth, Health, Policy tabs),
  the right-rail status cards, the bulk-action toolbar, and the
  footer.
- Land the `[wave-13-extends]` Protocol method cluster `mcp.servers.*`
  on the single-source `CanonicalWireTypes` and on the Phase 60
  HTTP+SSE / WebSocket transport. Every method is identity-scoped;
  admin verbs (`refresh_binding` / `revoke_binding`) require the
  `tools.admin` scope claim per D-066; control-plane verbs
  (`refresh_discovery` / `probe`) require the control-scope claim
  per D-066.
- Register the new audit event `mcp.raw_html_trust_toggled` (typed
  payload, `SafePayload` by construction, emitted by the runtime
  when the Console mutates the per-server trust flag) and a matching
  Protocol method `mcp.servers.set_raw_html_trust` (admin-gated)
  whose side effect is the audit emit + the Console-local trust
  persistence.
- Make every OAuth interaction on the OAuth & Auth tab — Connect /
  Reconnect / Revoke — a pure consumer of the shipped Phase 30 +
  Phase 64a OAuth surface: subscribe to `tool.auth_required` /
  `tool.auth_completed` events filtered to the server, drive the
  popup against the runtime-provided `AuthorizeURL`, and let the
  runtime close the flow via the existing pause/resume primitive
  (D-067). The Console NEVER sees plaintext tokens.
- Bundle `web/console/tests/mcp-connections-page.spec.ts` — the
  per-page Playwright spec landing in the same PR per §13's "Console
  page phase shipping without its feeding Protocol-surface phase"
  rejection rule (read forwards: page + Protocol primitive ship
  together).
- Discharge the D-025 concurrent-reuse contract for `mcp.servers.list`
  with N≥100 concurrent calls against the shared MCP registry under
  `-race`.
- Ship a cross-subsystem integration test
  (`test/integration/mcp_connections_page_test.go`) wiring real MCP
  driver + real Phase 60 Protocol transport + real OAuth provider +
  real audit Redactor; assert identity propagation across every
  seam; cover the admin-claim mismatch + identity-required failure
  modes; run under `-race` with N≥10 concurrent SSE subscriber
  stress on `tool.auth_required`.

## Non-goals

- **Adding / removing MCP servers from the Console.** Per
  page-mcp-connections.md §10, MCP server registration is runtime
  configuration (yaml + restart); the page is inspector + OAuth
  control only. The `mcp.servers.register` Protocol method
  referenced in §12 row "Add MCP server button" is deferred to a
  later phase that lands a runtime-config-mutation surface.
- **Per-tool MCP-Apps renderer customization.** Forbidden per brief
  11 §PG-3 — all MCP-Apps content flows through the canonical
  renderer registry at `web/console/src/lib/chat/renderers/`
  (D-062). Phase 73n (Playground) is the introducing phase for the
  shared chat module; Phase 73k consumes its renderers read-only
  for the Resources / Prompts tabs and events preview.
- **Cross-runtime MCP catalog aggregator.** Post-V1 per D-091. The
  page reflects the active runtime only; multi-runtime context is a
  separate Console-local concern (CC-1 from brief 11).
- **Per-server scheduled health checks / alerting.** Wave 13
  surfaces basic transport health events through `mcp.servers.health`
  (handshake-latency sparkline + reconnect history + transport-error
  rate); scheduled probing + alerting is post-V1.
- **`mcp.servers.bindings.aggregate` separate from `bindings.list`.**
  The "binding scope summary" right-rail card is computed by the
  Console from the `bindings.list` response — no separate aggregate
  method to maintain.
- **Editing `ToolPolicy` from the Policy tab.** Read-only at V1 per
  page-mcp-connections.md §4. A policy-admin surface is post-V1.
- **Bulk "Disable" action.** §12 flagged it post-V1; Phase 73k
  ships only the per-server `refresh_discovery` / `revoke_binding`
  bulk actions that compose existing per-server admin verbs.
- **Console-side custom HTML/SVG sanitiser.** Phase 73k consumes
  DOMPurify (via the canonical renderer registry it inherits) and
  the raw-HTML opt-in toggle simply flips a per-server flag the
  registry reads. No new sanitisation logic ships in this phase.

## Console consistency

This is a Console page phase. It is **binding** on the shared Console
design-system foundation defined in `docs/design/console/CONVENTIONS.md`
(D-121 in `docs/decisions.md`). `CONVENTIONS.md` is the cross-cutting
authority for every Console page; a page PR that diverges from a convention
below is **rejected on sight**. The MCP Connections page is a catalog
surface with a selected-server detail rail; it mounts inside the shared app
shell and clears the §5 depth bar like every other page.

The page MUST:

- **Route under `(console)/`.** The page lives at
  `web/console/src/routes/(console)/mcp-connections/` and is served at
  `/mcp-connections` with **no `/console/` URL prefix** (the `(console)` route
  group is a layout-grouping device and does not appear in the URL). Detail
  views live at `(console)/mcp-connections/[server]/` and are served at
  `/mcp-connections/<server>`. All inter-page links use the unprefixed form;
  a link to `/console/<anything>` is a bug.
- **Render inside the shared app shell.** The page renders as a child of
  `(console)/+layout.svelte` — the single app shell carrying the sidebar,
  breadcrumb, identity/connection indicator, and footer. It never ships a
  standalone layout.
- **Use the shared `components/ui/` inventory.** It composes the cross-page
  primitives in `web/console/src/lib/components/ui/` — `PageHeader`,
  `FilterBar`, `DataTable`, `DetailRail`/`RailCard`, `BulkActionBar`,
  `SavedViewChips`, `Pagination`, `StatusChip`, `ConnectionFooter`,
  `PageState`. It **never forks a primitive that already exists**;
  page-specific components go in `components/mcp-connections/`.
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

- [ ] `internal/protocol/types/mcp_servers.go` (new) defines the
  request / response shapes for the eleven new Protocol methods,
  all on `singlesource.CanonicalWireTypes` so
  `make protocol-ts-gen-check` round-trips them into
  `web/console/src/lib/protocol.ts` (D-093):
  - `mcp.servers.list` / `MCPServersListRequest` /
    `MCPServersListResponse` — paged list with filter shape
    `{state, transport, has_oauth, has_recent_error, name_prefix,
     tenant_id, page_token, page_size}`. Per-row payload includes
    `{name, transport, url_or_command, state, last_discovery_at,
     tool_count, resource_count, prompt_count, recent_latency_ms,
     error_rate_per_min, oauth_binding_count, raw_html_trusted}`.
  - `mcp.servers.get` / `MCPServerGetRequest` /
    `MCPServerGetResponse` — single-server detail. Includes the
    list shape plus `display_modes_advertised []DisplayMode` (D-062),
    `content_shapes []string`, `tool_policy ToolPolicyView`
    (read-only projection of D-024), `bindings_summary` (per-scope
    counts).
  - `mcp.servers.resources` / request / response — paged list of
    advertised resources `{uri, mime_type, size_bytes, name, title}`.
  - `mcp.servers.prompts` / request / response — paged list of
    advertised prompts `{name, description, arguments []PromptArg}`.
  - `mcp.servers.refresh_discovery` / request / response — triggers
    `Provider.Discover` on the named server; response is the new
    counts + a `discovery_id` for log correlation.
  - `mcp.servers.probe` / request / response — runs a transport
    ping (or `tools/list` round-trip per page-mcp-connections.md
    §3) and returns `{ok, latency_ms, error}`.
  - `mcp.servers.health` / request / response — handshake-latency
    sparkline buckets, reconnect history, transport-error rate
    (aggregated from `tool.*` events scoped to the server's tools).
  - `mcp.servers.bindings.list` / request / response — paged list
    of OAuth bindings for the server, projected from the shipped
    `tools.auth.TokenStore` index. Per-binding payload
    `{principal_id, binding_scope, scopes, expires_at, last_used_at}`
    — NEVER token plaintext (D-083 invariant).
  - `mcp.servers.policy` / request / response — the read-only
    `ToolPolicyView` projection.
  - `mcp.servers.refresh_binding` / request / response — admin verb;
    invokes `auth.OAuthProvider.InitiateFlow` for the named binding;
    response carries the `AuthorizeURL` + `State` so the Console
    can open the popup. Identity is mandatory; `tools.admin` claim
    required (D-066).
  - `mcp.servers.revoke_binding` / request / response — admin verb;
    invokes `auth.OAuthProvider.Revoke`. Identity mandatory;
    `tools.admin` claim required (D-066).
  - `mcp.servers.set_raw_html_trust` / request / response — admin
    verb; persists the per-server trust flag (Console DB shadow per
    D-061 + runtime mirror keyed on `(tenant, server_id)` for the
    raw-HTML carve-out only) AND emits the new
    `mcp.raw_html_trust_toggled` audit event. Identity mandatory;
    `tools.admin` claim required.
- [ ] `internal/protocol/methods/methods.go` extends with the
  eleven new method-name constants under the `mcp.servers.*`
  namespace. No hardcoded method strings anywhere else (CLAUDE.md §8).
- [ ] `internal/server/handlers_mcp_servers.go` (new) implements
  each handler against the shipped `tools/drivers/mcp` registry
  (read paths) + `tools/auth` (binding paths). Every handler reads
  `identity.MustFrom(ctx)` first; missing identity fails closed via
  the existing `errors.CodeIdentityRequired` Protocol error. Admin
  / control claims are validated via the shipped scope-claim helper
  (Phase 61 + D-066) — a mismatch surfaces
  `errors.CodeScopeMismatch`.
- [ ] `internal/tools/drivers/mcp/registry.go` (extended) exposes a
  process-local read API the server handlers consume:
  `ListServers(ctx, filter) ([]ServerView, *cursor, error)`,
  `GetServer(ctx, name) (*ServerView, error)`,
  `ListResources(ctx, name) ([]ResourceView, error)`,
  `ListPrompts(ctx, name) ([]PromptView, error)`,
  `RefreshDiscovery(ctx, name) (*DiscoveryResult, error)`,
  `Probe(ctx, name) (*ProbeResult, error)`,
  `Health(ctx, name, window) (*HealthSnapshot, error)`. The view
  shapes are projection-only types — no MCP-SDK leakage past the
  package boundary. The registry is a D-025 reusable artifact: all
  per-call state lives on `ctx`; mutable per-server stats are
  guarded by atomics or `sync.RWMutex` with documented invariants.
- [ ] `internal/audit/events.go` (extended) registers the new
  `mcp.raw_html_trust_toggled` `events.EventType` + the typed
  payload `MCPRawHTMLTrustToggledPayload` (`SafePayload` by
  construction — fields are server-name + boolean + actor identity).
  The runtime emits one event per successful
  `mcp.servers.set_raw_html_trust` call.
- [ ] `internal/protocol/errors/errors.go` introduces NO new error
  codes — the existing `CodeIdentityRequired`,
  `CodeScopeMismatch`, `CodeNotFound`, `CodeInvalidArgument`,
  `CodeAuthRejected` suffice. Adding a new code without need is
  the §13 "third place to define Protocol message types" smell.
- [ ] `web/console/src/routes/(console)/mcp-connections/+page.svelte`
  (new) implements the list view using only the generated
  `protocol.ts` typed client (D-093). No hand-rolled `fetch`. Uses
  Skeleton components throughout (D-091 stack pin). All raw colour
  / spacing literals are token references (CLAUDE.md §4.5 #3).
- [ ] `web/console/src/routes/(console)/mcp-connections/[server]/+page.svelte`
  (new) implements the per-server detail view with the six tabs
  (Tools / Resources / Prompts / OAuth & Auth / Health / Policy).
  The Tools tab deep-links to `/console/tools?server=<name>` —
  73f's surface (Stage 2.1 sibling); the deep-link works against a
  73f build because Phase 73k stays a pure URL consumer.
- [ ] `web/console/src/lib/mcp-connections/state.svelte.ts` (new)
  owns the page's reactive state in Svelte 5 runes mode (D-092).
  Subscribes to filtered event streams via
  `protocol.eventsSubscribe({event_types: ["mcp.resource_updated",
   "tool.auth_required", "tool.auth_completed", "tool.failed",
   "mcp.raw_html_trust_toggled"], server_id: <name>})`.
- [ ] The Resources / Prompts tabs and the right-rail "Recent events"
  card render content **only** through
  `web/console/src/lib/chat/renderers/` (D-062). No bespoke
  per-server renderer. The renderer registry is a read-only consumer
  of Phase 73n's shared chat module.
- [ ] The raw-HTML opt-in toggle on the per-server detail header
  (admin-gated) calls `mcp.servers.set_raw_html_trust` and on
  success refreshes the per-server `raw_html_trusted` flag from the
  follow-up `mcp.servers.get`. The toggle is disabled (UI + server)
  for non-admin sessions; the disabled state shows a tooltip naming
  the missing claim.
- [ ] The OAuth & Auth tab's Connect / Reconnect / Revoke actions
  go through `mcp.servers.refresh_binding` (Connect + Reconnect
  share a single admin verb that invokes `InitiateFlow` regardless
  of prior state) + `mcp.servers.revoke_binding`. The popup window
  navigates to the runtime-provided `AuthorizeURL` and closes on
  redirect; the page polls the event stream for the corresponding
  `tool.auth_completed` (matched by `State`) and refreshes the
  binding row. No Console-side token storage.
- [ ] `web/console/src/lib/protocol.ts` regenerates cleanly via
  `make protocol-ts-gen-check` after the wire-type additions land
  in `singlesource.CanonicalWireTypes`. A `make protocol-ts-gen`
  followed by `git diff --exit-code` is the gate.
- [ ] `web/console/tests/mcp-connections-page.spec.ts` (new)
  Playwright spec covers: (a) servers list renders ≥1 row; (b)
  state badges (Online / Reconnecting / Offline / Auth pending)
  render correctly; (c) drill-in to per-server detail; (d) each
  tab paints; (e) refresh-discovery as a non-admin yields a visible
  scope-mismatch error; (f) refresh-discovery as a control-claim
  holder succeeds; (g) the OAuth Connect popup opens the
  `AuthorizeURL`, and the binding row updates after a simulated
  `tool.auth_completed`; (h) the raw-HTML toggle is disabled for
  non-admins; (i) toggling it as admin emits a recorded
  `mcp.raw_html_trust_toggled` event in the right-rail events card;
  (j) deep-link from a tool row jumps to `/console/tools?server=…`.
- [ ] Concurrent-reuse test (D-025): `TestRegistry_ListServers_ConcurrentReuse`
  runs N=128 concurrent calls against the shared MCP registry
  reader under `-race`; baseline-restored goroutine count; no
  cross-tenant payload bleed; distinct per-goroutine identity
  quadruples.
- [ ] Integration test `test/integration/mcp_connections_page_test.go`
  wires real `tools/drivers/mcp.Provider` (via the in-process mock
  MCP server from Phase 28's test fixtures) + real Phase 60 HTTP+SSE
  transport + real `tools/auth.Provider` (against an `httptest.Server`
  authorisation server emulating PKCE) + real `audit/drivers/patterns`
  redactor + real `events/drivers/inmem` bus. Asserts:
  - End-to-end identity propagation through every new method
    (request → handler → driver → response carries the same
    `(tenant, user, session)` triple verbatim; cross-tenant calls
    return `CodeIdentityRequired` / empty list per scope).
  - Admin-verb claim gating (`mcp.servers.refresh_binding` /
    `revoke_binding` / `set_raw_html_trust` without `tools.admin`
    return `CodeScopeMismatch`).
  - The OAuth Connect / Reconnect round-trip emits exactly one
    `tool.auth_required` and exactly one `tool.auth_completed`
    bound to the correct `auth.BindingScope`, and the page-side
    polling for `tool.auth_completed` resolves the matching
    pause-token (Phase 50 Coordinator).
  - The raw-HTML toggle emits exactly one
    `mcp.raw_html_trust_toggled` audit event with a SafePayload
    body containing the actor's identity quadruple.
  - N≥10 concurrent SSE subscribers on
    `protocol.eventsSubscribe(event_types=[mcp.*, tool.auth_*])`
    receive each event exactly once per subscriber; no cross-talk;
    no goroutine leaks after the connections close.
- [ ] `scripts/smoke/phase-73k.sh` shipped, executable, header
  `# PREFLIGHT_REQUIRES: live-server`. Counters land at OK ≥ 11
  (one per new method round-trip + the admin-gate probes + the
  audit-event probe), SKIP ≥ 0, FAIL = 0 against a phase-73k build;
  earlier-phase builds SKIP via the 404/405/501 convention.
- [ ] `docs/glossary.md` extended with the new terms
  (`mcp.servers.list`, `mcp.raw_html_trust_toggled`,
  `raw-HTML trust toggle`, `MCP-Apps renderer registry` —
  the rest of the Protocol method namespace is internal-shape
  jargon and is documented in the singlesource Go file's godoc).
- [ ] `docs/plans/README.md` row for Phase 73k appended (under the
  Wave 13 cluster ordering; Status: `Shipped` flips at merge time;
  this PR lands the row with `Pending`).
- [ ] `README.md` Status table extended with the Phase 73k row.

## Files added or changed

- `internal/protocol/types/mcp_servers.go` — new wire types for the
  eleven new Protocol methods (singlesource).
- `internal/protocol/singlesource/canonical.go` — extended
  `CanonicalWireTypes` registration so the TS generator picks up the
  new types.
- `internal/protocol/methods/methods.go` — eleven new
  `MethodMCPServers*` constants.
- `internal/server/handlers_mcp_servers.go` — eleven new handlers.
- `internal/server/handlers_mcp_servers_test.go` — handler-level
  unit tests (identity / claim gating / shape).
- `internal/tools/drivers/mcp/registry.go` — extended with the
  read API consumed by the handlers. Fully-encapsulated;
  no MCP-SDK leakage past the package boundary.
- `internal/tools/drivers/mcp/registry_test.go` — extended.
- `internal/tools/drivers/mcp/concurrent_test.go` — extended with
  `TestRegistry_ListServers_ConcurrentReuse` (D-025 N=128).
- `internal/audit/events.go` — new
  `EventTypeMCPRawHTMLTrustToggled` + payload registration.
- `internal/audit/events_test.go` — extended.
- `web/console/src/lib/protocol.ts` — REGENERATED via
  `make protocol-ts-gen` (D-093). Never hand-edited.
- `web/console/src/lib/mcp-connections/state.svelte.ts` — new page
  state owner (Svelte 5 runes).
- `web/console/src/lib/mcp-connections/api.ts` — typed wrapper over
  `protocol.ts` for the page's Protocol calls.
- `web/console/src/routes/(console)/mcp-connections/+page.svelte` —
  list view.
- `web/console/src/routes/(console)/mcp-connections/[server]/+page.svelte`
  — detail view with the six tabs.
- `web/console/src/routes/(console)/mcp-connections/[server]/+layout.svelte`
  — shared header strip + right rail.
- `web/console/tests/mcp-connections-page.spec.ts` — Playwright
  spec (per-page; bundles in this PR).
- `web/console/src/lib/tokens.css` — extended with any new
  semantic tokens the state badges / sparklines / scope chips need
  (no raw colour literals in the `.svelte` files; CLAUDE.md §4.5 #3).
- `test/integration/mcp_connections_page_test.go` — cross-subsystem
  integration test (§17.3 binding: real drivers everywhere).
- `scripts/smoke/phase-73k.sh` — live-server smoke.
- `docs/plans/phase-73k-console-mcp-connections-page.md` — this file.
- `docs/plans/README.md` — append Phase 73k row.
- `docs/glossary.md` — new terms.
- `README.md` — Status row.

## Public API surface

```go
package types

// MCPServersListRequest is the wire shape for mcp.servers.list.
type MCPServersListRequest struct {
    Identity        IdentityScope        `json:"identity"`
    State           []MCPServerStateView `json:"state,omitempty"`
    Transport       []string             `json:"transport,omitempty"`
    HasOAuth        *bool                `json:"has_oauth,omitempty"`
    HasRecentError  *bool                `json:"has_recent_error,omitempty"`
    NamePrefix      string               `json:"name_prefix,omitempty"`
    TenantID        string               `json:"tenant_id,omitempty"`
    PageToken       string               `json:"page_token,omitempty"`
    PageSize        int32                `json:"page_size,omitempty"`
}

// MCPServerView is the per-row payload returned by list / get.
type MCPServerView struct {
    Name              string             `json:"name"`
    Transport         string             `json:"transport"`
    URLOrCommand      string             `json:"url_or_command"`
    State             MCPServerStateView `json:"state"`
    LastDiscoveryAt   time.Time          `json:"last_discovery_at"`
    ToolCount         int32              `json:"tool_count"`
    ResourceCount     int32              `json:"resource_count"`
    PromptCount       int32              `json:"prompt_count"`
    RecentLatencyMs   int64              `json:"recent_latency_ms"`
    ErrorRatePerMin   float64            `json:"error_rate_per_min"`
    OAuthBindingCount int32              `json:"oauth_binding_count"`
    RawHTMLTrusted    bool               `json:"raw_html_trusted"`
}

// MCPServerStateView is the canonical state chip.
type MCPServerStateView string

const (
    MCPStateOnline       MCPServerStateView = "online"
    MCPStateReconnecting MCPServerStateView = "reconnecting"
    MCPStateOffline      MCPServerStateView = "offline"
    MCPStateAuthPending  MCPServerStateView = "auth_pending"
    MCPStateError        MCPServerStateView = "error"
)

// (Get / Resources / Prompts / RefreshDiscovery / Probe / Health /
//  BindingsList / Policy / RefreshBinding / RevokeBinding /
//  SetRawHTMLTrust request + response shapes follow the same
//  identity-first pattern; full Go shapes ship in the type file.)
```

```go
package methods

const (
    MethodMCPServersList             = "mcp.servers.list"
    MethodMCPServersGet              = "mcp.servers.get"
    MethodMCPServersResources        = "mcp.servers.resources"
    MethodMCPServersPrompts          = "mcp.servers.prompts"
    MethodMCPServersRefreshDiscovery = "mcp.servers.refresh_discovery"
    MethodMCPServersProbe            = "mcp.servers.probe"
    MethodMCPServersHealth           = "mcp.servers.health"
    MethodMCPServersBindingsList     = "mcp.servers.bindings.list"
    MethodMCPServersPolicy           = "mcp.servers.policy"
    MethodMCPServersRefreshBinding   = "mcp.servers.refresh_binding"
    MethodMCPServersRevokeBinding    = "mcp.servers.revoke_binding"
    MethodMCPServersSetRawHTMLTrust  = "mcp.servers.set_raw_html_trust"
)
```

```go
package audit

const EventTypeMCPRawHTMLTrustToggled events.EventType =
    "mcp.raw_html_trust_toggled"

type MCPRawHTMLTrustToggledPayload struct {
    events.SafeSealed
    Identity   identity.Quadruple
    ServerName string
    Trusted    bool
    OccurredAt time.Time
}
```

```go
package mcp

// ServerView, ResourceView, PromptView, DiscoveryResult, ProbeResult,
// HealthSnapshot, BindingView are projection-only types the registry
// exposes. They never carry MCP-SDK internal types.
type ServerView struct { /* … */ }

func (r *Registry) ListServers(ctx context.Context, f ListFilter) (
    []ServerView, *Cursor, error)
func (r *Registry) GetServer(ctx context.Context, name string) (
    *ServerView, error)
func (r *Registry) ListResources(ctx context.Context, name string) (
    []ResourceView, error)
func (r *Registry) ListPrompts(ctx context.Context, name string) (
    []PromptView, error)
func (r *Registry) RefreshDiscovery(ctx context.Context, name string) (
    *DiscoveryResult, error)
func (r *Registry) Probe(ctx context.Context, name string) (
    *ProbeResult, error)
func (r *Registry) Health(ctx context.Context, name string,
    window time.Duration) (*HealthSnapshot, error)
```

## Test plan

- **Unit:**
  - `internal/protocol/types`: shape-compile checks for every new
    request / response, JSON round-trip, `IdentityScope` mandatory
    fields.
  - `internal/protocol/methods`: every constant appears in the
    `AllMethods` aggregator (drift trip-wire).
  - `internal/server/handlers_mcp_servers_test.go`: per-handler
    happy path + identity-missing → `CodeIdentityRequired` +
    admin-claim-missing → `CodeScopeMismatch` + unknown-server →
    `CodeNotFound`.
  - `internal/tools/drivers/mcp/registry_test.go`: read-API unit
    coverage; cross-tenant filtering; pagination cursor stability.
  - `internal/audit/events_test.go`:
    `EventTypeMCPRawHTMLTrustToggled` registers exactly once;
    payload `SafePayload` invariant holds.
- **Integration:**
  - `test/integration/mcp_connections_page_test.go` — see
    Acceptance Criteria for the full scope. Real
    `tools/drivers/mcp.Provider` against the in-process mock MCP
    server, real Phase 60 HTTP+SSE transport, real
    `tools/auth.Provider` against an `httptest.Server`
    authorisation server emulating PKCE + RFC 7591, real
    `audit/drivers/patterns` redactor, real `events/drivers/inmem`
    bus, real `pauseresume.Coordinator`. ≥1 failure mode covered
    per CLAUDE.md §17.3 #3. Runs under `-race`.
- **Conformance:** N/A — Phase 73k is a Console-page phase, not a
  multi-driver subsystem. The MCP driver's own conformance suite
  (Phase 28) continues to run unchanged.
- **Concurrency / leak:**
  - `internal/tools/drivers/mcp/concurrent_test.go::TestRegistry_ListServers_ConcurrentReuse`
    — N=128 concurrent invocations against one shared Registry
    under `-race`. Distinct per-goroutine identity quadruples; a
    pre-cancelled-ctx subset; baseline `runtime.NumGoroutine`
    restored after join.
  - `test/integration/mcp_connections_page_test.go::TestE2E_Phase73k_SSESubscriberStress`
    — N=10+ concurrent SSE subscribers on the
    `mcp.* + tool.auth_*` filter; each event delivered exactly
    once per subscriber; no goroutine leaks after teardown.
- **Playwright:**
  - `web/console/tests/mcp-connections-page.spec.ts` — see
    Acceptance Criteria for the full scope. Runs against the
    `harbor dev` boot.

## Smoke script additions

- `scripts/smoke/phase-73k.sh`:
  1. `assert_status 200 "$(api_url /healthz)" "healthz`" — sanity.
  2. `protocol_call mcp.servers.list` with valid identity → 200 +
     `.servers` is an array.
  3. `protocol_call mcp.servers.list` with missing identity →
     non-2xx + body `.code = "identity_required"`.
  4. `protocol_call mcp.servers.get` for a known server → 200 +
     `.name` matches.
  5. `protocol_call mcp.servers.get` for an unknown server →
     non-2xx + `.code = "not_found"`.
  6. `protocol_call mcp.servers.resources` for a known server →
     200 + `.resources` is an array.
  7. `protocol_call mcp.servers.prompts` for a known server →
     200 + `.prompts` is an array.
  8. `protocol_call mcp.servers.health` for a known server → 200 +
     `.handshake_latency_buckets` present.
  9. `protocol_call mcp.servers.policy` for a known server → 200 +
     `.tool_policy.timeout_ms` numeric.
  10. `protocol_call mcp.servers.bindings.list` for a known server
      with the operator's identity → 200 + `.bindings` is an array
      (may be empty for a fresh dev runtime).
  11. `protocol_call mcp.servers.refresh_discovery` without the
      control-scope claim → non-2xx + `.code = "scope_mismatch"`;
      with the claim → 200 + `.discovery_id` present.
  12. `protocol_call mcp.servers.refresh_binding` without the
      `tools.admin` claim → non-2xx + `.code = "scope_mismatch"`.
  13. `protocol_call mcp.servers.revoke_binding` without the
      `tools.admin` claim → non-2xx + `.code = "scope_mismatch"`.
  14. `protocol_call mcp.servers.set_raw_html_trust` without the
      `tools.admin` claim → non-2xx + `.code = "scope_mismatch"`;
      with the claim → 200 + a subsequent
      `mcp.servers.get` shows `.raw_html_trusted = true` + a
      subsequent `events.subscribe` filter on
      `mcp.raw_html_trust_toggled` yields ≥1 record.
  15. Static guard: `grep -q "mcp.servers.list" internal/protocol/methods/methods.go`.
  16. Static guard: `grep -q "EventTypeMCPRawHTMLTrustToggled"
      internal/audit/events.go`.
  17. Static guard: `make protocol-ts-gen-check` succeeds (the
      generated `protocol.ts` is current).
  18. Static guard: no `fetch(` calls inside
      `web/console/src/routes/(console)/mcp-connections/` — every
      Protocol call routes through the generated client (D-093 +
      CLAUDE.md §13 hand-rolled-fetch rule).

The 404/405/501-on-unimplemented-surface convention keeps the script
green against earlier-phase builds.

## Coverage target

- `internal/protocol/types/mcp_servers`: 100% (pure shape file —
  trivial coverage).
- `internal/server/handlers_mcp_servers`: 80%.
- `internal/tools/drivers/mcp` (extended): 80% (existing baseline
  preserved or improved).
- `internal/audit` (extended): 100% on the new event type
  registration (existing baseline preserved).
- `web/console/src/lib/mcp-connections/state.svelte.ts` /
  `api.ts`: Playwright spec covers the reachable surface
  end-to-end; per-file coverage gates are the Console-side
  `npm run check` strict-mode + the Playwright assertions, not a
  numeric percentage (Console coverage rule deferred to a later
  Wave 13 sub-phase per §17.7's Stage-2 carve-out).

## Dependencies

- Phase 28 (MCP southbound driver — Shipped) — owns the runtime-side
  Provider; Phase 73k extends its registry with the read API the
  Console handlers consume.
- Phase 30 (tool-side OAuth — Shipped) — owns
  `auth.OAuthProvider` + `auth.TokenStore` + the
  `tool.auth_required` / `tool.auth_completed` events
  (D-083). Phase 73k consumes; does NOT duplicate.
- Phase 31 (tool approval gates — Shipped) — no direct dependency,
  but the OAuth & Auth tab's wording is consistent with
  approval-gate vocabulary; reading the shipped surface keeps the
  Console's narrative coherent.
- Phase 50 (pause/resume coordinator — Shipped) — the OAuth flows
  the page consumes drive through the unified primitive (D-067).
- Phase 60 (Protocol HTTP+SSE wire transport — Shipped) — owns the
  transport every new method rides on.
- Phase 61 (Protocol auth — Shipped) — owns the JWT validator + the
  scope-claim helper (`tools.admin`, control-claim) the handlers
  call.
- Phase 64a (tool catalog OAuth + approval wiring — Shipped) —
  owns the operator-config shape that declares per-tool OAuth
  binding; the OAuth & Auth tab reflects what's declared.
- Phase 72a (events filter / aggregate — Wave 13 Stage 1) — the
  page's right-rail events card consumes the filter-shape extension
  for `mcp.* + tool.*` topic filters.
- Phase 75 (Playwright baseline harness — Wave 13 Stage 1) — the
  CI hook the per-page spec rides on.

## Risks / open questions

- **`mcp.servers.register` (add MCP server from the Console).** §12
  row "Add MCP server button" referenced a Protocol method that
  depends on a runtime-config-mutation surface (yaml + restart
  today). Phase 73k stays inspector-only; the "Add MCP server"
  button is wired to a Console-local "show me the docs" link in
  V1. The future config-mutation surface is its own RFC concern.
  Acceptable per page-mcp-connections.md §10.
- **`mcp.servers.bindings.aggregate`.** §12 enumerated a separate
  aggregate method; Phase 73k computes the right-rail
  "binding scope summary" card on the Console from the
  `bindings.list` response — no extra method to maintain. If the
  fan-out cost rises post-V1, the aggregate can land in a follow-up
  phase without churning the page.
- **Raw-HTML trust toggle storage location.** The toggle is a
  per-server preference that affects the canonical renderer
  registry's per-server behaviour. The Console-local shadow lives
  in Console DB (D-061 — Console-local preferences) AND a
  runtime-side mirror keyed on `(tenant, server_id)` accompanies
  the audit event so a fleet operator can audit "which Consoles
  have which trust flags enabled." This is the legitimate
  carve-out D-061's "Console-local state only" clause anticipated
  for preferences with audit consequences. Documented under "Open
  questions" in case the storage shape is revisited; the audit
  event is the load-bearing surface.
- **OAuth popup mechanics in Playwright.** Browser popups are
  brittle in Playwright. The spec uses Playwright's `context.on('page')`
  to capture the new-window navigation and asserts the URL; the
  actual auth flow is stubbed via an `httptest.Server` running
  inside `harbor dev` in test mode (the existing Phase 30
  test-fixture surface). No real upstream OAuth provider is
  contacted in CI.
- **D-061 / D-062 boundaries on the Resources tab.** Resources
  rendered through the canonical renderer registry MAY include
  `EmbeddedRef` shapes whose `mime_type` is `text/html`. Without
  the raw-HTML trust flag, the registry renders these as a sealed
  "untrusted HTML preview" card with a "trust this server to render
  raw HTML" prompt (admin-gated). The card itself never executes
  the HTML; this matches brief 11 §"Open architectural questions"
  #8's default-deny posture.
- **N=10 SSE stress vs. the §17 N≥10 floor.** The integration test
  ships N=16 by default to mirror the Phase 30 / Phase 50
  precedent; N=10 is the §17 minimum and would also pass.

## Glossary additions

- **`mcp.servers.list`** — Phase 73k `[wave-13-extends]` Protocol
  method returning the configured MCP southbound servers with live
  state (transport, status chip, last-discovery, tool / resource /
  prompt counts, recent latency, error rate, OAuth binding count,
  raw-HTML trust flag). Identity-scoped; the read scope is the
  default operator claim. Companion: `mcp.servers.get`,
  `mcp.servers.resources`, `mcp.servers.prompts`,
  `mcp.servers.refresh_discovery`, `mcp.servers.probe`,
  `mcp.servers.health`, `mcp.servers.bindings.list`,
  `mcp.servers.policy`, plus admin verbs
  `mcp.servers.refresh_binding`, `mcp.servers.revoke_binding`,
  `mcp.servers.set_raw_html_trust`. RFC §6.4 + §7. Phase 73k.
- **`mcp.raw_html_trust_toggled`** — canonical audit event emitted
  by the runtime when a Console admin flips the per-server
  raw-HTML opt-in flag via `mcp.servers.set_raw_html_trust`.
  `SafePayload` by construction (server-name + boolean + actor
  quadruple; no upstream MCP content). Registered in
  `internal/audit/events.go`. Default-deny invariant for raw HTML
  / SVG rendering in the canonical renderer registry (brief 11
  §"Open architectural questions" #8). Phase 73k.
- **Raw-HTML trust toggle** — per-server, per-tenant flag the
  Console exposes on the MCP Connections detail page. When false
  (default), the canonical renderer registry renders `text/html`
  / `image/svg+xml` `EmbeddedRef` resources as sealed "untrusted
  HTML preview" cards. When true, the registry sanitises with
  DOMPurify and renders the fragment inline. Toggling requires
  `tools.admin` (D-066). Emits `mcp.raw_html_trust_toggled`.
  Phase 73k.
- **MCP-Apps renderer registry** — the canonical Svelte component
  registry at `web/console/src/lib/chat/renderers/` (D-062) that
  dispatches every MCP-Apps content shape Phase 28 normalises
  (`string` / `ImageRef` / `AudioRef` / `LinkRef` / `EmbeddedRef`)
  through one rendering pipeline. Forbidden: bespoke per-MCP-server
  renderers (brief 11 §PG-3). Phase 73n owns the registry; Phase
  73k consumes it read-only for the Resources / Prompts tabs and
  the right-rail events preview.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test
      passes — covered by the integration test's per-tenant identity
      assertions + the registry concurrent-reuse test's per-goroutine
      identity quadruples.
- [ ] **Concurrent-reuse test passes (D-025)** — N=128 concurrent
      invocations against one shared `mcp.Registry` reader under
      `-race`, asserting no data races, no context bleed, no
      cancellation cross-talk, no goroutine leaks.
- [ ] **Integration test exists** —
      `test/integration/mcp_connections_page_test.go` wires real
      MCP driver + real Phase 60 transport + real OAuth provider +
      real audit Redactor + real events bus; identity propagates;
      ≥1 failure mode (`TestE2E_Phase73k_AdminClaimMismatch`);
      N≥10 concurrent SSE subscriber stress; `-race` clean.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above +
      decisions.md entry filed — N/A (no departures).
- [ ] `web/console/tests/mcp-connections-page.spec.ts` lands in
      the same PR (CLAUDE.md §13: no Console page phase ships
      without its feeding Protocol-surface phase + spec).
- [ ] `make protocol-ts-gen-check` clean (D-093 — the generated TS
      client is current).
- [ ] No raw colour / spacing / type-scale literals in the new
      `.svelte` files (CLAUDE.md §4.5 #3).
- [ ] No hand-rolled `fetch` in `.svelte` files (CLAUDE.md §13).
- [ ] No bespoke per-MCP-server renderer (brief 11 §PG-3 / D-062).
