# Phase 109e — mcp-app-tool-def-discovery

## Summary

A live test against a real `io.modelcontextprotocol/ui` ext-apps server (go-study-mcp) found the 109 wave's MCP App discovery inert: it parsed the `_meta.ui.resourceUri` app reference from the tool **RESULT** (`CallToolResult._meta`), but the canonical spec places it on the tool **DEFINITION** (`Tool._meta.ui`). A real ext-apps server binds the `ui://` UI resource per tool in `tools/list` and returns an empty result `_meta`, so Harbor's result-parse found nothing and `mcp.app_available` never fired — the 109b renderer + 109c layout were unreachable against real servers. This phase captures the tool-definition `_meta.ui` at discovery, fires `mcp.app_available` (and feeds the app-tool-call proxy projection) from that binding on invocation of a UI-bound tool, keeps the result `_meta.ui` as a secondary display-mode merge, and corrects the test fixtures to the canonical placement (plus a `HARBOR_LIVE_MCP`-gated probe against the real binary).

## RFC anchor

- RFC §6.4
- RFC §6.5
- RFC §7

## Briefs informing this phase

- brief 14
- brief 11

## Brief findings incorporated

- brief 14 (MCP client compliance): `ui://`-scheme resources are a distinct resource class declared via the `io.modelcontextprotocol/ui` extension. The canonical dialect binds the UI resource to the **tool**, so the discovery must read the tool DEFINITION's `_meta.ui.resourceUri` (captured at `tools/list`), not the tool result — this is the placement a real ext-apps server uses.
- brief 14 §6: "MCP Apps … Render in a **sandboxed iframe** … the AppBridge `postMessage` JSON-RPC dialect." — the renderer (109b) and layout (109c) already ship; this phase only corrects the DISCOVERY source so the host actually surfaces an app a real server declared.
- brief 11 (Console is a pure Protocol client): every rendered datum reaches the Console through a typed Protocol method/event. The discovery event `mcp.app_available` (D-215) is the only planner-path carrier; correcting its source keeps the Console honest as a Protocol client (it never grows a private hook to read internals).
- brief 11 §"Playground": the inline renderer mounts on a `{resourceUri, serverID}` reference and honours DisplayMode; a UI-bound tool with no negotiated/declared mode must still surface, defaulting to inline — the renderer's existing `app?.displayMode || 'inline'` default already satisfies this.

## Findings I'm departing from (if any)

None.

## Goals

- Capture the tool-DEFINITION `_meta.ui.resourceUri` at discovery (`ListTools`), bound to the tool, as the spec-conformant source of the app reference — kept MCP-driver-local and immutable after discovery (D-025), captured by value in the descriptor's `Invoke` closure (no shared mutable `Provider` state).
- Fire `mcp.app_available` on invocation of a UI-BOUND tool, from the captured binding — replacing the prior "fire when the result `_meta.ui` is present" trigger.
- Keep the result `_meta.ui` parse as a SECONDARY merge: a per-result display-mode hint wins over the binding's mode for that result; the result is never REQUIRED (conformant servers leave it empty).
- Feed the SAME corrected `value.AppRef` to BOTH the discovery event and the app-tool-call proxy projection (`mcpconsole/apps.go::appRefFromValue`), fixing the §17.6 bug-twin in the proxy path in the same change.
- Default DisplayMode to inline when no mode is negotiated/declared; confirm the Console renderer mounts on a bare `{resourceUri, serverID}` with no displayMode (no Console change required).
- Correct the test fixtures to the canonical placement (tool-def `_meta.ui`, empty result `_meta`) and add a `HARBOR_LIVE_MCP`-gated probe against the real go-study-mcp binary.

## Non-goals

- The `mcp.app_available` event shape, `MCPAppRef.server_id`, the iframe renderer, the AppBridge, and the fullscreen/pip layout (Phases 109a–d — unchanged; this phase only corrects the SOURCE the runtime reads the app reference from).
- Authoring MCP Apps — Harbor *hosts* apps; building the `ui://` document is a server-author concern.
- A new Protocol method, error code, event type, or wire type — none change.
- `tool.visibility` (`["model","app"]`) handling from the tool `_meta.ui` — out of scope; only `resourceUri` is consumed.

## Acceptance criteria

