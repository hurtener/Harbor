# Phase 85g — mcp-apps-host

## Summary

Implement the Console-side MCP Apps host: render the interactive HTML UIs MCP tools declare via the `io.modelcontextprotocol/ui` extension. A tool result references a `ui://` resource; the Console fetches it, renders it in a sandboxed iframe under a strict CSP, and bridges app↔host communication over the AppBridge `postMessage` JSON-RPC dialect. This phase also closes a standing primitive-without-consumer gap: `internal/tools/drivers/mcp/registry.go` already carries `DisplayModes` / `RawHTMLTrust` / `set_raw_html_trust` projection fields for a renderer that does not exist — Phase 85g is that renderer.

## RFC anchor

- RFC §6.4
- RFC §7

## Briefs informing this phase

- brief 14
- brief 11
- brief 12

## Brief findings incorporated

- brief 14 §6: "MCP Apps … is **Console** work, not runtime-driver work — it touches `web/console`." — this phase is Console-side.
- brief 14 §2 (#30) / §6: "`registry.go` carries `DisplayModes` / `RawHTMLTrust` projection fields for a Console renderer that does not exist — a §13 primitive-without-consumer smell. Phase 85g closes that gap." — explicit closure obligation.
- brief 14 §6: "Render in a **sandboxed iframe** — no parent DOM / cookie / localStorage access; strict CSP; host-controlled permissions … the AppBridge `postMessage` JSON-RPC dialect (`ui/initialize`, tool-call proxying, model-context updates, host-pushed data)." — the security + protocol surface.
- brief 11 (Console feature surface) + brief 12 (Console deployment + shared UI): the MCP-Apps renderer registry lives at `web/console/src/lib/chat/renderers/` per D-091's shared-chat-module rule.

## Findings I'm departing from (if any)

- None. This phase consumes the `registry.go` Apps projection fields as designed; it does not redesign them.

## Goals

- A tool result carrying `_meta.ui.resourceUri` is recognised; the Console fetches the `ui://` resource and preloads it.
- The UI renders in a **sandboxed iframe**: `sandbox` attribute set, no parent DOM / cookie / localStorage / same-origin access, a strict CSP, host-controlled permission grants.
- The AppBridge `postMessage` JSON-RPC dialect is implemented: `ui/initialize`, tool-call proxying (the app asks the host to call an MCP tool), model-context updates, host-pushed data.
- `registry.go`'s `DisplayModes` / `RawHTMLTrusted` projection + the `set_raw_html_trust` audit verb get a real consumer — the renderer honours the trust state when deciding sandbox strictness.
- The renderer lives in the shared chat module (`web/console/src/lib/chat/renderers/`) per D-091, so the future packed dev UI reuses it.

## Non-goals

- The runtime-side MCP driver — Apps is purely Console-side; `internal/tools/drivers/mcp` is unchanged except where it already projects the Apps metadata.
- Authoring MCP Apps — Harbor *hosts* apps; building them is a server-author concern.
- Persisting app state across sessions — an app instance is conversation-scoped.

## Acceptance criteria

- [ ] A tool result with `_meta.ui.resourceUri` triggers a `ui://` resource fetch + preload before the result renders.
- [ ] The UI renders inside an iframe with `sandbox` set (no `allow-same-origin` unless the trust state explicitly permits), a strict CSP, and no access to parent DOM / cookies / `localStorage`.
- [ ] The AppBridge implements `ui/initialize` and the documented `ui/*` methods over `postMessage`; messages are validated as JSON-RPC; malformed messages are rejected, not executed.
- [ ] App-initiated tool calls are *proxied* through the host — the app cannot call MCP tools directly; the host enforces the same tool-safety policy (consent, identity) it enforces for planner-initiated calls.
- [ ] The renderer honours `registry.go`'s `DisplayModes` / `RawHTMLTrusted` projection — an untrusted app gets the strictest sandbox; `set_raw_html_trust` transitions are audited.
- [ ] The renderer lives at `web/console/src/lib/chat/renderers/` and imports no other Console internals (D-091 chat-module encapsulation rule).
- [ ] `svelte-check --fail-on-warnings` and the Console lint (no raw color/spacing literals) pass.
- [ ] A Playwright test renders a fixture MCP App, exercises a proxied tool call, and asserts the iframe sandbox blocks parent-DOM access.

## Files added or changed

- `web/console/src/lib/chat/renderers/mcp-app.svelte` (+ supporting `.ts`) — the MCP Apps renderer.
- `web/console/src/lib/chat/` — AppBridge `postMessage` JSON-RPC client.
- `internal/protocol/` — a method/event surface for `ui://` resource fetch + app↔host data push, if not already covered by `resources/read` projection (finalised against the Console wave).
- `web/console` Playwright suite — an MCP App fixture + sandbox-escape assertion.
- `internal/tools/drivers/mcp/registry.go` — no behaviour change; this phase is its consumer (a comment may be added noting the consumer now exists).
- `scripts/smoke/phase-85g.sh`.
- `docs/decisions.md` — decision entry (filed at implementation time) on the sandbox/CSP posture + the trust-state→sandbox-strictness mapping.
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

Console-side TypeScript (the AppBridge client + renderer component). Any new Protocol method for `ui://` fetch lands in `internal/protocol/methods` + `internal/protocol/types` per the single-source rule. No new runtime-driver Go surface.

## Test plan

- **Unit (TS):** AppBridge JSON-RPC encode/decode; malformed-message rejection; `ui/initialize` handshake.
- **Integration:** a fixture MCP App + the AppBridge; proxied tool call round-trips through the host's tool-safety policy.
- **Conformance:** N/A — Phase 85j (the band conformance harness) covers the wire-level Apps capability negotiation; this phase's Playwright suite covers rendering.
- **Playwright (CI gate):** render a fixture App; assert sandbox isolation (parent-DOM access blocked, no cookie access); exercise a proxied tool call; assert CSP headers.
- **Security:** an App attempting `window.parent` access, cookie read, or `localStorage` access is blocked by the sandbox — explicit negative tests.

## Smoke script additions

- `scripts/smoke/phase-85g.sh` (classification: `static-only`):
  - Assert `web/console/src/lib/chat/renderers/mcp-app.svelte` exists.
  - Assert the renderer file references an iframe `sandbox` attribute and a CSP.
  - Assert no raw color/spacing literals in the new `.svelte` file (token-surface rule, AGENTS.md §4.5).

## Coverage target

- `web/console` (MCP Apps renderer + AppBridge): 80% (Console/tooling target per the master-plan coverage defaults).

## Dependencies

- 28 (MCP driver — supplies the Apps metadata projection in `registry.go`).
- 85a (the foundation — the driver's discovery surface must be sound).

## Risks / open questions

- **Sandbox escape is the whole game.** An iframe misconfiguration (an accidental `allow-same-origin` + `allow-scripts` combination) defeats the isolation. The sandbox attribute set is a documented, reviewed constant; the Playwright negative tests are non-negotiable gates.
- **AppBridge is a new JSON-RPC dialect over `postMessage`** — not stdio, not HTTP. Message-origin validation (only accept messages from the expected iframe) is mandatory; a missing origin check is a cross-frame injection vector.
- **Console wave coupling.** This phase is large and Console-shaped; if it proves too big once authored per §16, it decomposes (renderer / AppBridge / preloading as separate phases) — the band carries it as one row pending that authoring.
- **Apps spec is an extension, independently versioned** — the `io.modelcontextprotocol/ui` extension may evolve faster than the core spec; the phase pins the extension version it targets.

## Glossary additions

- **MCP App** — see brief 14 §9. The Console-side renderer + sandboxed iframe + AppBridge are this phase's deliverable.
- **AppBridge** — the JSON-RPC dialect spoken over `postMessage` between an MCP App's sandboxed iframe and the Console host (`ui/initialize`, tool-call proxying, model-context updates).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] Cross-isolation test — proxied tool calls run under the conversation's identity; isolation assertion included.
- [ ] Concurrent-reuse test — N/A (Console-side rendering; the runtime driver is unchanged). Marked N/A with this reason.
- [ ] **Integration / Playwright test passes** — fixture App rendered, sandbox isolation asserted, proxied tool call exercised.
- [ ] `svelte-check --fail-on-warnings` + Console lint pass.
- [ ] Glossary updated.
- [ ] No brief departures.
