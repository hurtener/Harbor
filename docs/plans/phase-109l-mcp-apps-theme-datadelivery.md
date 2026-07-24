# Phase 109l — MCP Apps host theme + data delivery (re-land, handshake-safe)

## Summary

Re-land the reverted Console halves of the MCP Apps host contract: the host emits the live theme (`color-scheme`) + structural design tokens (`styles.variables`) into the `ui/initialize` host-context and patches them via `host-context-changed`, AND delivers the originating tool call's input + result into the rendered app after `initialized`. Built handshake-safe (bridge constructed once with the final host-context; lifecycle `$effect` isolated from theme reactivity; every host→app send gated behind `oninitialized`; no teardown-rebuild for a theme/data change), so it does NOT reintroduce the `ui/initialize` timeout that got the original work reverted. Gated by a real-iframe end-to-end Playwright handshake test AND a binding live-render check against a real ext-apps App under a real LLM agent.

## RFC anchor

- RFC §7.3

## Briefs informing this phase

- brief 14

## Brief findings incorporated

- brief 14 §6 (host-pushed Data Delivery dialect): the lifecycle is host→app push AFTER `ui/notifications/initialized` — the app reads the delivered input/result and does NOT re-invoke the tool (re-invoking would double a side effect); input-then-result order.
- brief 14 §2–3 (extension negotiation / host obligations): theme + `styles.variables` + `host-context-changed` are host obligations of the `io.modelcontextprotocol/ui` ext-apps dialect the host already negotiates; the app-side SDK consumes them with zero per-component work, so the host producing them is the whole job.
- brief 14 (spec-fixture discipline / D-216): a self-consistent hand fixture stays green while the real handshake is inert — the gate must drive the REAL vendored App client, not a hand-rolled postMessage stub.

## Findings I'm departing from (if any)

None. This is a faithful re-land of the reverted D-226/D-227 Console halves, authorized by D-342, with the handshake root cause fixed.

## Goals

- A rendered `ui://` app adapts to the host's light/dark mode + native tokens (theme + `styles.variables` in `ui/initialize`; patched on host theme change via `host-context-changed`).
- The app receives the originating tool call's input + result after `initialized` (renders real content, not an empty shell).
- The `ui/initialize` handshake stays green — no teardown-rebuild mid-handshake; theme changes mutate the live bridge, never rebuild it.
- The real Dockyard `analytics-widgets` app renders correctly under a real LLM agent (the binding done-gate).

## Non-goals

- Progressive `tool-input-partial` / streaming render (deferred — D-343; needs a runtime `llm.toolcall.partial` companion).
- Any Go / Protocol / wire change — the ext-apps dialect is vendored, outside `CanonicalWireTypes`; the D-225/D-227 backend halves are already on main.
- A Console-wide applied light/dark theme system — theme source is OS `prefers-color-scheme` (D-227 precedent); a full Console theme store is out of scope.

## Acceptance criteria

- [ ] `AppBridgeHost` is constructed ONCE with the final `McpUiHostContext` including `styles.variables` (typed against the vendored `McpUiHostStyles`); theme sourced from OS `prefers-color-scheme`, Console `tokens.css` names mapped to the ext-apps `McpUiStyleVariableKey` namespace.
- [ ] A host theme change calls `setHostContext` on the LIVE bridge (→ `host-context-changed`) from a SEPARATE effect that no-ops until initialized; the bridge-owning `$effect` depends only on `loadState`+`iframeEl` and reads theme untracked (never tears down + rebuilds for a theme change).
- [ ] After `oninitialized`, the host delivers `sendToolInput` → `sendToolResult` from `mcp.apps.tool_context` via the injected client (D-173, heavy-aware, best-effort — D-226 semantics).
- [ ] **Real-iframe Playwright gate:** boots the vendored ext-apps `App` client in the sandboxed iframe, completes `ui/initialize` end-to-end, toggles theme (asserts `host-context-changed` arrives, handshake stays alive), asserts the delivered tool input/result renders. No `ui/initialize timed out`.
- [ ] **Binding live-render (done-gate):** the real Dockyard `analytics-widgets` MCP app, driven by a real `openai/gpt-5.6-luna` agent (OpenRouter), renders correctly (themed, with data) in the Console/playground — browser-verified with a screenshot. NOT done until this passes.
- [ ] The reverted 109j smoke SKIP guards in `scripts/smoke/phase-109j.sh` flip to OK-on-presence, and `scripts/smoke/phase-109l.sh` asserts the theme/styles + delivery source lines present.

## Files added or changed