- [x] The MCP driver captures the tool-DEFINITION `_meta.ui.resourceUri` at discovery (`buildToolDescriptor` parses `t.Meta`) and binds it to the tool immutably (closure capture). — `mcp.go::buildToolDescriptor` (`toolApp := parseAppRef(t.Meta)`).
- [x] A planner-initiated call to a tool that declares a `ui://` app on its DEFINITION emits `mcp.app_available` on the real bus EVEN when the result `_meta` is empty (the conformant golden path), carrying serverID + resourceURI + run/identity. — `mcp.go::callTool` (`reconcileAppRef` + `publishAppAvailable`); `TestE2E_MCPAppAvailable_PlannerPathEmitsEvent` (corrected fixture: tool-def `_meta.ui`, empty result `_meta`).
- [x] A per-result `_meta.ui` display-mode hint merges over the tool-binding default (result wins for display mode; binding is the source of the resourceURI). — `reconcileAppRef` + `uiDisplayModeHint`; `TestMCPAppAvailable_ResultHintMergesOverBinding`.
- [x] A tool with NO `_meta.ui` binding (and no result hint) emits NO discovery event. — `TestMCPAppAvailable_PlainResultEmitsNoEvent`.
- [x] The reconciled app reference is surfaced on `MCPToolValue.AppRef`, the source the app-tool-call proxy projects from — fixing the proxy path's identical result-only bug. — `callTool` sets `value.AppRef`; asserted in `TestE2E_MCPAppAvailable_PlannerPathEmitsEvent`.
- [x] A UI-bound tool with no negotiated/declared mode surfaces as renderable, defaulting to inline; the Console renderer mounts on a bare `{resourceUri, serverID}`. — event `DisplayMode == ""`; `mcp-app.svelte`'s `data-display-mode={app?.displayMode || 'inline'}` (unchanged).
- [x] A `HARBOR_LIVE_MCP`-gated probe drives the real go-study-mcp binary over stdio and asserts a UI-bound tool call fires `mcp.app_available` from the tool-definition `ui://`; CI skips it. — `mcp_live_test.go::TestLive_MCPAppAvailable_RealExtAppsServer` (verified green in dev).
- [x] `scripts/smoke/phase-109e.sh` pins the tool-def capture, the reconcile, the corrected fixture, and the live probe; static-only, FAIL = 0.

## Files added or changed

```text
internal/tools/drivers/mcp/
  content.go               # parseAppRef godoc (def OR result); uiDisplayModeHint; reconcileAppRef; AppRef/MCPToolValue.AppRef godoc
  mcp.go                   # buildToolDescriptor captures tool-def binding; callTool(toolApp); reconcile + emit
  events.go                # EventTypeMCPAppAvailable + DisplayMode godoc → tool-definition source
  mcp_app_available_test.go  # CORRECTED fixture (tool-def _meta.ui, empty result _meta) + merge test + negative test
  mcp_live_test.go         # NEW: HARBOR_LIVE_MCP-gated real-go-study-mcp stdio probe
internal/mcpconsole/
  apps.go                  # CallTool + appRefFromValue godoc → tool-definition source (no logic change; fed the corrected value.AppRef)
docs/plans/README.md       # 109e row + detail block
docs/decisions.md          # D-216
docs/glossary.md           # mcp.app_available / MCP App discovery / MCP App / ui:// resource → tool-definition placement
scripts/smoke/phase-109e.sh
CLAUDE.md / AGENTS.md       # §17.8 (real-spec conformance fixtures) — verbatim-identical
docs/skills/use-the-harbor-protocol/SKILL.md   # discovery-semantics note (tool-definition source)
```

## Public API surface

No new exported Go surface and no Protocol-wire change. `Provider.callTool` gains an internal `toolApp *AppRef` parameter (unexported). `content.go` adds two unexported helpers (`uiDisplayModeHint`, `reconcileAppRef`). The `mcp.app_available` event + `MCPAppRef` are unchanged from D-215; only the SOURCE the runtime reads the app reference from changed.

## Test plan

