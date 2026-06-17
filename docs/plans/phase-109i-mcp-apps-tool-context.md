# Phase 109i — mcp-apps-tool-context

## Summary

The BACKEND half of the MCP Apps "Data Delivery" lifecycle. The 109 wave lets the Console discover (`mcp.app_available`), fetch (`mcp.servers.read_resource`), and render a `ui://` MCP App in a sandboxed iframe — but a rendered app has no way to read the tool context (the input + the lowered result) that produced it. This phase captures that context at the tool-invocation site and exposes a new identity-scoped Protocol read method, `mcp.apps.tool_context`, so a rendered app can fetch its own data. Capture rides the EXISTING StateStore (all three persistence drivers + identity isolation come free; no new driver, no new migration), keyed by the caller's identity triple under `kind = "mcp.apps.tool_context/<serverID>/<toolCallID>"`; the input and result are heavy-content-aware at WRITE (a payload at or above the heavy threshold offloads to the ArtifactStore by reference through the same loud-bypass path the resource read uses). The `mcp.app_available` event and the app-tool-call proxy projection both gain the stable `tool_call_id` so the client can correlate a discovered app to its captured context. The §13 same-wave consumer is the read path itself: a planner-path MCP app result now captures its context, and the new method reads it back projected inline or by reference. The Console consumption (the rendered app calling `mcp.apps.tool_context`) lands in 109j.

## RFC anchor

- RFC §6.4
- RFC §6.5
- RFC §7

## Briefs informing this phase

- brief 14
- brief 11

## Brief findings incorporated

- brief 14 §5 (security context-binding): an MCP App is bound to the `(tenant, user, session)` security context it was created under; a cross-context read MUST be rejected. The captured tool context is a `StateRecord` scoped to the caller's identity triple, and `StateStore.Load` filters by that triple — so a read under a different identity is not found by construction (existence is never revealed across identities).
- brief 14 §6 (host-pushed data): the host delivers the tool's structured data to the rendered app; the app does not re-call the tool. This phase captures the lowered result at the invocation site and serves it by reference to the app — the app reads its data, it never re-invokes (which would double a side effect).
- brief 14 §2 rows 18–19 (tool-result lowering preserved): the captured result is the SAME lowered `MCPToolValue` the planner sees (and the app-tool-call proxy projects), so the app's view of the result is consistent with the runtime's — not a second, divergent lowering.
- brief 11 (Console is a pure Protocol client): the rendered app reads its tool context only through a typed Protocol method (`mcp.apps.tool_context`); it never reaches a Runtime internal. Heavy halves ride by reference (an `artifact_ref` the Console resolves through the artifacts surface), exactly as `read_resource` and the proxy already do.

## Findings I'm departing from (if any)

None.

## Goals

- Capture the tool context (input arguments + lowered result + `is_error`) behind a declared `ui://` app at the MCP provider's invocation site, keyed by the caller's identity triple + a stable per-invocation `tool_call_id`, persisted through the EXISTING `StateStore`.
- Mint the `tool_call_id` with NO mutable `Provider` state (D-025): a deterministic content hash of `run | server | tool | args`, collision-free across distinct calls.
- Add `tool_call_id` + `tool_name` to the `mcp.app_available` discovery event (ids/names are safe — they are not content; the payload stays `SafeSealed`), and `tool_call_id` to the wire `MCPAppRef` (+ the runtime projection + the proxy response) so the client correlates an app to its captured context.
- Add a new Protocol method `mcp.apps.tool_context` (`ToolContextRequest` → `ToolContextResponse`) routed through the AppsSurface dispatcher; identity-mandatory; an unknown or cross-identity id fails with `CodeNotFound`.
- Make capture heavy-content aware at WRITE (a payload ≥ the heavy threshold offloads to the ArtifactStore by reference, the loud `mcp.resource_offloaded` bypass) and the read project each half inline OR as an `artifact_ref` — the same discipline as `read_resource`.
- Fail loud, never silently: a capture error is logged + observable but does not fail the tool call (the planner still gets its result); a missing identity fails closed.

## Non-goals

- The Console consumption — the rendered app calling `mcp.apps.tool_context` and surfacing the data in the iframe (Phase 109j).
- A new persistence driver or migration — the capture rides the shipped `StateStore` (D-026 heavy content offloads to the shipped `ArtifactStore`).
- Re-invoking the tool to deliver data — the captured lowered result is the source; the app never re-calls.
- The `mcp.servers.read_resource` document fetch and the `mcp.apps.call_tool` proxy (109a/109g — consumed, not redefined).

## Acceptance criteria

