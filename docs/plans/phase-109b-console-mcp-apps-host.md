# Phase 109b — console-mcp-apps-host

## Summary

Implement the Console-side MCP Apps host: the sandboxed-iframe renderer, the AppBridge wrapper, and the **inline** DisplayMode. A tool result projected by 109a carries `_meta.ui.resourceUri` pointing at a `ui://` resource; the Console fetches the HTML, renders it inside a sandboxed iframe under a strict CSP, and bridges app↔host traffic over the official `@modelcontextprotocol/ext-apps` AppBridge `postMessage` JSON-RPC dialect. **D-173 is the load-bearing decision: the Harbor Console is a Protocol CLIENT of the Runtime (CLAUDE.md §4.5) and MUST drive the AppBridge in MANUAL-HANDLER mode** — every app→host request (tool call, resource read, list) is wired to the injected Harbor `ProtocolClient`, never to a direct MCP transport, so identity, audit redaction, and the unified approval/OAuth gates all stay in force. This phase is the §13 same-wave consumer of 109a's primitives; it supersedes the deprecated Phase 85g (D-172).

## RFC anchor

<!-- Required. List the RFC sections this phase implements. Format exactly: RFC §6.X.
     The drift-audit script verifies every RFC §N.M reference resolves to a real heading. -->

- RFC §6.4
- RFC §7

## Briefs informing this phase

<!-- Required. List research briefs whose findings this phase depends on. Format: `brief NN`. -->

- brief 14
- brief 11
- brief 12

## Brief findings incorporated

- brief 14 §6: "MCP Apps … is **Console** work, not runtime-driver work — it touches `web/console`." — this phase is Console-side; the runtime/protocol work is 109a.
- brief 14 §6: "Render in a **sandboxed iframe** — no parent DOM / cookie / localStorage access; strict CSP; host-controlled permissions … the AppBridge `postMessage` JSON-RPC dialect (`ui/initialize`, tool-call proxying, model-context updates, host-pushed data)." — the security + protocol surface this phase delivers.
- brief 14 §6: the renderer must honour the `DisplayModes` / `RawHTMLTrust` projection and the `set_raw_html_trust` audit verb — the renderer maps `RawHTMLTrusted` to sandbox strictness and audits trust transitions.
- brief 11 (Console feature surface) + brief 12 (Console deployment + shared UI): per D-091 the MCP-Apps renderer registry lives at `web/console/src/lib/chat/renderers/`, imports no other Console internals, and the `ProtocolClient` is injected — never a Console-specific singleton.

## Findings I'm departing from (if any)

<!-- Required (can be "None"). -->

None.

## Goals

- **Add the ext-apps + MCP SDK dependencies** (`@modelcontextprotocol/ext-apps` + its peer `@modelcontextprotocol/sdk`) to `web/console/package.json` — npm only, lockfile committed, no other package manager — gated on the RFC §10 companion update (see Risks). We import the core + app-bridge entry points only, NOT the `/react` entry, so this is not a forbidden React/Vue dependency.
- Add `web/console/src/lib/chat/renderers/mcp-app.svelte` plus a thin `app-bridge-host.ts` wrapper — the MCP Apps renderer living in the shared chat module (`web/console/src/lib/chat/`) per D-091, importing NO other Console internals; the `ProtocolClient` is INJECTED.
- **Sandboxed iframe:** the `sandbox` attribute is set (NO `allow-same-origin` unless the projected `RawHTMLTrusted` trust state explicitly permits), a strict CSP applies, no parent-DOM / cookie / localStorage access is possible, and `postMessage` ORIGIN VALIDATION accepts messages only from the expected iframe — a missing origin check is a cross-frame injection vector.
- **Integrate the official AppBridge in MANUAL-HANDLER mode (D-173):** wire `oncalltool` → 109a's app-tool-call proxy, `onreadresource` → `mcp.servers.read_resource`, `onlistresources` / `onlisttools` → the existing list methods, and the `ui/initialize` handshake; honour `RawHTMLTrusted` → sandbox strictness; audit `set_raw_html_trust` transitions. The Console opens NO direct MCP transport — every handler routes through the injected `ProtocolClient` → Runtime → MCP southbound.
- **inline DisplayMode:** an app declared `inline` renders as a widget in the chat scroll via the renderer registry (`registerRenderer` in `web/console/src/lib/chat/renderers/index.ts`). Fullscreen + pip are 109c.
- Keep the generated typed Protocol client clean: `web/console/src/lib/protocol.ts` is GENERATED from `CanonicalWireTypes` (D-093), so any 109a wire-type change regenerates it and `make protocol-ts-gen-check` must stay clean. Hand-written client-method additions go in the namespace layer (`web/console/src/lib/protocol/client.ts`), never in the generated file.