- **Unit:** `reconcileAppRef` precedence (binding is the resourceURI source; result mode wins) is covered through the integration tests; `uiDisplayModeHint` extraction is exercised by the merge case.
- **Integration:** (Go, `-race`) `TestE2E_MCPAppAvailable_PlannerPathEmitsEvent` — a planner-path call (through the real `Invoke` closure) to a tool declaring `_meta.ui` on its DEFINITION with an EMPTY result `_meta` emits `mcp.app_available` on the real inmem bus + surfaces `value.AppRef`; `TestMCPAppAvailable_ResultHintMergesOverBinding`; `TestMCPAppAvailable_PlainResultEmitsNoEvent`. The mcpconsole proxy path is covered transitively (it reads the same `value.AppRef`).
- **Conformance:** the canonical `io.modelcontextprotocol/ui` placement is the gate — the fixture derives from the vendored `McpUiToolMetaSchema` (tool meta), and `mcp_live_test.go` drives the real go-study-mcp binary (`HARBOR_LIVE_MCP=1`), the real-server regression guard (§17.8).
- **Concurrency / leak:** N/A — the tool binding is captured immutably by value in the `Invoke` closure (no per-run `Provider` state added); the existing `TestProvider_*ConcurrentReuse` coverage stands.

## Smoke script additions

- `scripts/smoke/phase-109e.sh` (classification: `static-only`):
  - Assert `buildToolDescriptor` captures the tool-definition binding (`parseAppRef(t.Meta)`) and `callTool` takes the binding (`toolApp *AppRef`).
  - Assert `reconcileAppRef` + `uiDisplayModeHint` exist in `content.go`.
  - Assert the corrected Go fixture declares `_meta.ui` on the tool DEFINITION (`Tool{ … Meta: mcpsdk.Meta{"ui"`) — and NOT on the result of the `weather` tool.
  - Assert the negative test + the merge test + the `HARBOR_LIVE_MCP`-gated probe exist.
  - Assert the Console renderer keeps the inline default (`app?.displayMode || 'inline'`).

## Coverage target

- `internal/tools/drivers/mcp`: 85% (the existing target; the capture + reconcile are covered by the integration + negative + merge tests).
- `internal/mcpconsole`: unchanged (no logic change; the corrected `value.AppRef` flows through the existing proxy test surface).

## Dependencies

- 109a (the MCP driver discovery + invocation surface; the `MCPAppRef` projection + the proxy).
- 109b / 109c (the renderer + layout the discovery feeds — unchanged).
- 109d / D-215 (the `mcp.app_available` event + `MCPAppRef.server_id` whose SOURCE this corrects).
- 28 / 85a (the MCP southbound driver).

## Risks / open questions

- **§13 primitive-with-consumer.** No new primitive — this corrects an existing one. The consumer is the existing 109d render path (and the proxy), now fed from the spec-conformant source; the corrected Go integration test + the live probe exercise it end-to-end.
- **Result-only fallback.** A server that (non-conformantly) declares the full app only on the result still surfaces via the `resultHint` fallback in `reconcileAppRef`. This is a graceful superset, not a second parallel trigger (§13): one reconcile with a documented precedence (tool binding is the resourceURI source).
- **Display mode is empty on the golden path.** The canonical tool `_meta.ui` carries no display mode and go-study-mcp does not advertise the UI capability, so `DisplayMode` is empty and the renderer defaults to inline. A server that wants fullscreen/pip supplies a per-result hint; this is the documented merge.
- **Live probe needs the real binary + key.** `mcp_live_test.go` skips unless `HARBOR_LIVE_MCP=1`, the go-study-mcp binary exists (default `~/Repos/go-study-mcp/go-study-mcp`, override `HARBOR_GO_STUDY_MCP_BIN`), and `OPENROUTER_API_KEY` is set. The event fires even on an IsError TTS result (the binding is captured at discovery), so the probe is robust without a successful generation.

## Glossary additions

- No new terms. Updated existing entries (`mcp.app_available`, `MCP App discovery`, `MCP App`, `ui:// resource`) to reflect the tool-DEFINITION `_meta.ui` placement + D-216.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §6.4`, `RFC §6.5`, `RFC §7`, `brief 14`, `brief 11`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: the discovery event is scoped to the call's `(tenant, user, session)` triple (+ run); the bus rejects an empty triple — unchanged from 109d.
- [x] Concurrent-reuse test — N/A: the tool binding is captured immutably by value in the `Invoke` closure; no per-run `Provider` state is added. Marked N/A with this reason.
- [x] **Integration test exists** — `TestE2E_MCPAppAvailable_PlannerPathEmitsEvent` (corrected fixture, real MCP driver + real bus, identity + run propagation, the negative + merge branches, `-race`) plus the `HARBOR_LIVE_MCP` real-server probe.
- [x] If Protocol types changed: none — `make protocol-docs-gen-check` clean.
- [x] If new vocabulary: none new; existing glossary entries corrected.
- [x] If a brief finding was departed from: none.
