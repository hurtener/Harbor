# Phase 109a — mcp-apps-runtime-protocol

## Summary

Build the runtime + Protocol enablement layer for MCP Apps — the official `io.modelcontextprotocol/ui` (ext-apps) extension, where an MCP tool declares an interactive HTML UI via a `ui://` resource referenced from a tool result's `_meta.ui.resourceUri`. This phase makes the runtime carry the app reference end-to-end: the MCP driver parses `_meta.ui.resourceUri` and recognises `ui://`-scheme resources distinctly; the Protocol projects the app reference (resourceUri + negotiated DisplayMode + trust flag) onto the surface the Console reads; a new `mcp.servers.read_resource` Protocol method fetches the `ui://` HTML under the identity triple (honouring the D-026 heavy-content safety net); `DisplayModes` are negotiated from the server's advertised UI capability instead of the static registration placeholder; and an app-initiated tool-call proxy routes back through the existing identity + approval-gate + tool-side-OAuth invocation path with no new bypass. This phase **supersedes the deprecated Phase 85g** (D-172): 85g asserted the runtime driver was unchanged for Apps, which is factually wrong against the current code — there is no `_meta` slot on tool-result content, no result content on `tool.completed`, no Protocol exposure of `ReadResource`, and the DisplayMode wire fields are static placeholders. 109a corrects all four.

## RFC anchor

- RFC §6.4
- RFC §6.5
- RFC §7

## Briefs informing this phase

- brief 14
- brief 11

## Brief findings incorporated

