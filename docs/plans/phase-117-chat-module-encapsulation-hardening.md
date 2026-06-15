# Phase 117 — chat module encapsulation hardening

## Summary

Make the chat module (`web/console/src/lib/chat/`) render fully
self-contained — its own theming surface, its own font inheritance, and a
parameterized host identity/theme — so it carries no implicit dependency
on the Console's global stylesheet or app shell. D-091 already mandates
the module be encapsulated ("extract on second consumer"); this phase
delivers the encapsulation half **in place**, independent of any later
`web/shared/` move, so the module is provably standalone where it lives.

## RFC anchor

- RFC §7 — Console layer (the chat/playground surface and its
  Protocol-client contract).

## Briefs informing this phase

- brief 12

## Brief findings incorporated

- brief 12 §35–37: *"The Playground …, the chat composer, the MCP-Apps
  content renderer registry, the file-upload pipeline, the rich-output
  renderers, and the trace toggle are a single component library used by
  both surfaces."* — for one library to serve two surfaces it must not
  inherit styling/identity from one surface's shell; this phase removes
  those implicit inheritances.
- brief 12 §11: the future packed dev UI (`harbor dev --ui`) reuses these
  chat components via the shared library — a legitimate second framework
  consumer whose existence the encapsulation must anticipate.
- brief 12 §26: *"Reference design tokens from a single location … raw
  color/spacing literals in `.svelte` rejected."* — the module references
  tokens only; this phase makes the token contract explicit (the set the
  module depends on) rather than ambient.

## Findings I'm departing from (if any)

None. The `web/shared/chat/` git move (D-091's "extract on second
consumer") is explicitly NOT done here — there is no second consumer yet,
so the move is premature; this phase is the in-place encapsulation that
makes the eventual move mechanical.

## Goals

- The chat panel applies its own `font-family: var(--font-sans)` at its
  root, so it renders correctly without inheriting from the Console's
  global `html, body` rule (the current standalone-render gap).
- Host identity and theme are injected through the module seam, not
  hardcoded: `app-bridge-host.ts`'s `HOST_INFO` and theme become
  parameters supplied by the caller (the Console adapter), defaulting
  sensibly.
- The token contract the module depends on is documented + reconciled
  (the module references only tokens that resolve in `tokens.css`); a
  fixture proves the module renders against just that contract.

## Non-goals

- The `web/shared/chat/` directory move (deferred to the real second
  consumer).
- Any change to the MCP-Apps renderer behaviour or the Protocol client
  surface — this is pure encapsulation.

## Acceptance criteria

- [x] `.chat-panel` (or the module root) sets `font-family` from a token;
      the module renders with correct typography with no Console global
      stylesheet present (a portability fixture asserts this).
- [x] `HOST_INFO` (name/version) and theme are injected via the module's
      typed seam; the baked-in literal becomes the overridable exported
      `DEFAULT_HOST_INFO` default (see D-222 §4.3).
- [x] The chat module has zero imports of other Console internals
      (`$lib/` outside `$lib/chat/`) — now enforced by the mechanical
      encapsulation guard (`scripts/check-chat-encapsulation.mjs`).
- [x] `svelte-check --fail-on-warnings` + `npm run lint` (token-literal
      rule) stay clean.

## Files added or changed

- `web/console/src/lib/chat/ChatPanel.svelte` — root font-family token.
- `web/console/src/lib/chat/renderers/app-bridge-host.ts` — parameterized
  `HOST_INFO` + theme through the injected seam.
- `web/console/src/lib/chat/` — a documented token-contract module +
  portability fixture.
- The Console adapter (`web/console/src/lib/...`) — supplies host
  identity + theme to the module.
- `scripts/smoke/phase-117.sh`.

## Public API surface

- The chat module's injected `ProtocolClient` / host-options interface
  gains host-identity + theme fields. No Go surface.

## Test plan

- **Unit:** a Svelte component test mounts the chat panel with ONLY the
  token contract (no Console global CSS) and asserts computed
  `font-family` resolves; a host-info injection test asserts the bridge
  advertises the injected name/version, not a hardcoded one.
- **Integration:** N/A (frontend-only; the boundary check + portability
  fixture are the cross-surface guards).
- **Conformance:** N/A.
- **Concurrency / leak:** N/A (UI module).

## Smoke script additions

- `scripts/smoke/phase-117.sh` (static-only): grep that the chat module
  applies a font-family token at its root and contains no hardcoded
  `harbor-console` host literal; CI's `frontend` job runs `svelte-check`
  and the lint pass.

## Coverage target

- `web/console` chat module: maintain the Console phase's component-test
  bar; the new portability + injection tests are additive.

## Dependencies

- Phase 109b (the MCP-Apps host + AppBridge that owns `HOST_INFO`/theme),
  Phase 108 (the Console shell + `tokens.css` the contract reconciles to).

## Risks / open questions

- The exact token-contract surface (which subset of `tokens.css` the
  module legitimately depends on) — enumerate at authoring; keep it
  minimal so a second surface can supply it cheaply.
- Theme propagation through the AppBridge to sandboxed app iframes — keep
  the sandbox/CSP envelope (D-173) byte-identical; only the theme value
  flows.
- Full §16 brief pass (brief 12 + D-091/D-092/D-093 context) when
  dispatched; per CLAUDE.md §18, touching the playground surface updates
  the `drive-the-playground` skill in the same PR if a step changes.

## Glossary additions

- `chat-module token contract` — add to glossary if the term is adopted.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes — run by the coordinator (Go-side gate; this
      phase's static smoke `scripts/smoke/phase-117.sh` passes: 7 OK / 0 FAIL)
- [x] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits)
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (chat-module component
      tests extended: injection + portability + guard specs added, all green)
- [x] If multi-isolation paths changed: cross-session isolation test passes — N/A (UI-only)
- [x] Concurrent-reuse test — N/A (UI module, no reusable Go artifact)
- [x] Integration test — N/A (frontend-only; the encapsulation guard + portability test are the guards)
- [x] If new vocabulary: glossary updated (`chat-module token contract`)
- [x] If a brief finding was departed from: justified + decisions.md entry (D-222 §4.3)
