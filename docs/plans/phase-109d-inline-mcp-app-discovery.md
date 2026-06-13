# Phase 109d — inline-mcp-app-discovery

## Summary

Close the dead seam the 109 MCP Apps wave left behind: the chain "a planner-initiated MCP tool result carrying `_meta.ui.resourceUri` → a chat message that mounts the 109b renderer → 109c's layout activates" was never wired, so the sandboxed renderer (109b) and the entire fullscreen/pip layout (109c) were unreachable in production. This phase wires the three breaks the wave-end audit pinned: (1) the runtime now emits a new canonical `mcp.app_available` SafePayload event at the MCP provider's invoke site whenever a tool result declares a `ui://` app, carrying the server source id + resource URI + display-mode hint + the run/identity correlation; (2) the wire `MCPAppRef` gains a `server_id` field so the renderer can resolve which server to read the `ui://` document from; (3) the Console's `ChatMessage` gains an `app`/`serverID` field, `MessageBubble` dispatches it under `MCP_APP_INLINE_MIME` to mount the real renderer, and the Playground page attaches the decoded `mcp.app_available` SSE event to the run's agent bubble. The §13 same-wave consumer is the discovery path itself — a planner-path MCP app result now reaches the inline renderer, and an inline app's `onrequestdisplaymode` drives the already-shipped 109c page-level layout.

## RFC anchor

- RFC §6.4
- RFC §6.5
- RFC §7

## Briefs informing this phase

- brief 14
- brief 11

## Brief findings incorporated

- brief 14 §6: "MCP Apps … Render in a **sandboxed iframe** … the AppBridge `postMessage` JSON-RPC dialect." — the renderer + bridge (109b) and the page-level layout (109c) already ship; this phase supplies the missing DISCOVERY signal that mounts them, so the host actually surfaces an app a planner-driven tool declared.
- brief 14 (MCP client compliance): `ui://`-scheme resources are a distinct resource class fetched via the standard `resources/read` MCP method. The discovery event carries the server source id so the Console fetches the `ui://` document via `mcp.servers.read_resource(serverID, resourceUri)` — the read path 109a shipped.
- brief 11 (Console feature surface): the Console is a pure Protocol client — every rendered datum reaches it through a typed Protocol method/event. The planner-path app reference had no surface (the proxy response only carried it for app-initiated calls); `mcp.app_available` is the canonical event that closes the "no primitive without its consumer, read backwards" gap (D-062 ordering) so the renderer never grows a private hook.
- brief 11 §"Playground" / §PG-3: the MCP-Apps `DisplayMode` honoring is part of the chat module's Console scope. This phase makes the inline renderer the live entry point; an inline app's `onrequestdisplaymode` feeds 109c's page-level layout, activating it exactly as 109c's plan foretold ("activates the moment that discovery path lands").

## Findings I'm departing from (if any)

None.

## Goals

- Emit a new canonical, SafePayload event `mcp.app_available` at the MCP provider's tool-invocation site whenever a tool result declares a `ui://` MCP App via `_meta.ui.resourceUri`, carrying `serverID` (the provider source id), `resourceUri`, the per-result `displayMode` hint, the default-deny `rawHTMLTrusted` posture, and the actor identity quadruple (its RunID correlates the discovery to the turn).
- Add `server_id` to the single-sourced wire `MCPAppRef` (and its runtime-side row + Console hand-synced mirror) so the renderer can resolve which server to read the `ui://` document from; populate it on the app-tool-call proxy response too.
- Give the Console `ChatMessage` an `app`/`serverID` field; make `MessageBubble` mount the 109b renderer (via the `MCP_APP_INLINE_MIME` registry dispatch) when an app ref is present; drive the discovery from the Playground page's `mcp.app_available` SSE handler, attaching the app to the run's agent bubble.
- Keep the chat module encapsulated (D-091): the renderer + injected `appHostClient` seam are unchanged; the page wires discovery at the page level + the message model, never reaching into the chat module beyond the existing injected-props seam.
- Activate 109c with no further downstream breaks: an inline app's `onrequestdisplaymode` (granted fullscreen/pip by the page's available-mode set) opens the app as a page-level app through the already-shipped layout reducer.