- brief 14 §6: "MCP Apps … the AppBridge `postMessage` JSON-RPC dialect (`ui/initialize`, tool-call proxying, model-context updates, host-pushed data)." — the tool-call proxy is part of the Apps contract; 109a lands the runtime-side proxy method that the 109b AppBridge calls, and binds it to the existing tool-safety policy rather than a fresh path.
- brief 14 §6: "Render in a **sandboxed iframe** … host-controlled permissions." — the host controls what an app may do. The runtime expression of "host-controlled" is that an app-initiated tool call re-enters the same identity + approval + OAuth gate a planner call does. 109a makes that the only path; 109b consumes it.
- brief 14 §2 (#30): "`registry.go` carries `DisplayModes` … projection fields … set at registration time." — 109a replaces that static placeholder with real negotiation from the server's `io.modelcontextprotocol/ui` capability, so the projected modes reflect what the server actually advertises.
- brief 14 (MCP client compliance): `ui://`-scheme resources are a distinct resource class within the Apps extension — they are fetched via the standard `resources/read` MCP method but are semantically the app's UI document, not arbitrary content. 109a recognises the scheme distinctly so an ordinary `file://` / `https://` resource is never mistaken for an app.
- brief 11 (Console feature surface): the Console is a pure Protocol client — it has no method to fetch a `ui://` resource today. brief 11's surface inventory requires that every Console-rendered datum reach it through a typed Protocol method/event. 109a supplies the missing `mcp.servers.read_resource` method and the tool-result app-ref projection so 109b can render without reading runtime internals.

## Deviations (§4.3, recorded at implementation)

- **App-ref projection home.** The Public API section placed the `{resourceUri, displayMode, rawHTMLTrusted}` projection field on "the existing tool-result Protocol projection (`internal/protocol/types/tools.go`)". There is no tool-result projection in `tools.go` (it holds catalog/manifest types only — consistent with D-172's finding that `tool.completed` carries no result content). The field landed as `types.MCPAppRef` on the new `mcp.apps.call_tool` response (`types/mcp_apps.go`) — the surface that actually carries a tool result. No RFC impact; this is a §4.3 simpler-home deviation, not a decision-grade change (D-212 not filed).
- **Loud bypass event.** D-026's "loud bypass event" is realised as a new canonical event `mcp.resource_offloaded` (registered in the MCP driver, emitted by the runtime-side `mcpconsole.AppsAccessor`), mirroring `EventTypeMCPResourceUpdated`. By-reference shape is `types.MCPResourceArtifactRef` (mirrors `MemoryArtifactRef`) since `types` has no `ArtifactStub`.
- **Surface routing.** Both methods route through a NEW `AppsSurface` + `IsMCPAppsMethod` predicate (not the existing twelve-method `MCPSurface`), because read_resource needs the ArtifactStore/threshold and the proxy needs the tool catalog — deps the Console `MCPSurface` does not carry. Keeps the shipped `MCPSurface` untouched.

## Findings I'm departing from (if any)

- Departing from Phase 85g's central claim — brief-adjacent, since 85g's plan text encoded it. 85g §"Non-goals" stated: "Apps is purely Console-side; `internal/tools/drivers/mcp` is unchanged except where it already projects the Apps metadata." That is incorrect against the shipped code (`content.go` has no `_meta` slot; `tool.completed` carries no result content; `ReadResource` is not on the Protocol; `DisplayModes` are static placeholders in `registry.go`). 109a deliberately departs: the runtime driver and Protocol surface DO change. The departure is recorded in `docs/decisions.md` (D-172 deprecates 85g; D-173 records the 3-phase 109a/b/c reshape — filed by the coordinator).

## Goals

- Parse `_meta.ui.resourceUri` on MCP tool results and recognise `ui://`-scheme resources distinctly from ordinary resources.
- Project the app reference (the `ui://` resourceUri + the DisplayMode negotiated for that result + the RawHTMLTrusted trust flag) onto the Protocol surface that reaches the Console, using single-source wire types in `internal/protocol/types` only.
- Add a Protocol method `mcp.servers.read_resource` that fetches a `ui://` resource's HTML scoped to the request identity triple `(tenant, user, session)`, honouring the D-026 heavy-content safety net — content meeting the heavy threshold routes through the ArtifactStore by reference with a loud bypass event, never a silent truncation or inline leak.
- Negotiate `DisplayModes` from the server's advertised `io.modelcontextprotocol/ui` capability, replacing the static placeholder set at registration time in `registry.go`.
- Add an app-initiated tool-call **proxy** Protocol method that routes into the EXISTING identity + approval-gate (Phase 31) + tool-side-OAuth (Phase 30) tool-invocation path — the same unified pause/approval/OAuth tool-safety policy planner-initiated calls use. No new bypass; identity is mandatory; fail loudly on missing identity.

## Non-goals

- The iframe renderer + AppBridge `postMessage` wiring + inline rendering — that is **Phase 109b**.
- The fullscreen / picture-in-picture layout host — that is **Phase 109c**.
- Authoring MCP Apps — Harbor *hosts* apps; building the `ui://` document is a server-author concern.
- Persisting app state across sessions — an app instance is conversation-scoped; no cross-session state store.
- Wire-level Apps capability-negotiation conformance harness — Phase 85j owns the band conformance surface (cross-ref under Test plan).

## Acceptance criteria

- [x] `_meta.ui.resourceUri` on an MCP tool result is parsed by the driver and surfaced on the tool-result Protocol projection, with a unit test over a fixture result that carries the `_meta.ui` slot. — `parseAppRef` in `content.go`; `TestParseAppRef_*` + `TestLowerCallToolResult_SurfacesAppRef`; projected onto `types.MCPAppCallToolResponse.App` (see Deviations: the projection lands on the proxy response, not `tools.go`, because no tool-result projection exists there per D-172).
- [x] `ui://`-scheme resources are recognised distinctly: a result referencing a `ui://` resource is flagged as an app; an ordinary `file://` / `https://` resource is NOT treated as an app (negative unit test). — `IsUIResourceURI`; `TestParseAppRef_RejectsNonUIScheme`.
- [x] `mcp.servers.read_resource` returns the `ui://` resource content scoped to the request identity triple; a request missing any identity component is rejected fail-closed (not silently served), asserted by test. — `Provider.ReadResource` + `Registry.ReadResource` + `AppsSurface.handleReadResource`; `TestProvider_ReadResource_FailsClosedOnMissingIdentity`, `TestRegistry_ReadResource_EndToEnd`, `TestAppsSurface_ReadResource_FailsClosedOnMissingIdentity`.
- [x] `read_resource` honours D-026: content ≥ the heavy threshold is routed via an ArtifactStore reference and a loud bypass event is emitted; it is never inlined past the threshold and never silently truncated. — `AppsAccessor.offload` emits `mcp.resource_offloaded`; `TestAppsAccessor_ReadResource_HeavyOffload` asserts the artifact ref + the loud event; the shared `config.DefaultHeavyOutputThresholdBytes` is the threshold source.
- [x] `DisplayModes` is populated from the server's `io.modelcontextprotocol/ui` capability negotiation — a fake server advertising inline/fullscreen/pip yields exactly those modes on the projection — NOT the static registration placeholder. — `negotiateDisplayModes` (replaces the static `ServerRegistration.DisplayModes` placeholder; the registry now reads `Provider.DisplayModes()`); `TestNegotiateDisplayModes`, `TestProvider_DisplayModes_NegotiatesFromCapability`, `TestRegistry_ReadResource_EndToEnd`.
- [x] The app-tool-call proxy routes through the existing approval / OAuth / identity tool-safety path: an app call to a gated tool parks on the SAME pause primitive as a planner-initiated call (asserted to NOT bypass the gate). — `AppsAccessor.CallTool` invokes the catalog descriptor's wrapped `Invoke`; `TestE2E_MCPApps_ProxyParksOnGate` asserts the pause record exists before approval (fails if the gate is bypassed).
- [x] The new Protocol method is registered in `internal/protocol/methods/methods.go`; new wire types live ONLY in `internal/protocol/types`; any new error code lives ONLY in `internal/protocol/errors`. — `MethodMCPReadResource` / `MethodMCPAppsCallTool` in methods.go; wire types in `types/mcp_apps.go`; no new error code needed (reused `CodeNotFound` / `CodeIdentityRequired` / `CodeInvalidRequest` / `CodeRuntimeError`).
- [x] `scripts/smoke/phase-109a.sh` exercises `mcp.servers.read_resource` (`skip_if_404` until shipped) and asserts the method name is wired. — POSTs both methods (asserting `invalid_request` to prove the surface dispatches, not 404), plus static method + `_meta.ui` parse greps.
- [x] If the MCP driver gains per-run state for app handling, a concurrent-reuse test (N≥100 invocations against a single shared driver instance, under `-race`) passes with no data races, context bleed, cross-cancellation, or goroutine leaks. — The driver gained NO per-run state (`ReadResource` / `DisplayModes` / `parseAppRef` are pure per-call). The new reusable artifact is `mcpconsole.AppsAccessor`; `TestAppsAccessor_ConcurrentReuse` (N=128, `-race`) covers it.

## Files added or changed

```text
internal/tools/drivers/mcp/
  content.go        # add the _meta slot + app-reference shape on tool-result content
  mcp.go            # ui:// scheme recognition; expose ReadResource for the read_resource method
  registry.go       # real DisplayMode negotiation from the io.modelcontextprotocol/ui capability
internal/protocol/
  types/            # app-ref projection field; read_resource request/response wire types (single source)
  methods/methods.go  # the new method name "mcp.servers.read_resource" (+ proxy method name)
  errors/           # new error code, only if one is genuinely needed
  mcp.go            # projection: tool-result app-ref + read_resource handler glue
cmd/harbor/         # method wiring into the Protocol server's dispatch table
scripts/smoke/
  phase-109a.sh     # exercises mcp.servers.read_resource; static _meta-parse assertion
docs/decisions.md   # references D-172 (85g deprecation) / D-173 (109 reshape) — filed by coordinator
docs/plans/README.md  # status flip on merge — done by coordinator
```

## Public API surface

Single-source rule: every wire type below lives in `internal/protocol/types`; every method name in `internal/protocol/methods/methods.go`; every error code in `internal/protocol/errors`. Go-flavoured signatures:

- **New Protocol method `mcp.servers.read_resource`** — fetch a `ui://` resource's content under the identity triple.

  ```go
  // package types
  type ReadMCPResourceRequest struct {
      ServerID    string `json:"server_id"`
      ResourceURI string `json:"resource_uri"` // expected ui:// scheme for app fetches
  }

  type ReadMCPResourceResponse struct {
      ResourceURI string `json:"resource_uri"`
      MIMEType    string `json:"mime_type"`
      // Exactly one of Content / ArtifactRef is set. ArtifactRef is used when the
      // content meets the D-026 heavy threshold (loud-bypass event emitted).
      Content     string         `json:"content,omitempty"`
      ArtifactRef *ArtifactStub  `json:"artifact_ref,omitempty"`
  }
  ```

- **New app-tool-call proxy method** (e.g. `mcp.apps.call_tool`) — an MCP App asks the host to invoke an MCP tool; the request carries the identity triple from the Protocol context and re-enters the existing tool-invocation path (Phase 30 OAuth + Phase 31 approval gate). No new safety semantics are introduced; the proxy is a thin re-entry, not a parallel path.

- **Tool-result app-ref projection field** — on the existing tool-result Protocol projection (`internal/protocol/types/tools.go`), a typed field carrying `{ resourceUri (ui://), displayMode (negotiated, D-062), rawHTMLTrusted }`. Empty for non-app tool results.

No new runtime-internal Go surface is exported to the Console; the Console reads only the projection + method results.

## Test plan

- **Unit:** driver `_meta.ui.resourceUri` parse over a fixture tool result (present and absent); `ui://`-scheme recognition (positive) vs `file://` / `https://` (negative — not flagged as app); DisplayMode negotiation from a fake `io.modelcontextprotocol/ui` capability advertising inline/fullscreen/pip yields exactly those modes; the D-026 threshold branch (sub-threshold → inline `Content`; over-threshold → `ArtifactRef` + bypass event).
- **Integration:** `mcp.servers.read_resource` end-to-end through the Protocol server with REAL drivers (real MCP driver against a fake MCP server, real ArtifactStore driver), asserting identity propagation through every layer; ≥2 failure modes — (a) a request with a missing identity component is rejected fail-closed, (b) an unknown / non-existent resource URI returns the typed error, not a panic or empty success. Plus the load-bearing proxy test: an app-initiated call to a gated tool parks on the same pause primitive as a planner call (asserted to NOT bypass approval/OAuth). Run under `-race`.
- **Conformance:** N/A — Phase 85j owns the band conformance harness covering wire-level Apps capability negotiation; 109a cross-refs it and does not duplicate the wire-conformance suite. 109a's negotiation test uses a fake capability fixture, not the conformance band.
- **Concurrency / leak:** if the MCP driver gains per-run app-handling state, a concurrent-reuse test runs N≥100 `read_resource` / tool-result-projection invocations against a single shared driver instance under `-race`, asserting no data races, no context bleed (run A's `ui://` content never reaches run B), no cross-cancellation, and `runtime.NumGoroutine()` restored to baseline after teardown.

## Smoke script additions

- `scripts/smoke/phase-109a.sh` (classification: `live-server`):
  - `protocol_call 'mcp.servers.read_resource' '{...}'` with `skip_if_404` — SKIPs until the method ships, OK once wired.
  - Assert the method name `mcp.servers.read_resource` is registered (wired into the Protocol dispatch surface).
  - Static assertion that `_meta.ui.resourceUri` parsing exists in `internal/tools/drivers/mcp` (grep the driver for the `_meta.ui` parse path).

## Coverage target

- `internal/tools/drivers/mcp`: 85%
- `internal/protocol`: 80%

## Dependencies

- 28 (MCP driver — the tool catalog's MCP transport).
- 85a (MCP client core-compliance — the driver's discovery + invocation surface must be sound).
- 84a (Protocol capability / session-aggregate surface — the negotiated-capability plumbing this phase reads DisplayModes from).
- Consumer **109b** (Console iframe + AppBridge + inline rendering) lands in the SAME wave, satisfying the §13 primitive-with-consumer rule.

## Risks / open questions

- **§13 same-wave consumer.** The primitives this phase introduces — the `ui://` app-ref projection field, `mcp.servers.read_resource`, and the app-tool-call proxy method — get their first consumer in the SAME wave: Phase 109b (the Console iframe renderer + AppBridge). This is explicit and binding; the proxy and read_resource are not allowed to ship "ahead" of 109b into a wave where nothing exercises them.
- **D-062 DisplayMode semantics are already glossary-defined; this phase makes them real.** Until now `DisplayModes` / `DisplayModesAdvertised` were static placeholders set at registration time. The risk is a negotiation that silently falls back to the placeholder when the capability is absent — the negotiation must produce an empty/explicit set when the server advertises no `io.modelcontextprotocol/ui` capability, never a stale default. Do not redefine DisplayMode (D-062 owns it).
- **Heavy-content `ui://` HTML must obey D-026.** An app's HTML document can be large. The threshold decision is the open question: 109a reuses the existing runtime heavy-output threshold and routes over-threshold content through the ArtifactStore by reference with a loud bypass event (`llm.context_leak`-style loud emission), never an inline leak and never a silent truncation. Flag at review: confirm the threshold constant is the shared runtime one, not a new MCP-local number.
- **The app-tool-call proxy is a NEW call site into the tool-safety path.** The risk is accidentally minting a gate bypass — an app call that skips approval/OAuth/identity because it entered through a different door. The load-bearing gate is the integration test asserting that an app call to a gated tool parks on the same pause primitive as a planner call. If that test cannot be made to fail when the gate is removed, it is not testing the right thing.

## Glossary additions

- **MCP App** — an interactive HTML UI an MCP tool declares via the `io.modelcontextprotocol/ui` (ext-apps) extension, referenced from a tool result's `_meta.ui.resourceUri`. (Coordinator adds the canonical entry.)
- **`ui:// resource`** — the resource-scheme an MCP App's UI document is fetched under; recognised distinctly from ordinary `file://` / `https://` resources. (Coordinator adds the canonical entry.)
- **DisplayMode** — already defined (D-062). NOT redefined here; this phase makes the negotiated value real.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §6.4`, `RFC §6.5`, `RFC §7`, `brief 14`, `brief 11`) resolve
- [ ] Coverage on touched packages ≥ stated target (`internal/tools/drivers/mcp`: 85%; `internal/protocol`: 80%)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — `read_resource` and the app-tool-call proxy run under the request's `(tenant, user, session)` triple; a missing component is rejected fail-closed.
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes — N≥100 concurrent invocations against a single shared MCP driver instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** If the driver gains no per-run app state, mark N/A with that reason.
- [ ] **Integration test exists** (`test/integration/<topic>_test.go` or in-package adapter test): real MCP driver + real ArtifactStore on the seam, identity propagation asserted, ≥1 failure mode (missing-identity rejection AND unknown-uri error), runs under `-race`. Not N/A — `Dependencies` names shipped phases 28 / 85a / 84a.
- [ ] If new vocabulary: glossary updated (`MCP App`, `ui:// resource` — coordinator files the canonical entries)
- [ ] If a brief finding was departed from: justified above (the 85g "runtime unchanged" departure) + decisions.md entry filed (D-172 / D-173, by coordinator)
