# Phase 109f — render heavy MCP App documents + an operator "pop to side-by-side" affordance

## Summary

Closes two gaps a live test against a real `io.modelcontextprotocol/ui` ext-apps server (go-study-mcp) surfaced after the 109a–e wave shipped. **Gap A:** a `ui://` App document at/above the heavy-content threshold (D-026) is offloaded to the ArtifactStore by reference, but the Console renderer treated the resulting `artifactRef` as a fatal "server bug" and refused to render — which hits nearly every real App, since Svelte/React bundles are routinely larger than 32 KiB (go-study-mcp's `studio/index.html` is 86.4 KB). The renderer now fetches the offloaded artifact's bytes and loads them into the same sandboxed `srcdoc` the inline path uses. **Gap B:** a host-side operator "expand ⤢" affordance on the inline app frame pops the app to the 109c side-by-side (`pip`) or `fullscreen` layout without the app having to ask, dispatched through the existing injected display-mode seam.

## RFC anchor

- RFC §6.4
- RFC §6.5
- RFC §7

## Briefs informing this phase

- brief 14
- brief 11

## Brief findings incorporated

- **brief 14 §"MCP Apps / `ui://` resources":** an App's UI document is a full HTML bundle, not a snippet — real App bundles carry inlined JS/CSS and routinely exceed any small inline threshold. This phase treats the heavy/offloaded path as the COMMON case, not an error.
- **brief 14 §"heavy content rides by reference":** the runtime must never inline heavy bytes through the context/LLM plane (D-026). This phase CONSUMES the by-reference form — the offload stays correct; only the renderer's content source changes (fetch the artifact at the iframe edge instead of inlining).
- **brief 11 §"the shared chat/playground module":** the chat module is dependency-injected and imports no Console internals (D-091). The new artifact-fetch capability is added to the INJECTED `MCPAppHostClient` interface; its real implementation lives in the Console-side adapter outside the chat module.
- **brief 11 §"Console is a Protocol client":** the renderer drives every host call through the injected Protocol surface — the artifact fetch resolves a presigned URL via `artifacts.get_ref` and fetches the opaque object-store bytes, exactly as the other MIME renderers fetch their presigned `src`.

## Findings I'm departing from (if any)

None.

## Goals

- A heavy (offloaded-by-reference) `ui://` App document renders correctly: the renderer fetches the artifact bytes and loads them into the sandboxed iframe `srcdoc`, identical to the inline path except for the content source.
- The sandbox token set, CSP (`buildAppCSP`), `wrapAppDocument` wrapper, and postMessage origin guard are UNCHANGED on both paths — only the document source differs.
- An operator can pop an inline app to side-by-side (`pip`) — and optionally `fullscreen` — from a host-side affordance on the frame, without the app initiating the request.
- The affordance dispatches through the EXISTING injected `onDisplayModeRequest` seam and reuses the 109c layout reducer — no parallel display-mode path; no chat-module reach into the page (D-091).

## Non-goals

- Changing the heavy-content offload itself (D-026 is correct — heavy bytes never inline through the context plane).
- New Runtime endpoints or Protocol methods. This is a Console-only change; `artifacts.get_ref` and `mcp.servers.read_resource` already ship.
- Caching fetched App documents, streaming partial documents, or any App-bundle optimisation — out of scope.
- A new layout region or teardown path — 109c already handles fullscreen/pip and return-to-inline teardown.

## Acceptance criteria

- [x] When `mcp.servers.read_resource` returns an `artifactRef` (heavy path), the renderer resolves it to a presigned URL via the injected client, fetches the bytes, and loads them into the iframe `srcdoc` — the "App failed to load … exceeds the inline heavy-content threshold" error is gone.
- [x] The inline (below-threshold) path still loads from `resource.content` — regression preserved.
- [x] The wrong "server bug" comment is corrected.
- [x] The fetch path raises a clear error on a non-2xx artifact fetch (fail-loud, no blank frame).
- [x] An "expand" affordance on the inline frame dispatches `onDisplayModeRequest({ requested: 'pip', granted: 'pip' })` (and optionally fullscreen), reusing the existing injected seam → 109c layout reducer moves the app to the pip region.
- [x] The affordance is hidden in the page-level fullscreen/pip panels and when the host advertises no non-inline mode.
- [x] The chat module imports ZERO Console internals (`$lib/protocol|connection|stores|components`).
- [x] `npm run check && npm run lint && npm test && npm run build` all green; `scripts/smoke/phase-109f.sh` OK ≥ acceptance count, FAIL = 0.

## Files added or changed

- `web/console/src/lib/chat/renderers/app-bridge-host.ts` — `MCPAppHostClient` gains `resolveArtifact(artifactID): Promise<string>`.
- `web/console/src/lib/chat/renderers/mcp-app.svelte` — Gap A heavy-fetch in `preload()`; Gap B operator expand affordance (overlay buttons + dispatch through `onDisplayModeRequest`).
- `web/console/src/lib/mcp-app-host-client.ts` — adapter implements `resolveArtifact` via `artifacts.get_ref` → `presigned_url`.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — §17.6 bug-twin fix: the `ChatProtocolClient.resolveArtifact` adapter read the absent `resp.url`; corrected to `presigned_url`.
- `web/console/src/routes/(console)/playground/[session_id]/mcp-app-discovery.spec.ts` — Gap A + Gap B always-on Vitest guards; fixtures gain `resolveArtifact`.
- `web/console/src/lib/mcp-app-host-client.spec.ts`, `web/console/src/lib/chat/renderers/app-bridge-host.spec.ts` — fixtures + adapter `resolveArtifact` test.
- `scripts/smoke/phase-109f.sh` — static-only smoke.

## Public API surface

- `MCPAppHostClient.resolveArtifact(artifactID: string): Promise<string>` — the injected chat-module seam returning a presigned URL for a heavy `ui://` document's by-reference artifact. Implemented by the Console adapter (`makeMCPAppHostClient`) over `artifacts.get_ref`.

## Test plan

- **Unit:** adapter `resolveArtifact` routes to `artifacts.get_ref` and returns `presigned_url` (not the absent `url`). AppBridge-host handler fixtures carry the new method.
- **Integration:** real-component Vitest guard — mount the real `MessageBubble` → `McpAppRenderer` with an injected client whose `readResource` returns an `artifactRef` (NOT inline content) + an artifact-fetch stub returning a realistic >32 KiB App HTML; assert the iframe `srcdoc` is populated from the fetched bytes and the error state is gone. The test FAILS if the artifactRef branch reverts to the error path (verified). An inline-path regression test stays. A Gap-B test asserts the expand button dispatches a `pip` request through the injected callback and the real layout reducer moves the app to pip.
- **Conformance:** N/A — no multi-driver subsystem changed.
- **Concurrency / leak:** N/A — no Go reusable artifact; the renderer is a per-mount Svelte component with no cross-run shared state.

## Smoke script additions

`scripts/smoke/phase-109f.sh` (static-only): the `resolveArtifact` seam on the interface + renderer + adapter (`presigned_url`); the fetched bytes feed `wrapAppDocument`; the "server bug" comment is gone; the expand affordance dispatches through `onDisplayModeRequest`; chat-module encapsulation holds; the Gap A + Gap B Vitest guards exist.

## Coverage target

- `web/console/src/lib/chat/renderers/` + `web/console/src/lib/mcp-app-host-client.ts`: the new branches (heavy fetch, expand affordance, `resolveArtifact`) are covered by the Vitest guards above. (The Console coverage gate runs over `src/lib/db/**`; these surfaces are guarded by the always-on component + adapter specs.)

## Dependencies

- 109a (the `mcp.servers.read_resource` heavy-content-aware method + the `MCPResourceArtifactRef` stub).
- 109b (the sandboxed iframe renderer + AppBridge + CSP).
- 109c (the fullscreen/pip DisplayMode layout reducer + teardown).
- 109d (the `mcp.app_available` discovery → renderer-mount wiring).
- 73l (the `artifacts.get_ref` presigned-URL resolver).

## Risks / open questions

- **Trust posture on the fetched bytes:** the fetched document is loaded into the SAME sandboxed `srcdoc` under the SAME CSP and origin guard as the inline path — the content source changes, the security envelope does not. No `allow-same-origin`; `appIframeSandbox` still throws if the forbidden token ever appears.
- **Presigned-URL expiry:** the fetch happens immediately after resolving the ref, well inside the bounded expiry window; a stale URL fails loud (non-2xx → error state), never a blank frame.
- A Playwright happy-path against a real built Playground route is optional and gated on the console subcommand; the Vitest component guards are the always-on regression gate.

## Glossary additions

None — "MCP App", "DisplayMode", "heavy-content threshold", and "by-reference artifact" already exist in `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, no identity-scoped storage path changed (the renderer drives the already-scoped injected client).
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A, Console-only; no Go reusable artifact built.
- [x] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — the real-component Vitest guard mounts the real renderer + injected client end-to-end and covers the failure mode (heavy fetch + revert guard).
- [x] If new vocabulary: glossary updated — N/A, no new terms.
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — N/A, no departures.
