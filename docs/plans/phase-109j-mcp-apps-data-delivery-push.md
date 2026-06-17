# Phase 109j — mcp-apps-data-delivery-push

## Summary

This phase closes the MCP Apps "Data Delivery" lifecycle on the Console side. After the sandboxed app sends `ui/notifications/initialized`, the host must push the originating tool's INPUT args and RESULT into the app via the official AppBridge `sendToolInput()` / `sendToolResult()` — today the Harbor host never calls either, so a spec-conformant app that renders from host-pushed data boots empty. Consuming Phase 109i's `mcp.apps.tool_context` surface, the renderer fetches the captured `{tool, input, result}` for the app's `toolCallID` and pushes it across the bridge once the app has initialized.

## RFC anchor

- RFC §6.4
- RFC §7

## Briefs informing this phase

- brief 14

## Brief findings incorporated

- brief 14 §6: the AppBridge dialect includes *"host-pushed data"* — this phase implements the host→app push the dialect specifies, using the official `@modelcontextprotocol/ext-apps` `sendToolInput` / `sendToolResult` (no hand-rolled message), matching the manual-handler integration (D-173).
- brief 14 §6: Apps *"Render in a sandboxed iframe — no parent DOM / cookie / localStorage access; strict CSP."* The pushed data flows over the EXISTING postMessage bridge to the existing sandboxed iframe; this phase adds no new iframe capability, no new network reach, and changes no CSP/sandbox token.
- brief 14 §2 (rows 18–19): the lowered tool result (`StructuredContent` preserved) is what the app receives — the push surfaces the same structured result the runtime captured in 109i, not a re-serialised summary.
- brief 14 §5 (context binding → identity triple): the fetch goes through the injected Protocol client (`mcp.apps.tool_context`), so the delivered data is the caller's identity-scoped record — an app can never receive another identity's tool context.

## Findings I'm departing from (if any)

None.

## Goals

- After the app sends `ui/notifications/initialized`, the host fetches `mcp.apps.tool_context(serverID, toolCallID)` through the injected `MCPAppHostClient` and pushes it: `bridge.sendToolInput({ arguments })` then `bridge.sendToolResult({ content, isError })`, in that order (the SDK requires `initialized` before `sendToolResult`).
- The push is heavy-aware: a `tool_context` response that returns an `artifactRef` (heavy result) is resolved to a presigned URL and the bytes fetched at the host edge (mirroring the 109f heavy `read_resource` path), then pushed — OR, when not resolvable (non-S3), the host pushes a faithful by-reference stub rather than silently delivering empty data.
- The `toolCallID` flows from the decoded `mcp.app_available` event → the ChatMessage app ref → the renderer props, so the renderer knows which tool context to fetch.
- An app with no captured tool context (unknown/evicted id → `CodeNotFound`) boots without a push and does not error — the app simply receives no `tool-result` (degraded, never a thrown render error).

## Non-goals

- No backend change — the capture + `mcp.apps.tool_context` surface ships in 109i.
- No streaming/partial tool-input (`sendToolInputPartial`) — only the final input + result are pushed at V1.
- No dynamic re-push on a later tool call within the same app session — one push per app mount (the app re-fetches via its own `tools/call` for subsequent interaction, already wired).
- No change to the sandbox tokens, CSP, or the postMessage origin guard.

## Acceptance criteria

- [ ] After `oninitialized`, `AppBridgeHost` calls `sendToolInput` then `sendToolResult` exactly once, with the data fetched from `mcp.apps.tool_context`; a vitest with a fake bridge + fake `MCPAppHostClient` asserts the order and the payloads.
- [ ] `toolCallID` is decoded from the `mcp.app_available` frame (`wire-events.ts`), carried on `MCPAppRefView` + the ChatMessage app ref, and passed to the renderer; a test asserts it reaches the host.
- [ ] A heavy `tool_context` result (`artifactRef`) is resolved + fetched and pushed; a test with a stubbed `resolveArtifact` asserts the bytes are delivered (not the stub) when resolvable.
- [ ] A non-resolvable heavy result (resolve throws / non-S3) pushes a faithful by-reference stub and surfaces the limitation — it does NOT push empty content silently (fail-loud posture).
- [ ] An app whose `tool_context` returns `CodeNotFound` mounts and initializes with NO push and NO thrown error (the `App failed to load` / error state is not entered for a missing-context case).
- [ ] The push routes ONLY through the injected `MCPAppHostClient` — the no-direct-transport test (network spies installed) still asserts zero direct network calls from the bridge handlers; the new push uses the injected client only.
- [ ] `svelte-check --fail-on-warnings` and `npm run lint` pass (no Svelte 4 syntax, no raw literals, no hand-rolled `fetch` in `.svelte`).