## Non-goals

- The fullscreen tab + pip split layout (109c).
- The runtime/protocol work — the `ui://` projection, the app-tool-call proxy method, the `read_resource` surface (109a).
- Authoring MCP Apps — Harbor *hosts* apps; building them is a server-author concern.
- Persisting app state across sessions — an app instance is conversation-scoped.

## Acceptance criteria

<!-- Required. Bulleted, testable. These are binding. -->

- [ ] A tool result carrying `_meta.ui.resourceUri` (projected by 109a) triggers a `ui://` fetch via `mcp.servers.read_resource` and preloads the resource before render.
- [ ] The app renders inside an iframe with `sandbox` set (no `allow-same-origin` unless the trust state explicitly permits), a strict CSP, and no parent-DOM / cookie / `localStorage` access.
- [ ] The official AppBridge is driven in MANUAL-HANDLER mode; the Console opens NO direct MCP transport (asserted in tests).
- [ ] The `ui/initialize` handshake completes; malformed or foreign-origin `postMessage` messages are rejected, not executed.
- [ ] App-initiated tool calls are PROXIED through the host (109a's proxy method) and hit the same tool-safety policy (approval / OAuth / identity) as planner-initiated calls — an app call to a gated tool parks on the unified pause primitive.
- [ ] The renderer honours `RawHTMLTrusted` → sandbox strictness; `set_raw_html_trust` transitions are audited.
- [ ] inline DisplayMode renders the app as a chat-scroll widget via the renderer registry.
- [ ] The renderer lives at `web/console/src/lib/chat/renderers/` and imports no other Console internals (D-091 chat-module encapsulation rule); the `ProtocolClient` is injected.
- [ ] `svelte-check --fail-on-warnings` and the Console lint (no raw color/spacing literals, tokens only) pass; `make protocol-ts-gen-check` is clean.
- [ ] A Playwright test renders a fixture MCP App, exercises a proxied tool call through the host policy, asserts the iframe sandbox blocks parent-DOM / cookie / `localStorage` access, asserts the CSP, and asserts a foreign-origin message is rejected.

## Files added or changed

- `web/console/package.json` (+ `package-lock.json`) — add `@modelcontextprotocol/ext-apps` + `@modelcontextprotocol/sdk` (pinned versions; gated on the RFC §10 companion).
- `web/console/src/lib/chat/renderers/mcp-app.svelte` — the MCP Apps renderer (sandboxed iframe + inline mode).
- `web/console/src/lib/chat/renderers/app-bridge-host.ts` — thin wrapper over the official AppBridge in manual-handler mode; constructed with an injected `ProtocolClient`.
- `web/console/src/lib/chat/renderers/index.ts` — register the inline app renderer via `registerRenderer`.
- `web/console/src/lib/protocol/client.ts` — hand-written namespace methods calling 109a's `mcp.servers.read_resource` + the app-tool-call proxy. (NOT the generated `protocol.ts`.)
- `web/console/tests/` — Playwright suite: fixture app + sandbox-escape + proxied-tool-call + origin-rejection.
- `scripts/smoke/phase-109b.sh`.
- `docs/decisions.md` — D-173 reference (filed by the coordinator).
- `docs/plans/README.md` — Status flip on merge (by the coordinator).

## Public API surface

<!-- What other phases depend on. -->

Console-side TypeScript only — no new Go surface (that is 109a):

- The `mcp-app.svelte` renderer component contract (`RendererProps` — the tool result + injected `ProtocolClient` + DisplayMode).
- The `app-bridge-host.ts` wrapper API — constructed with an injected `ProtocolClient`, exposing the manual-handler registration that wires `oncalltool` / `onreadresource` / `onlistresources` / `onlisttools` / `onrequestdisplaymode` / `ui/initialize` to the client.
- The new `ProtocolClient` namespace methods in `web/console/src/lib/protocol/client.ts` (the `read_resource` + app-tool-call proxy wrappers).

## Test plan

<!-- Required. -->

- **Unit:** (TS) AppBridge wrapper handler wiring (each manual handler dispatches to the injected `ProtocolClient`, not a transport); `ui/initialize` handshake; malformed / foreign-origin message rejection; trust-state → sandbox-strictness mapping.
- **Integration:** a fixture MCP App + the AppBridge wrapper; a proxied tool call round-trips through an injected fake `ProtocolClient`, asserting the conversation identity is carried and the tool-safety gate is hit; assert the wrapper opens NO direct MCP transport (D-173 invariant).
- **Conformance:** N/A — Phase 85j covers wire-level Apps capability negotiation; this phase's Playwright suite covers rendering. Cross-ref Phase 85j.
- **Concurrency / leak:** N/A — Console-side rendering only; the runtime artifact is unchanged (the `ui://` projection + proxy live in 109a). No reusable Go artifact is built here.
- **Playwright (CI gate):** render a fixture App; exercise a proxied tool call through the host policy; assert the iframe sandbox blocks parent-DOM / cookie / `localStorage` access; assert the CSP; assert a foreign-origin `postMessage` is rejected.

## Smoke script additions

- `scripts/smoke/phase-109b.sh` (classification: `static-only`):
  - Assert `web/console/src/lib/chat/renderers/mcp-app.svelte` exists.
  - Assert the renderer file references an iframe `sandbox` attribute, a CSP, and an origin check.
  - Assert no raw color/spacing literals in the new `.svelte` file (token-surface rule, CLAUDE.md §4.5).

## Coverage target

- `web/console` (MCP Apps renderer + AppBridge wrapper): 80% (Console/tooling target per the master-plan coverage defaults).

## Dependencies

- 109a — the Protocol surface this phase consumes (the `ui://` projection, the app-tool-call proxy, the `read_resource` method). This phase is its §13 same-wave consumer.
- 73n — the Console Playground page / shared chat module.
- 108 — Playground polish + Console shell.

## Risks / open questions

- **RFC §10 dependency-addition prerequisite (binding).** This phase adds `@modelcontextprotocol/ext-apps` + `@modelcontextprotocol/sdk` to `web/console/package.json`. Per CLAUDE.md §13 ("Pulling in heavy frameworks … additions require RFC update") and §16 ("A phase plan introduces a dependency not in RFC §10 — that's an RFC PR first"), an RFC §10 companion update approving these dependencies MUST land before or with this phase. They are framework-agnostic TypeScript (core + app-bridge entry points, NOT the `/react` entry), so they are not forbidden React/Vue dependencies — but the RFC sign-off is still required.
- **D-173 manual-handler-mode is the architectural invariant.** The official AppBridge supports auto-forward mode (wrapping a live MCP Client) and manual-handler mode (host-registered handlers). Harbor MUST use manual-handler mode so every app→host request routes through the injected `ProtocolClient` → Runtime → MCP southbound, staying inside the `(tenant, user, session)` isolation boundary and the unified approval/OAuth/audit gates. Auto-forward would let the AppBridge open its own MCP transport and bypass identity, audit redaction, and the gates — a CLAUDE §6/§7/§13 violation. The "Console opens NO direct MCP transport" test guards this.
- **Sandbox escape is the whole game.** An accidental `allow-same-origin` + `allow-scripts` combination defeats isolation. The sandbox attribute set is a documented, reviewed constant; the Playwright negative tests are non-negotiable gates.
- **`postMessage` origin validation is mandatory** — only accept messages from the expected iframe; a missing origin check is a cross-frame injection vector.
- **The ext-apps extension is independently versioned** — `@modelcontextprotocol/ext-apps` may evolve faster than the core MCP spec; the phase pins the exact version it targets in `package.json`.

## Glossary additions

<!-- terms listed here; the coordinator adds the actual entries. -->

- **MCP App** — the Console-side renderer + sandboxed iframe + AppBridge host are this phase's deliverable. (Entry added by the coordinator.)
- **AppBridge** — the JSON-RPC dialect spoken over `postMessage` between an MCP App's sandboxed iframe and the Console host (`ui/initialize`, `tools/call`, `resources/read`, `resources/list`, `ui/request-display-mode`, `ui/notifications/*`). (Entry added by the coordinator.)
- **DisplayMode** — already exists (D-062); this phase consumes the `inline` mode.

## Pre-merge checklist

<!-- Tick when complete. -->

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation cross-session test passes — proxied tool calls run under the conversation identity; the isolation assertion is included.
- [ ] Concurrent-reuse test — N/A (Console-side rendering; the runtime artifact is unchanged — the `ui://` projection + proxy live in 109a). Marked N/A with this reason.
- [ ] Integration / Playwright test passes — fixture App rendered, sandbox isolation asserted, proxied tool call exercised, foreign-origin message rejected.
- [ ] `svelte-check --fail-on-warnings` + Console lint pass; `make protocol-ts-gen-check` clean.
- [ ] If new vocabulary: glossary updated (`MCP App`, `AppBridge`).
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed. (No departures.)