- [ ] A planner-path MCP tool result that declares a `ui://` app captures `{tool, input, lowered result, is_error}` through the `StateStore`, keyed by the identity triple + `tool_call_id`; a deterministic, collision-free `tool_call_id` is minted with no mutable Provider field. — `Provider.captureToolContext` + `ToolCallID`; `TestProvider_CaptureToolContext_PlannerPath`, `TestToolCallID_DeterministicAndCollisionFree`.
- [ ] `mcp.apps.tool_context` reads the captured context back, projecting each half inline OR as an `artifact_ref` (heavy result offloaded at capture). — `AppsSurface.handleToolContext` + `ToolContextStore.Capture/Load`; `TestToolContextStore_CaptureLoadInline`, `TestToolContextStore_CaptureLoadHeavyOffload`, `TestAppsSurface_ToolContext_*Projection`.
- [ ] An unknown or cross-identity (server_id, tool_call_id) fails with `CodeNotFound`; a ≥2-identity isolation test proves a context captured under A is not loadable under B. — `TestToolContextStore_UnknownIDNotFound`, `TestToolContextStore_CrossIdentityNotFound`, `TestAppsSurface_ToolContext_UnknownIDMapsToNotFound`.
- [ ] A capture error is logged + does NOT fail the tool call (the planner still gets its result); a missing identity fails closed. — `TestProvider_CaptureToolContext_ErrorLoggedNotSwallowed`, `TestToolContextStore_MissingIdentityFailsClosed`.
- [ ] The `mcp.app_available` event carries `tool_call_id` + `tool_name`, the wire `MCPAppRef` carries `tool_call_id`, single-sourced in `internal/protocol/types` and hand-synced into the Console wire types. — `events.AppAvailablePayload`, `types.MCPAppRef.ToolCallID`, `protocol/mcp.ts`; generated docs + wire manifest regenerated.
- [ ] The `AppsAccessor` / `ToolContextStore` stay immutable-after-construction; the concurrent-reuse tests (N≥128) pass under `-race`. — `TestAppsAccessor_ToolContext_ConcurrentReuse`, `TestProvider_ToolContextCapture_ConcurrentReuse`.
- [ ] A `HARBOR_LIVE_MCP`-gated real-server probe drives a real ext-apps server through capture → tool-context read. — `TestLive_ToolContext_RealServerRoundTrip`.

## Files added or changed

```text
internal/tools/drivers/mcp/
  toolcontext.go          # ToolContextCapturer seam + CapturedToolContext + ToolCallID hash
  content.go              # AppRef.ToolCallID
  events.go               # AppAvailablePayload.ToolCallID + .ToolName
  mcp.go                  # Config.ToolContext; callTool mints id + captures; publishAppAvailable
  attach.go               # AttachDeps.ToolContext threaded into Config
  toolcontext_test.go     # id determinism, capture wiring, error-not-swallowed, concurrent reuse
internal/mcpconsole/
  toolcontext.go          # ToolContextStore (Capture + Load) over StateStore + ArtifactStore
  apps.go                 # offloadHeavy shared helper; AppsAccessor.ToolContext; ToolCallID projection
  toolcontext_test.go     # capture/load inline+heavy, unknown→NotFound, cross-identity isolation, concurrent reuse
  toolcontext_live_test.go # HARBOR_LIVE_MCP real-server round-trip probe
internal/protocol/
  methods/methods.go      # MethodMCPAppsToolContext in canonicalMethods + canonicalMCPAppsMethods
  types/mcp_apps.go       # MCPAppRef.ToolCallID + ToolContextRequest/Payload/Response
  apps.go                 # AppToolContextReader seam + rows + handleToolContext + dispatch case
  mcp.go                  # isMCPNotFound also matches the tool-context not-found marker
  transports/control/apps_handler.go  # decode + identity-scope the new request
  singlesource/singlesource.go        # new method + wire types
internal/runtime/assemble/assemble.go # construct ToolContextStore, wire into providers, expose on Stack
harbortest/devstack/devstack.go       # mirror the wiring (Stack field + AppsDeps)
cmd/harbor/cmd_dev.go                 # AppsAccessor + AppsSurface get the tool-context seam
cmd/harbor-gen-protocol-docs/         # methodTable + typeInstanceIndex rows; regenerated docs
cmd/harbor-protocol-ts-lockstep/      # typeInstanceIndex rows; regenerated wire manifest
web/console/src/lib/protocol/mcp.ts   # tool_call_id + ToolContext{Request,Payload,Response}
scripts/smoke/phase-109i.sh
docs/plans/README.md, docs/decisions.md (D-225), docs/glossary.md
```

## Public API surface

Single-source rule honoured: the only new wire surface is the new method `mcp.apps.tool_context`, the new wire types `ToolContextRequest` / `ToolContextPayload` / `ToolContextResponse` (all in `internal/protocol/types`), and the new `tool_call_id` field on the existing `MCPAppRef`. The `mcp.app_available` event payload gains `tool_call_id` + `tool_name` (a `SafeSealed` payload — ids/names, never content). No error code or event type is added. Go-flavoured:

