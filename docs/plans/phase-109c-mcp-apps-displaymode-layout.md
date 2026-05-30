# Phase 109c — mcp-apps-displaymode-layout

## Summary

Implement the Playground page-level layout state machine that honours an active MCP App's `DisplayMode` (D-062): `fullscreen` (the app replaces the chat + composer region and is addressable via a tab strip within the session view — multiple fullscreen apps yield multiple tabs) and `pip` (a resizable 50/50 split between the chat column and the app panel, with the right rail hidden by default and a toggle to reopen it). `inline` already shipped in Phase 109b and is unchanged here. The AppBridge `onrequestdisplaymode` handler (wired in 109b) drives runtime transitions between `inline ↔ fullscreen ↔ pip` without reloading the session, and the operator can switch via UI affordances. This is the final phase of the three-phase MCP Apps host wave (109a runtime/protocol projection, 109b iframe host + AppBridge + inline mode, 109c this), superseding the deprecated Phase 85g (D-172).

## RFC anchor

- RFC §7
- RFC §6.4

## Briefs informing this phase

- brief 11
- brief 14

## Brief findings incorporated

- brief 11 §"Playground" / §PG-3: the MCP-Apps `DisplayMode` honoring (`inline` chat-scroll widget, `fullscreen` new tab within the agent/session view, `pip` split-screen default 50/50 resizable per D-062) is part of the chat module's Console scope — this phase delivers the page-level layout that surfaces it.
- brief 11 §PG-6: side-by-side two-agent comparison foreshadows Evaluations (D-064, post-V1) — this phase's `pip` is **one app beside chat**, explicitly NOT the two-agent comparison surface; the distinction is held below in Risks.
- brief 14 §6: "MCP Apps … is **Console** work, not runtime-driver work — it touches `web/console`." — this phase is Console-side only; no Go surface, no new Protocol method.
- brief 14 §6: the MCP-Apps renderer ships inside the shared chat module per D-091; this phase reuses 109b's renderer (`web/console/src/lib/chat/renderers/`) inside the new `fullscreen` / `pip` host panels rather than forking it.

## Findings I'm departing from (if any)

None.

## Goals

- A Playground layout state machine that maps the active app's `DisplayMode` (projected by 109a, surfaced by 109b) to a page-level region routing:
  - `fullscreen`: the MCP App replaces the chat + composer region; a tab strip within the session view lets the operator switch between Chat and the app, and between multiple fullscreen apps.
  - `pip`: a resizable 50/50 split between the chat column and the app panel; the right rail is hidden by default in `pip`, with a toggle to reopen it.
  - `inline`: unchanged from 109b — the app renders as a widget in the chat scroll.
- The AppBridge `onrequestdisplaymode` (wired in 109b) drives runtime transitions between `inline ↔ fullscreen ↔ pip` without reloading the session; the operator can also switch via UI affordances.
- Closing or tearing down an app returns the layout to the chat + right-rail default.
- Design tokens only — no raw color / spacing / type-scale literals; the `.stylelintrc.cjs` token-surface rule (`npm run lint`) gates this. Skeleton primitives are used where they fit (D-092, §4.5 rule 4).
- The change is contained to the Playground page plus small new components; the shared chat module's renderer (109b) is reused, not forked.

## Non-goals

- The iframe host, the AppBridge `postMessage` JSON-RPC dialect, and `inline` rendering — all shipped in Phase 109b.
- The runtime / protocol projection of `DisplayMode` — shipped in Phase 109a; this phase consumes the projected value, it does not define wire surface.
- Authoring MCP Apps — Harbor *hosts* apps; building them is a server-author concern.
- Side-by-side TWO-agent comparison (PG-6, post-V1 / D-064) — distinct from `pip`, which is one app beside chat. No comparison back-end here.
- Persisting layout state across sessions — layout is conversation-scoped; only Console-local view state (split ratio, rail toggle position) may be remembered per D-061.

## Acceptance criteria

- [ ] An app with `DisplayMode` `fullscreen` replaces the chat + composer region; a tab strip lets the operator switch between Chat and the app, and between multiple fullscreen apps.
- [ ] An app with `DisplayMode` `pip` renders a resizable 50/50 split (chat | app); the right rail is hidden by default in `pip`.
- [ ] In `pip`, a toggle reopens the right rail; the split ratio is clamped to sane bounds when the operator drags the divider.
- [ ] `inline` continues to render in the chat scroll — 109b behaviour is unchanged (regression guard).
- [ ] `onrequestdisplaymode` from the app transitions the layout at runtime (`inline → pip`, `pip → fullscreen`, etc.) without reloading the session.
- [ ] Closing or tearing down an app returns the layout to the chat + right-rail default.
- [ ] The layout uses design tokens only — no raw color / spacing / type-scale literals in the new `.svelte` files (the `.stylelintrc.cjs` token-surface rule; `npm run lint` gates it).
- [ ] `svelte-check --fail-on-warnings` and the Console lint pass.
- [ ] A Playwright test asserts each `DisplayMode`'s layout: `fullscreen` tab strip + app-replaces-chat; `pip` 50/50 split + rail hidden + toggle reopens; `inline` unchanged.

## Files added or changed

- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — the layout state machine; replaces the fixed `grid-template-columns: 1fr var(--size-rail)` grid with `DisplayMode`-driven region routing (chat-only + rail default, `fullscreen` tab strip, `pip` split).
- `web/console/src/lib/components/playground/AppPanel.svelte` — hosts the 109b MCP-Apps renderer in the `fullscreen` and `pip` regions; takes the active app reference + its `DisplayMode`.
- `web/console/src/lib/components/playground/AppTabStrip.svelte` — the `fullscreen` tab strip (Chat tab + one tab per fullscreen app); add / remove / activate. If Skeleton ships a Tabs primitive that fits, this wraps it rather than rebuilding (justified in PR per §4.5 rule 4).
- `web/console/src/lib/components/playground/SplitPane.svelte` — the resizable `pip` split; takes two slot regions + a ratio, clamps the ratio on drag. If a Skeleton splitter primitive fits, this wraps it rather than rebuilding (justified in PR).
- `web/console/src/lib/components/playground/layout.ts` (or co-located module) — the pure layout state-machine logic (`DisplayMode` → region routing; ratio clamp; tab add/remove) so it is unit-testable without the DOM.
- `web/console/tests/` — the Playwright layout suite asserting the per-`DisplayMode` scenarios.
- `scripts/smoke/phase-109c.sh` — the static-only smoke (this PR).
- `docs/plans/README.md` — Status flip on merge (by coordinator).

## Public API surface

Console-side TypeScript only — no Go surface, no new Protocol method (the `DisplayMode` value rides 109a's projection, surfaced by 109b).

- `AppPanel` props: the active app reference + its `DisplayMode`.
- `AppTabStrip` props: the ordered tab set (Chat + fullscreen apps), the active tab id; emits activate / close.
- `SplitPane` props: two slot regions (left = chat, right = app) + a ratio (default `0.5`); emits the clamped ratio on drag.
- The layout state machine: a pure function mapping `(DisplayMode, openApps)` → the active region layout, exported from `layout.ts` for unit tests.

## Test plan

- **Unit:** the layout state machine (`DisplayMode` → region routing); the `SplitPane` ratio clamp (lower/upper bounds honoured); tab-strip add / remove / activate. Pure TS, no DOM.
- **Integration:** a fixture app driven through `inline → pip → fullscreen` via a simulated `onrequestdisplaymode`, asserting the DOM regions swap correctly and the session does not reload; closing the app returns to the chat + rail default.
- **Conformance:** N/A — no wire surface; the `DisplayMode` value is projected by 109a and covered by that phase's conformance.
- **Concurrency / leak:** N/A — Console-side page layout; there is no runtime artifact reused across goroutines. The concurrent-reuse contract (§5, D-025) applies to Go runtime artifacts, not Svelte components.
- **Playwright (CI gate):** the acceptance-criteria layout scenarios — `fullscreen` tab strip + app-replaces-chat; `pip` 50/50 split + rail hidden + toggle reopens; `inline` unchanged.

## Smoke script additions

- `scripts/smoke/phase-109c.sh` (classification: `static-only`):
  - Assert the Playground page (`+page.svelte`) references the `fullscreen` / `pip` / `inline` `DisplayMode` branches.
  - Assert the new layout components exist (`AppPanel.svelte`, `AppTabStrip.svelte`, `SplitPane.svelte`).
  - Assert no raw color / spacing literals in the new `.svelte` files (token-surface rule, §4.5).

## Coverage target

- `web/console` (Playground layout + new components): 80%.

## Dependencies

- 109b — the iframe host + AppBridge + `inline` mode this phase extends.
- 109a (transitive) — the runtime/protocol `DisplayMode` projection.
- 108 (transitive) — the Playground page polish + Console shell layout this builds on.
- 73n (transitive) — the Console foundation the Playground page sits on.

## Risks / open questions

- **`pip` vs PG-6 confusion.** `pip` is ONE app beside chat — a single MCP App panel split with the chat column. PG-6 (two-agent / two-model side-by-side comparison) is a different, post-V1 surface (D-064). They look superficially similar (two columns) but are distinct features; this phase implements `pip` only and must not grow comparison affordances.
- **Page-level complexity.** The layout state machine adds page-level state to an already-large Playground page. If it grows beyond what the page can carry cleanly, extract a `PlaygroundLayout` controller component / module — flagged here as the decomposition path rather than left implicit.
- **Split + rail interaction in `pip`.** The default is unambiguous: in `pip` the right rail is **hidden**, and a toggle reopens it. The resizable split divider and the rail toggle are independent affordances; reopening the rail in `pip` must not reset the split ratio.
- **`DisplayMode` semantics.** Honour D-062 exactly (the glossary-defined `inline` / `fullscreen` / `pip` semantics). Do not invent new modes or alter the meaning of the existing three.

## Glossary additions

- `DisplayMode` already exists (D-062) — do **not** redefine; this phase references it.
- **App panel** (candidate term for the coordinator) — the Playground region that hosts the 109b MCP-Apps renderer when an app is in `fullscreen` or `pip` mode (as opposed to the `inline` widget in the chat scroll). Listed for the coordinator to land in `docs/glossary.md` if deemed worth a formal entry; not redefined here.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target (`web/console`: 80%)
- [ ] Multi-isolation note — N/A: this is Console-side page layout; identity flows through the Protocol client established in 109b/108 and is unchanged here. No new identity-scoped code path.
- [ ] Concurrent-reuse test — N/A: Console-side Svelte layout; the §5 / D-025 concurrent-reuse contract applies to Go runtime artifacts, not Svelte components. Marked N/A with this reason.
- [ ] **Integration / Playwright test passes** — the per-`DisplayMode` layout scenarios (fullscreen tab strip + app-replaces-chat; pip 50/50 + rail hidden + toggle reopens; inline unchanged) are green.
- [ ] `svelte-check --fail-on-warnings` + Console lint (no raw color/spacing literals) pass.
- [ ] Glossary updated (coordinator lands the `App panel` term if accepted; `DisplayMode` unchanged).
- [ ] No brief departures (this plan departs from none).