## Non-goals

- The iframe renderer + AppBridge wiring + sandbox/CSP (Phase 109b — unchanged).
- The fullscreen / pip page-level layout state machine (Phase 109c — unchanged; this phase only feeds it the discovery + the inline `onrequestdisplaymode` route).
- The `mcp.servers.read_resource` method + the app-tool-call proxy (Phase 109a — consumed, not redefined).
- Authoring MCP Apps — Harbor *hosts* apps; building the `ui://` document is a server-author concern.
- Reconciling the full per-server raw-HTML trust posture into the discovery event — the driver emits the default-deny posture; the Console reconciles via `mcp.servers.get` (the 109a/109b posture).

## Acceptance criteria

- [x] A planner-initiated MCP tool result carrying `_meta.ui.resourceUri` emits `mcp.app_available` on the real bus, carrying `serverID` + `resourceUri` + `displayMode` + the run/identity correlation, asserted by a Go integration test (real MCP driver against a fake MCP server, real bus, `-race`). — `EventTypeMCPAppAvailable` + `AppAvailablePayload` in `events.go`; `Provider.publishAppAvailable` emits from `callTool`; `TestE2E_MCPAppAvailable_PlannerPathEmitsEvent`.
- [x] An ordinary tool result (no `_meta.ui`) emits NO discovery event (negative). — `TestMCPAppAvailable_PlainResultEmitsNoEvent`.
- [x] The wire `MCPAppRef` carries `server_id`, single-sourced in `internal/protocol/types`, hand-synced into the Console wire type, and populated on the app-tool-call proxy response. — `types.MCPAppRef.ServerID`; `MCPAppRefRow.ServerID` + the proxy projection; `protocol/mcp.ts`; `TestAppsSurface_CallTool_AppRefProjection` asserts the projected `ServerID`.
- [x] The Console `ChatMessage` carries the inline app ref + server id; `MessageBubble` mounts the real renderer under `MCP_APP_INLINE_MIME` when present; the Playground page attaches the decoded `mcp.app_available` SSE event to the run's agent bubble. — `ChatMessage.app`/`serverID`; the `{#if message.app}` block; `decodeAppAvailable` + `applyAppAvailable` + the `'mcp.app_available'` subscription.
- [x] A real-component test mounts the REAL `MessageBubble` → `McpAppRenderer` (assert `data-renderer-source='mcp-app'`) and drives the real 109c layout reducer into the real `AppPanel` for fullscreen/pip; reverting the discovery→render wiring fails the suite. — `web/console/src/routes/(console)/playground/[session_id]/mcp-app-discovery.spec.ts` (verified to fail when the `{#if message.app}` block is reverted).
- [x] The chat module gains ZERO imports from Console internals (D-091); the discovery is wired at the page level + the message model. — the `phase-109d.sh` encapsulation grep is clean.
- [x] The generated Protocol reference regenerates with the new event; `scripts/smoke/phase-109d.sh` pins the event, the `server_id` wire field, the bubble dispatch, the page subscription, and the encapsulation grep. — `make protocol-docs-gen` committed; smoke shows OK ≥ acceptance count, FAIL = 0.

## Files added or changed

```text
internal/tools/drivers/mcp/
  events.go             # EventTypeMCPAppAvailable + AppAvailablePayload + registration
  mcp.go                # publishAppAvailable emitted from callTool
  mcp_app_available_test.go  # Go integration E2E (planner-path emit + negative branch)
internal/protocol/
  types/mcp_apps.go     # server_id on the wire MCPAppRef
  apps.go               # ServerID on MCPAppRefRow + the proxy projection
internal/mcpconsole/
  apps.go               # appRefFromValue carries the tool's source id
cmd/harbor-gen-protocol-docs/
  events.go             # mcp.app_available join row
docs/site/protocol/     # regenerated events.md + types.md (make protocol-docs-gen)
web/console/
  vite.config.ts        # VITEST-gated browser resolve condition (component-mount tests)
  src/lib/protocol/mcp.ts                       # server_id hand-sync
  src/lib/chat/types.ts                         # ChatMessage.app / serverID
  src/lib/chat/MessageBubble.svelte             # renderer dispatch under MCP_APP_INLINE_MIME
  src/lib/chat/ChatPanel.svelte                 # thread appHostClient / availableDisplayModes / onAppDisplayModeRequest
  src/routes/(console)/playground/[session_id]/mcp-app-discovery.spec.ts        # real-component regression guard
  src/routes/(console)/playground/[session_id]/+page.svelte       # subscribe + applyAppAvailable + inline onrequestdisplaymode → layout
  src/routes/(console)/playground/[session_id]/wire-events.ts     # decodeAppAvailable
  src/routes/(console)/playground/[session_id]/wire-events.spec.ts # decoder cases
  tests/mcp-app-displaymode.spec.ts             # rewritten to the real Playground route (replaces the synthetic-DOM harness)
scripts/smoke/phase-109d.sh
docs/plans/README.md      # 109d row + detail block (coordinator)
docs/decisions.md         # D-215
docs/glossary.md          # MCP App discovery / mcp.app_available
```