## Files added or changed

- `web/console/src/lib/chat/renderers/app-bridge-host.ts` — a post-`initialized` `deliverToolContext()` that fetches via the injected client and calls `sendToolInput` / `sendToolResult`; `MCPAppRefView` gains `toolCallId`; `MCPAppHostClient` gains `toolContext(serverID, toolCallID)`.
- `web/console/src/lib/chat/renderers/mcp-app.svelte` — pass `toolCallId` through; trigger delivery after the bridge reports initialized.
- `web/console/src/lib/mcp-app-host-client.ts` — adapt `client.mcp.apps.toolContext` onto the injected `MCPAppHostClient.toolContext`.
- `web/console/src/lib/protocol/mcp.ts` — the `mcp.apps.tool_context` typed client call + response type (hand-mirrored from 109i's Go wire types).
- `web/console/src/routes/(console)/playground/[session_id]/wire-events.ts` — decode `tool_call_id` off the `mcp.app_available` frame.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — carry `toolCallId` onto the message app ref in `applyAppAvailable`.
- `web/console/src/lib/chat/MessageBubble.svelte` — forward `toolCallId` to the renderer.
- `web/console/src/lib/chat/renderers/app-bridge-host.spec.ts` — push-order + heavy + not-found tests.
- `docs/plans/phase-109j-mcp-apps-data-delivery-push.md`, `scripts/smoke/phase-109j.sh`, `docs/plans/README.md`, `docs/decisions.md` (D-226).

## Public API surface

- `MCPAppHostClient` gains `toolContext(serverID, toolCallID): Promise<MCPAppToolContext>` (the injected seam — chat module stays free of `$lib/protocol`, D-091).
- `MCPAppRefView` gains `toolCallId`.
- No Go / Protocol change.

## Test plan

- **Unit (vitest):** push order (`sendToolInput` before `sendToolResult`, after `initialized`); payload shape; heavy-result resolve+fetch+push; non-resolvable heavy → stub + surfaced limitation; `CodeNotFound` → no push, no error; the no-direct-transport spy test extended to cover the delivery path.
- **Integration:** the Playground page wiring test asserts `toolCallId` flows event → message → renderer → host; a Playwright app-render spec (the existing sandbox harness) asserts an app receives a `tool-result` it renders.
- **Conformance:** N/A — Console.
- **Concurrency / leak:** N/A — Console; the bridge lifecycle teardown (`host.close()`) test confirms no listener leak across mount/unmount, retained.

## Smoke script additions

- `scripts/smoke/phase-109j.sh` — `static-only`: assert `app-bridge-host.ts` references `sendToolInput` + `sendToolResult` (the Data Delivery push exists), assert `wire-events.ts` decodes `tool_call_id`, and assert `mcp.ts` exposes the `tool_context` client call. (The live render path is covered by the Playwright spec, not preflight.)

## Coverage target

- `web/console` chat-module: maintain or improve the `app-bridge-host` test coverage; the new delivery branch fully covered by the vitest cases.

## Dependencies

- Phase 109i (the `mcp.apps.tool_context` surface + `toolCallID` on the event). Phase 109b (the AppBridge host + sandboxed iframe this extends). Same wave as 109i (D-062 — no Console consumer without its feeding Protocol surface in the same wave).

## Risks / open questions

- **Push timing.** `sendToolResult` requires `ui/notifications/initialized` first (SDK contract). The delivery is gated on the existing `oninitialized` callback; an app that never initializes never receives a push (correct — it isn't ready). A test asserts no push before initialized.
- **Heavy result on non-S3 stores.** Inherited from 109i — a heavy captured result is delivered by-reference stub when the presign is unsupported, not silently empty. Most tool results are small.
- **One-shot delivery.** Only the originating result is pushed; ongoing interactivity uses the app's own `tools/call` (already wired). Streaming/partial input is a documented post-V1 extension.

## Glossary additions

- None new — consumes "MCP Apps Data Delivery" / "MCP Apps tool context" introduced in Phase 109i.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: N/A — the delivered data is the caller's identity-scoped 109i record; the Console adds no new scope.
- [ ] **Reusable artifact:** N/A — Console renderer; the bridge teardown/no-leak test is retained.
- [ ] **Consumes a shipped subsystem's surface:** consumes 109i's `mcp.apps.tool_context`; the Playground wiring + Playwright render specs exercise it end-to-end. The no-direct-transport spy test asserts the push uses only the injected client.
- [ ] If Protocol types changed: `mcp.ts` mirrors 109i's wire types; `svelte-check --fail-on-warnings` passes.
- [ ] If new vocabulary: N/A — reuses 109i terms.
- [ ] If a brief finding was departed from: N/A — none departed.
