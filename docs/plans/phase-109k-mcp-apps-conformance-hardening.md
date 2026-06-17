# Phase 109k — mcp-apps-conformance-hardening

## Summary

The wave-end adversarial spec-compliance review of the MCP Apps band found two conformance-breaking FAILs (green against Harbor's own fixtures, broken against a real `io.modelcontextprotocol/ui` ext-apps server — the D-216 failure class) plus a set of host-obligation gaps a conformant app relies on. This phase hardens Harbor into a conformant MCP Apps host: it advertises the spec `mimeTypes` UI capability (not the hand-rolled `displayModes`), resolves app→host tool calls against the server namespace, sources display modes from the spec slot, and closes the size-changed / teardown / theme / host-context gaps — keeping the sanctioned deviations (D-173 manual-handler proxy, D-224 deployment-level declaration, D-225 durable tool-context) intact.

## RFC anchor

- RFC §6.4
- RFC §7

## Briefs informing this phase

- brief 14

## Brief findings incorporated

- brief 14 §3 (the "roots honesty" bar): *"correct behaviour for every capability you advertise."* The two FAILs both violate it — Harbor advertises a UI capability a conformant server cannot parse (wrong shape), and advertises `serverTools` it cannot service (unresolvable tool names). This phase makes every advertised capability honestly serviceable.
- brief 14 §2 (row 31, "Extension negotiation"): the extension must be populated in the shape the spec/SDK read. The 109h `displayModes` payload is not a `McpUiClientCapabilities` field; the spec field is `mimeTypes` (the SDK's `getUiCapability(caps).mimeTypes` gate, `RESOURCE_MIME_TYPE = "text/html;profile=mcp-app"`).
- brief 14 §6: the AppBridge dialect is the full host↔view contract (`size-changed`, `host-context-changed`, teardown, `tool-input`/`tool-result`). A host that pushes data but ignores `size-changed` / teardown / theme is a partial host.
- brief 14 §0 / §4 (the binding wording rule): never claim "MCP compliant" unscoped until conformance is substantiated. This phase is what lets the claim become true for the Apps surface; the `HARBOR_LIVE_MCP` probe is the substantiation.

## Findings I'm departing from (if any)

None. This phase corrects departures FROM the spec that the 109 band shipped; it does not introduce new ones. The sanctioned deviations (D-173, D-224's deployment-declaration intent, D-225, D-218) are preserved — only the wrong field shapes and missing host obligations are fixed.

## Goals

- **FAIL-1 — spec UI capability.** The MCP client advertises `ClientCapabilities.Extensions["io.modelcontextprotocol/ui"] = {"mimeTypes": ["text/html;profile=mcp-app"]}` (the spec field a conformant server gates on), advertised whenever MCP-Apps hosting is enabled. The non-spec `displayModes` capability payload is removed. A `RESOURCE_MIME_TYPE` const mirrors the SDK value.
- **Reconcile the 109h `display_modes` config to the spec slot.** Display modes are NOT a capability field — the spec carries them in the `ui/initialize` host-context `availableDisplayModes`. The deployment-declared `tools.mcp_app_host.display_modes` is surfaced on `runtime.info` capabilities so the Console sets `availableDisplayModes` from the deployment's declaration (closing the loop 109h opened, in the spec-correct place) instead of the Console hard-coding the set.
- **FAIL-2 — app→host tool resolution.** An app-initiated `tools/call` (bare server-side tool name, e.g. `get_weather`) resolves against the calling app's server namespace: the host prefixes the app's `serverID` (`<serverID>_<name>`) before catalog `Resolve`, so the call hits the right tool AND an app can only call its own server's tools (a confinement property).
- **`negotiateDisplayModes` non-spec read removed.** Stop reading `displayModes` off `ServerCapabilities` (not a spec field — always inert vs real servers); the host-side `availableDisplayModes` is the source of truth.
- **Host-obligation gaps closed:** consume `ui/notifications/size-changed` (adapt iframe height); send `ui/resource-teardown` before unmount + handle `request-teardown`; thread the live Console theme into `ui/initialize` host-context AND push `ui/notifications/host-context-changed` on theme toggle; populate host-context `toolInfo` + `containerDimensions`; wire `resources/templates/list` (`onlistresourcetemplates`) through `mcp.servers.*`.
- **Fold A cleanups:** correct the `toolCallId` godoc (newline-separated, not length-prefixed); align the devstack AppsSurface guard to fail-loud like cmd_dev; resolve the heavy-INPUT silent-`{}` asymmetry (deliver a faithful stub or document the decision in D-227).

## Non-goals

- The defensible omissions stay omitted (capabilities NOT advertised, so conformant apps degrade honestly): `ui/update-model-context`, `ui/message`, `ui/open-link`, `ui/download-file`, `sampling/createMessage`, logging `notifications/message`, `*/list_changed`, `sendToolInputPartial`, `tool-cancelled`. Adding any is a separate, capability-gated phase.
- D-173 stays: `connect-src 'none'` is NOT relaxed to honour a server-declared CSP `connectDomains` — all app traffic remains bridge-proxied (documented divergence; confirm no target app self-fetches).
- No change to the durable tool-context store (D-225) or the app-doc inline cap (D-218).

## Acceptance criteria

- [ ] `hostCapabilities` advertises `{"mimeTypes": ["text/html;profile=mcp-app"]}` under the UI extension; a unit test asserts the built `ClientCapabilities` carries `mimeTypes` (incl. the exact `RESOURCE_MIME_TYPE`) and NOT `displayModes`; roots (`RootsV2.ListChanged`) still preserved.
- [ ] A `HARBOR_LIVE_MCP`-gated probe drives a real ext-apps server that GATES UI-tool registration on `getUiCapability(caps).mimeTypes` and asserts its `ui://` tools ARE registered against Harbor (the FAIL-1 revert-guard — fails if reverted to `displayModes`).
- [ ] An app-initiated `mcp.apps.call_tool` with a bare server tool name resolves and invokes the correct `<serverID>_<name>` catalog tool; a test asserts a bare name resolves AND that an app cannot reach another server's tool (confinement).
- [ ] `negotiateDisplayModes` no longer reads a `displayModes` field off `ServerCapabilities`; available modes derive from the host/config; tests updated.
- [ ] `tools.mcp_app_host.display_modes` is surfaced on `runtime.info`; the Console's `AppBridgeHost` receives `availableDisplayModes` from it (not hard-coded); a test asserts the configured set reaches the host-context.
- [ ] The host consumes `ui/notifications/size-changed` and the inline iframe height tracks the app's reported content height (vitest with a fake bridge emitting size-changed).
- [ ] On unmount the host `await`s `ui/resource-teardown` before `bridge.close()`; `request-teardown` from the app is handled; tests assert the teardown is sent before close.
- [ ] The live Console theme is threaded into `ui/initialize` host-context, and a theme toggle pushes `ui/notifications/host-context-changed`; a test asserts both.
- [ ] Host-context carries `toolInfo` (the originating tool definition/id) and `containerDimensions`; a test asserts they are populated.
- [ ] `resources/templates/list` is handled (routed through the injected client); a test asserts a template list round-trips.
- [ ] `toolCallId` godoc corrected; devstack AppsSurface guard fails loud (parity with cmd_dev); heavy-input asymmetry resolved or recorded in D-227.
- [ ] All gates green: Go `-race`, `svelte-check --fail-on-warnings`, lint, build, drift-audit, the no-direct-transport spy still passes (the new delivery/teardown/size paths use only the injected client / the bridge), `scripts/smoke/phase-109k.sh` OK > 0 / FAIL = 0.

## Files added or changed

- `internal/tools/drivers/mcp/mcp.go` — `RESOURCE_MIME_TYPE`; `hostCapabilities` → `mimeTypes`; remove the `displayModes` capability payload; remove the non-spec `negotiateDisplayModes` server read (or re-source).
- `internal/config/config.go` / `internal/config/validate.go` — keep `tools.mcp_app_host.display_modes` as the deployment's declared renderable modes (now consumed via runtime.info, not the capability).
- `internal/protocol/...` + the `runtime.info` builder — surface the configured MCP-app display modes on the capabilities projection.
- `internal/mcpconsole/apps.go` — (if backend-side resolution is chosen) resolve within the server namespace; otherwise unchanged (frontend prefixes).
- `web/console/src/lib/chat/renderers/app-bridge-host.ts` — `oncalltool` prefixes `serverID`; `onlistresourcetemplates`; `size-changed` listener seam; `teardownResource` on close + `onrequestteardown`; thread theme + `setHostContext`/host-context-changed; populate `toolInfo` + `containerDimensions`; `MCPAppHostClient` gains `listResourceTemplates`.
- `web/console/src/lib/chat/renderers/mcp-app.svelte` — iframe height tracks size-changed; pass live theme + `availableDisplayModes` (from runtime.info) + `toolInfo`.
- `web/console/src/lib/mcp-app-host-client.ts` — `listResourceTemplates` adapter; surface runtime.info display modes.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — read runtime.info modes + theme, pass to the renderer.
- Specs: `app-bridge-host.spec.ts`, `mcp-app-host-client.spec.ts`, the Go `mcp`/`mcpconsole`/`config` tests, + a `HARBOR_LIVE_MCP` capability-gating probe.
- `docs/plans/phase-109k-mcp-apps-conformance-hardening.md`, `scripts/smoke/phase-109k.sh`, `docs/plans/README.md`, `docs/decisions.md` (D-227).

## Public API surface

- `mcp.MCPAppHostClient` (TS) gains `listResourceTemplates`.
- `runtime.info` capabilities gain the configured MCP-app display modes (read-only projection).
- No new Harbor Protocol METHOD; `mcp.apps.call_tool` semantics unchanged on the wire (the name is prefixed host-side). If backend-side resolution is chosen instead, `mcp.apps.call_tool` gains an optional `server_id` (decided in D-227).

## Test plan

- **Unit:** capability `mimeTypes` shape + roots-preserved (Go); bare-tool-name resolution + cross-server confinement; `negotiateDisplayModes` no longer reads the server field; runtime.info carries configured modes. Frontend vitest: size-changed→height, teardown-before-close, theme init + host-context-changed, toolInfo/containerDimensions populated, listResourceTemplates round-trip, oncalltool prefix.
- **Integration:** the `HARBOR_LIVE_MCP` probe against a real ext-apps server that gates on `mimeTypes` (FAIL-1 revert-guard) and exercises an app→server tool callback (FAIL-2) — per §17.8 the fixture/probe derives from the real spec/SDK, not a hand blob.
- **Conformance:** N/A new driver; the capability shape is asserted against the vendored SDK's `McpUiClientCapabilities` / `getUiCapability` (ground truth).
- **Concurrency / leak:** the `Provider` / `AppsAccessor` concurrent-reuse tests are retained (immutable artifacts unchanged).

## Smoke script additions

- `scripts/smoke/phase-109k.sh` — static guards: `mcp.go` advertises `mimeTypes` (RESOURCE_MIME_TYPE) and no longer the `displayModes` capability; `app-bridge-host.ts` wires `size-changed` + `teardownResource` + theme/host-context + `serverID`-prefixed `oncalltool`; `mcp.apps.call_tool` resolution path present. SKIP→OK as the surface lands.

## Coverage target

- `internal/tools/drivers/mcp`, `internal/mcpconsole`, `internal/config`: maintain/raise the package baselines; the changed branches fully covered.
- `web/console` chat module: maintain/raise the `app-bridge-host` coverage; every new delivery/teardown/size/theme branch covered.

## Dependencies

- 109a (the MCP Apps runtime + capability read/write surface), 109b (the AppBridge host), 109h (the capability advertisement this corrects), 109i (the tool-context surface), 109j (the data-delivery push). All shipped on main.

## Risks / open questions

- **Config reconciliation (the key D-227 decision).** Recommended: capability advertises constant `mimeTypes`; `display_modes` config is surfaced via `runtime.info` for the Console's `availableDisplayModes`. Alternative (smaller): capability `mimeTypes` only, Console keeps owning `availableDisplayModes`, and `display_modes` becomes advisory/documented. Pick in D-227; do NOT silently drop the just-shipped config field without recording it (§10 backward-compat).
- **FAIL-2 placement.** Frontend `serverID` prefix is minimal and gives app→own-server confinement for free; a backend `server_id` on `mcp.apps.call_tool` is more defensive but a Protocol change. D-227 records the choice.
- **Live-test dependency.** The FAIL-1/FAIL-2 revert-guards need a real ext-apps server that gates on `mimeTypes` and exposes a callback tool (a Dockyard MCP server or go-study-mcp variant). If none is available at implementation time, capture a real server transcript as the fixture (§17.8) and `t.Skip` the live probe with a tracking note — never a hand blob.
- **CSP `connectDomains` (D-173).** Confirmed out of scope; documented divergence.

## Glossary additions

- None new — refines existing terms (MCP UI-host capability now correctly defined as the spec `mimeTypes` extension; display modes negotiated via the `ui/initialize` host-context, not the capability).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation: app→host tool-call confinement test passes; identity paths unchanged
- [ ] **Reusable artifact:** Provider / AppsAccessor concurrent-reuse tests retained under `-race`
- [ ] **Consumes shipped surfaces + closes spec-conformance seams:** the `HARBOR_LIVE_MCP` probe drives a real ext-apps server end-to-end (capability gating + tool callback); per §17.8 the fixture derives from the real spec/SDK
- [ ] If Protocol types changed (runtime.info / optional call_tool server_id): single-sourced + `protocol.ts`/docs mirrored
- [ ] If config changed: example + backward-compat recorded in D-227
- [ ] `svelte-check --fail-on-warnings` + the no-direct-transport spy pass
- [ ] **Orchestrator LIVE regression test (binding pre-merge gate).** Before this PR merges, the orchestrator boots the test agent + the Console and drives the FULL MCP Apps surface end-to-end against a real ext-apps server (stdio): tool call → `mcp.app_available` discovery → sandboxed `ui://` render → data delivery (`sendToolInput`/`sendToolResult` populating the app) → an app→server tool callback → a display-mode change. The surface worked BEFORE the 109 wave; this gate proves the 109h–k fixes did not regress it. Evidence (screenshots / observed events) attached to the PR. A live regression blocks merge.
- [ ] If a brief finding was departed from: N/A — none departed.