## Public API surface

Single-source rule honoured: the only new wire surface is the `server_id` field on the existing `types.MCPAppRef` and the new canonical event `mcp.app_available` (registered via `events.RegisterEventType`, indexed by the docs generator). Go-flavoured:

- **New canonical event** `mcp.app_available` — `mcptool.AppAvailablePayload` (SafePayload): `{ Identity identity.Quadruple; ServerID tools.ToolSourceID; ResourceURI string; DisplayMode string; RawHTMLTrusted bool; OccurredAt time.Time }`. Emitted by the MCP provider's tool-invocation path; consumed off the `events.subscribe` SSE stream by the Console Playground.
- **`types.MCPAppRef.ServerID`** (`json:"server_id,omitempty"`) — the MCP server source id paired with `ResourceURI` for `mcp.servers.read_resource`. Empty for a non-app result.

No new runtime-internal Go surface is exported to the Console; the Console reads only the event payload + the existing method results.

## Test plan

- **Unit:** (Go) the negative branch — a plain tool result emits no discovery event. (TS) `decodeAppAvailable` grammar (PascalCase payload, run correlation, drops a frame missing `server_id`/`resource_uri`, ignores other types).
- **Integration:** (Go) `TestE2E_MCPAppAvailable_PlannerPathEmitsEvent` — a planner-initiated MCP tool call (through the descriptor's `Invoke` closure, the real planner path) whose result declares a `ui://` app emits `mcp.app_available` on the real inmem bus with the server source id + resource URI + display-mode hint + run/identity, under `-race`. (TS) the real-component `mcp-app-discovery.spec.ts` mounts the real `MessageBubble` → `McpAppRenderer` and drives the real reducer/projection into the real `AppPanel`.
- **Conformance:** N/A — no new wire-conformance surface; the `server_id` field rides the existing `MCPAppRef` covered by 109a's negotiation tests, and the event is a SafePayload covered by the docs generator's lockstep test.
- **Concurrency / leak:** N/A — the MCP `Provider` gains no per-run state (`publishAppAvailable` is a pure per-call emit reading the call's `ctx`); the existing `TestProvider_*ConcurrentReuse` coverage stands. The Console change is Svelte rendering, not a Go runtime artifact.

## Smoke script additions

- `scripts/smoke/phase-109d.sh` (classification: `static-only`):
  - Assert `mcp.app_available` is declared + registered + has its `AppAvailablePayload`, and the driver emits it.
  - Assert the generated Protocol events reference carries `mcp.app_available`.
  - Assert the wire `MCPAppRef.server_id` field (Go single source + Console hand-sync).
  - Assert `ChatMessage.app`, the `MessageBubble` `{#if message.app}` dispatch under `MCP_APP_INLINE_MIME`, and the page's `decodeAppAvailable` + `'mcp.app_available'` subscription + `applyAppAvailable`.
  - Assert the chat module imports ZERO Console internals (D-091 encapsulation grep), excluding test files.

## Coverage target

- `internal/tools/drivers/mcp`: 85% (the existing target; the new emit + payload are covered by the integration + negative tests).
- `internal/protocol`: 80% (the `server_id` projection is covered by the extended `TestAppsSurface_CallTool_AppRefProjection`).
- `web/console` (chat module discovery path): 80% (the real-component suite + the decoder suite).

## Dependencies

- 109a (the `mcp.servers.read_resource` method + the app-ref projection + the proxy response the `server_id` field rides).
- 109b (the iframe renderer + AppBridge + the `MCP_APP_INLINE_MIME` registry entry the bubble dispatches to).
- 109c (the page-level fullscreen/pip layout the inline app's `onrequestdisplaymode` activates).
- 28 / 85a (the MCP driver's discovery + invocation surface the emit site sits on).

## Risks / open questions

- **§13 primitive-with-consumer.** The new `mcp.app_available` event is a primitive; its consumer lands in the SAME phase — the Console Playground decodes it and mounts the inline renderer, exercised end-to-end by the real-component Vitest guard. The event is not allowed to ship "ahead" of its consumer.
- **Run correlation depends on the ctx quadruple.** The event's `RunID` (the SSE frame's `run`) is read from `identity.QuadrupleFrom(ctx)`; the foreground Playground turn's `run` equals its task id, which is the key the bubble correlates on. A path that invokes an MCP tool without a run quadruple in ctx emits the event with an empty `RunID` (best-effort observability, never a failure) — the bubble then finds no matching agent message and the discovery is a no-op rather than a synthetic bubble.
- **Default-deny raw-HTML trust on the discovery.** The driver emits `RawHTMLTrusted=false` (the safe default) because the per-server trust posture lives on the registry, not the provider; the Console reconciles full trust via `mcp.servers.get`. This is the same posture the proxy projection carries — a discovery never grants raw-HTML trust by itself.
- **The proxy path also flows through the emit site.** Because both planner- and app-initiated MCP calls flow through `callTool`, an app-initiated tool result that declares an app also emits `mcp.app_available`. This is harmless (the proxy response already carries the app, and the page correlates by run) and consistent — any tool result with a `ui://` app surfaces the discovery.
- **Playwright real-route limitation (§4.3).** The rewritten `tests/mcp-app-displaymode.spec.ts` drives the real built Playground route under the standard `CONSOLE_AVAILABLE` skip (the repo's page-spec convention); it cannot trigger a runtime-emitted `mcp.app_available` without a real MCP app, so the deterministic, always-on real-component regression guard is the Vitest suite `mcp-app-discovery.spec.ts` (which fails if the discovery→render wiring is reverted). This is a §4.3 deviation from "drives the real route" recorded in the PR — the real-component INTENT is satisfied by the Vitest guard; the Playwright spec proves the same surface ships in the bundle.

## Glossary additions

- **MCP App discovery** — the runtime signal (`mcp.app_available`) that a tool result declared a `ui://` MCP App, consumed by the Console to mount the inline renderer for the run's turn. (Coordinator adds the canonical entry.)
- **`mcp.app_available`** — the canonical SafePayload event carrying the discovered app's server source id + `ui://` resource URI + display-mode hint + run/identity. (Coordinator adds the canonical entry.)
- **MCP App** / **`ui:// resource`** / **DisplayMode** — already defined (109a / D-062); not redefined here.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §6.4`, `RFC §6.5`, `RFC §7`, `brief 14`, `brief 11`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Multi-isolation: the discovery event is scoped to the call's `(tenant, user, session)` triple (+ run); the bus rejects an empty triple; the Console SSE subscription auto-scopes to the bearer's session.
- [x] Concurrent-reuse test — N/A: the MCP `Provider` gains no per-run state (`publishAppAvailable` reads the call's ctx); the existing concurrent-reuse coverage stands. Marked N/A with this reason.
- [x] **Integration test exists** — `TestE2E_MCPAppAvailable_PlannerPathEmitsEvent` (real MCP driver + real bus, identity + run propagation, the no-event negative branch, `-race`) plus the real-component Vitest guard.
- [x] If Protocol types changed: `server_id` single-sourced in `internal/protocol/types`, hand-synced into `protocol/mcp.ts`, generated docs regenerated + committed.
- [x] If new vocabulary: glossary updated (`MCP App discovery`, `mcp.app_available`).
- [x] If a brief finding was departed from: none (the Playwright real-route limitation is a §4.3 deviation recorded in Risks + the PR, not a brief departure).