- **New method** `mcp.apps.tool_context` — `types.ToolContextRequest{Identity, ServerID, ToolCallID}` → `types.ToolContextResponse{Tool, Input, Result, IsError}`, each of Input/Result `{Content json.RawMessage | ArtifactRef}`. Routed through `AppsSurface` (`IsMCPAppsMethod`).
- **New seam** `protocol.AppToolContextReader` — implemented by `mcpconsole.AppsAccessor` (delegating to `ToolContextStore`).
- **New driver seam** `mcp.ToolContextCapturer` — implemented by `mcpconsole.ToolContextStore`, wired into every Provider's `Config`.

## Test plan

- **Unit:** `ToolCallID` determinism + collision-freedom (incl. field-boundary aliasing); `ToolContextStore` capture/load inline + heavy-offload; unknown id → not-found marker; missing identity fails closed; the `AppsSurface.handleToolContext` inline + by-reference projection + invalid-request branches.
- **Integration:** the in-package `mcpconsole/toolcontext_test.go` IS the seam (ToolContextStore ↔ real inmem StateStore + ArtifactStore + EventBus), identity-scoped, with the heavy-offload event surface; the `mcp/toolcontext_test.go` drives the real MCP Provider's invoke path (real in-memory MCP server) capturing through a recording capturer; the `HARBOR_LIVE_MCP` probe drives a real ext-apps server end-to-end.
- **Cross-identity isolation:** `TestToolContextStore_CrossIdentityNotFound` (≥2 identities) + `TestAppsSurface_ToolContext_UnknownIDMapsToNotFound`.
- **Concurrency / leak:** `TestAppsAccessor_ToolContext_ConcurrentReuse` + `TestProvider_ToolContextCapture_ConcurrentReuse` (N=128) under `-race`, asserting no id collisions, no context bleed, no goroutine leak.

## Smoke script additions

- `scripts/smoke/phase-109i.sh` (classification: live-server): asserts `mcp.apps.tool_context` is wired (a body missing `tool_call_id` → `invalid_request`, never 404), an unknown id → `not_found`, and the static capture/read seams exist in the driver + AppsAccessor. SKIPs on 404/405/501.

## Coverage target

- `internal/mcpconsole`: maintain or improve the package baseline (the new `ToolContextStore` capture/load + the concurrent-reuse path are fully covered).
- `internal/tools/drivers/mcp`: 85% (the capture seam + the id hash are covered by the new tests + the existing discovery tests).
- `internal/protocol`: 80% (the new handler + projection are covered by the extended apps tests).

## Dependencies

- 109a (the AppsSurface + `mcp.servers.read_resource` heavy-offload path the capture reuses).
- 109d (the `mcp.app_available` event + `MCPAppRef` the `tool_call_id` rides on).
- 109g (the heavy-aware inline/offload pattern reused at the capture seam).
- 28 / 85a (the MCP driver's invocation surface the capture site sits on).

## Risks / open questions

- **§13 primitive-with-consumer.** The new `mcp.apps.tool_context` method is a primitive; its consumer is the read path itself, exercised end-to-end by the Go integration tests (capture at the Provider → read through the AppsSurface). The Console UI consumption lands in 109j.
- **Session-scoped key (empty RunID).** The captured `StateRecord` is keyed by the triple with an EMPTY `RunID`, because the read (from a rendered app) knows the session but not necessarily the producing run. The `tool_call_id` disambiguates calls within the session via the Kind. A re-invocation of the same tool with the same args inside the same run re-derives the same id and overwrites its own slot (idempotent) — acceptable, since the latest context is the one the app wants.
- **Capture is best-effort relative to the tool call.** A capture failure (store error, encode error, missing identity) is logged loudly and the tool call still returns its result. This is intentional (the tool result is the planner's source of truth); the app's later tool-context read then returns not-found, which the Console handles as "no context available."

## Glossary additions

- **MCP Apps tool context** — the input arguments + lowered result captured behind a declared `ui://` MCP App at the tool-invocation site, readable by the rendered app via `mcp.apps.tool_context` (keyed by server source id + `tool_call_id`, identity-scoped). (Coordinator adds the canonical entry.)
- **MCP Apps Data Delivery** — the MCP Apps lifecycle stage where the host delivers a tool call's structured data to the rendered app (capture at invocation + the `mcp.apps.tool_context` read), so the app renders its own data without re-invoking the tool. (Coordinator adds the canonical entry.)

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §6.4`, `RFC §6.5`, `RFC §7`, `brief 14`, `brief 11`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Multi-isolation: the captured context is scoped to the `(tenant, user, session)` triple; a cross-identity read is not found (≥2-identity test).
- [x] **Reusable artifact:** `ToolContextStore` + `AppsAccessor` concurrent-reuse tests pass — N=128 under `-race`.
- [x] **Consumes a shipped subsystem's surface:** the StateStore + ArtifactStore + EventBus seams are wired with real inmem drivers, identity propagation asserted, the heavy-offload failure surface covered, `-race`.
- [x] If Protocol types changed: new method + wire types single-sourced, hand-synced into `protocol/mcp.ts`, generated docs + wire manifest regenerated + committed.
- [x] If new vocabulary: glossary updated (MCP Apps tool context, MCP Apps Data Delivery).
- [x] If a brief finding was departed from: none.