```text
web/console/src/lib/chat/renderers/app-bridge-host.ts     # styles.variables host-context at construction; post-init setHostContext theme relay; re-landed oninitialized tool-input/result delivery
web/console/src/lib/chat/renderers/mcp-app.svelte         # thread resolved theme/styles (untracked); isolate lifecycle $effect from theme reactivity; separate theme-relay effect
web/console/src/lib/mcp-app-host-client.ts                # re-add toolContext adapter method (fetch via injected client, D-173)
web/console/src/lib/chat/renderers/theme-tokens.ts        # NEW — map Console tokens.css custom props → ext-apps McpUiStyleVariableKey namespace + prefers-color-scheme resolve
web/console/src/lib/chat/renderers/app-bridge-host.spec.ts        # vitest: theme + styles.variables in ui/initialize; setHostContext patch; delivery order (typed vs vendored schema)
web/console/tests/mcp-app-host-handshake.spec.ts          # NEW real-iframe Playwright: vendored App client, ui/initialize e2e, theme toggle, tool delivery render
scripts/smoke/phase-109l.sh                               # NEW — static presence of theme/styles + delivery; flips 109j SKIP guards
scripts/smoke/phase-109j.sh                               # flip re-land SKIP guards → OK-on-presence
examples/harbor.yaml                                      # (if a host-theme knob is added) documented; else N/A
docs/skills/drive-the-playground/SKILL.md                 # note theme adaptation + data-delivery render (surface: playground)
docs/plans/phase-109l-mcp-apps-theme-datadelivery.md      # this plan
docs/glossary.md                                          # "MCP Apps host theming", "MCP Apps Data Delivery"
```

## Public API surface

No new exported Go/Protocol API (Console-only). Internal to the chat module: `AppBridgeHost` gains a `setHostContext`-relay path + the re-landed delivery; the theme/token mapping helper is chat-module-local.

## Test plan

- **Unit (vitest):** theme + `styles.variables` present in the constructed `ui/initialize` host-context (typed against vendored `McpUiHostContext`); `setHostContext` patch emits `host-context-changed`; delivery pushes input-then-result after initialized; the token mapping produces valid `McpUiStyleVariableKey` names; a theme change does NOT reconstruct the bridge.
- **Integration (real-iframe Playwright):** `mcp-app-host-handshake.spec.ts` — vendored App client, `ui/initialize` end-to-end, theme toggle mid-session (handshake survives), tool input/result renders. This is the regression gate for the reverted break.
- **Conformance:** N/A (single renderer; the "conformance" here is spec-fidelity via §17.8 typed fixtures + the real App).
- **Concurrency / leak:** N/A — the renderer is per-app-instance, not a shared reusable artifact; the lifecycle-teardown correctness is covered by the handshake test.
- **Live (§17.8, binding done-gate, env-gated `HARBOR_LIVE_MCP` + real LLM):** the `analytics-widgets` app renders under a real `gpt-5.6-luna` agent, browser-verified.

## Smoke script additions

- `scripts/smoke/phase-109l.sh` (`PREFLIGHT_REQUIRES: static-only`): assert `app-bridge-host.ts` constructs host-context with `styles`/`variables` + has a `setHostContext` theme relay + the `sendToolInput`/`sendToolResult` delivery; assert the lifecycle `$effect` does not depend on the theme store (grep guard against the reverted pattern). Flip the `scripts/smoke/phase-109j.sh` re-land SKIP guards to OK-on-presence.

## Coverage target

- `web/console/src/lib/chat/renderers`: vitest coverage on the touched host + mapping ≥ the module's current bar; the real-iframe Playwright gate is pass/fail (not coverage-counted).

## Dependencies

- Gate-0 (D-342). Builds on the shipped MCP Apps host (109b, D-173), the backend Data-Delivery capture (109i / D-225 `mcp.apps.tool_context`), and the D-227 backend (`mimeTypes` capability + `RuntimeInfo.MCPAppDisplayModes`) — all on `dev-experimental`.

## Risks / open questions

- **The handshake regression is THE risk.** Mitigated by: the construct-once + lifecycle-isolation + gate-behind-initialized pattern (D-342), the real-iframe Playwright test that would fail on a mid-handshake teardown, and the binding live-render check. The original break was never isolated from git (the 109k Console code was squashed into the revert PR); the root-cause hypothesis (reactive theme in the bridge-owning effect) is documented in D-342 and the safe pattern addresses it regardless.
- §17.8: the fixtures type against the vendored `@modelcontextprotocol/ext-apps@1.7.4` schema; the live-render uses the real Dockyard `analytics-widgets` App + a real LLM — no hand-authored wire shapes.

## Glossary additions

- **MCP Apps host theming** — the host renderer populating `ui/initialize` host-context with `color-scheme` + `styles.variables` (ext-apps `McpUiStyleVariableKey` namespace) and patching them via `host-context-changed`, so a rendered `ui://` app adapts to the host light/dark + native tokens. D-342.
- **MCP Apps Data Delivery** — the host pushing the originating tool call's input + result into a rendered app after `ui/notifications/initialized` (the app renders from host-pushed data; never re-invokes the tool). D-225 (backend) / D-226 + D-342 (Console).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] `web/console` `npm run check` + `lint` + `test` + `build` green; the real-iframe Playwright handshake test passes
- [ ] **Binding live-render done-gate:** `analytics-widgets` renders under a real `gpt-5.6-luna` agent, screenshot captured
- [ ] If multi-isolation paths changed: N/A (Console renderer; identity flows via the injected client's Protocol calls, unchanged)
- [ ] Concurrent-reuse test: N/A — per-app-instance renderer, not a shared reusable artifact (justified above)
- [ ] Integration test exists — the real-iframe Playwright handshake gate (closes the reverted-work seam)
- [ ] If new vocabulary: glossary updated
- [ ] Skill `drive-the-playground` updated same-PR (§18)
