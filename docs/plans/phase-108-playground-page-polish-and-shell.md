# Phase 108 — Playground page polish + Console shell layout (D-167)

## Summary

The Playground page today is structurally complete (Phase 73n shipped the wiring; Phase 106 plumbed the real answer; Phase 107/107a will stream tokens + project reasoning) but visually nowhere near the binding mockup at `docs/rfc/assets/console-playground-page.png`. The current page renders as a single vertically-scrolling document with no app-shell brand, no agent identity in the header, raw-string assistant messages (markdown leaks as literal `**bold**`), no KPI strip, monochrome chips with no intent palette, and no bottom status bar. The mock is a full-bleed dashboard with four independently-scrolling regions (sidebar / chat history / right rail / fixed footer + composer dock) that fit one viewport.

This phase is the **first of a 14-round page-by-page polish series** — one phase per Console page, in lockstep with the binding `docs/design/console/CONVENTIONS.md` foundation and the 14 page-spec files at `docs/design/console/page-*.md`. Phase 108 takes the Playground from "demo-shaped" to "operator-product-shaped" without changing any Protocol surface, any wire shape, or any runtime behaviour. The work is mechanical: app-shell layout fix (fixed-height viewport, three scrollable regions), token-surface expansion (chip palette / avatar palette / type scale / tabular-num), markdown rendering in chat bubbles (in-house safe subset — no new dependency, preserving CLAUDE.md §13's no-new-deps-without-RFC posture), KPI strip + agent-identity header + bottom status bar, and component-level polish (Controls / Pending Interventions / Recent Artifacts / Composer).

Phase 108 is intentionally **Console-only and additive**: no Protocol method, wire field, or Runtime emit changes. Streaming token rendering (Phase 107) and reasoning-accordion population (Phase 107a) land on the same polished surface — Phase 108 is upstream of those streams and does not block them.

## RFC anchor

- RFC §7 — Console as Protocol client (the Playground is the load-bearing first-touch operator surface, and is the canonical "Console is a real product" demo per RFC §1).
- RFC §1 — first-five-minutes adoption guarantee (visual polish is what determines whether the operator stays past the first send).

## Briefs informing this phase

- brief 11 — Console feature surface (the binding feature inventory for the Playground page, §PG-1 through §PG-7).
- brief 12 — Console deployment + shared UI library (the shared `web/console/src/lib/chat/` module per D-091).
- brief 13 — react prompt engineering / operator UX (the "operator reads the visual as the product" finding — perceived polish dominates first impressions).

## Brief findings incorporated

- **brief 11 §"Playground / direct interaction".** "Every Playground turn flows through the canonical Protocol — there is no parallel chat surface and no debug shortcut." Phase 108 enforces this by never inventing a markdown-rendering Protocol field — the bubble parses the same `result_inline` envelope Phase 106 pinned and renders it client-side. No new wire shape.
- **brief 11 §"Constraints on the Playground".** "Identity is mandatory; audit and ceiling enforcement uniform with production." Phase 108 visualises the existing `(tenant, user, session)` triple in the header chip + scope indicator rather than introducing a new surface; the cost-ceiling state visualisation reads from the existing `governance.budget_exceeded` event (no new emit).
- **brief 12 §"The shared chat / playground library".** "Encapsulate first, extract on second consumer (D-091)." Phase 108 keeps every new component under `web/console/src/lib/chat/` or `web/console/src/lib/components/playground/` — nothing migrates to a notional `web/shared/chat/` (the second consumer hasn't shipped). A future packed dev UI in `harbor dev` reuses these components mechanically after extraction.
- **brief 12 §"Console design tokens — one scale, one place".** "All chip / status / accent palettes live in `tokens.css`; raw literals in `.svelte` are mechanically rejected." Phase 108 extends `tokens.css` in place (CLAUDE.md §4.5 rule 3, CONVENTIONS.md §7) — no per-phase append block.
- **brief 13 §"Perceived latency dominates first impressions".** "A Playground that reads as unfinished erases every other success." The bullet-list / bold-text / inline-code segments the LLM already returns are leaking as raw text today — the same finding that justified Phase 106 (silent answer-drop) applies to silent-markdown-leak.

## Findings I'm departing from (if any)

- **`MessageBubble.svelte` comment "full markdown render is post-V1 (it needs vetted renderer dependencies — an RFC change per CLAUDE.md §13)"** — Phase 108 departs from this stance by shipping an in-house safe subset (CommonMark headings + bullets + numbered lists + bold/italic + inline code + line breaks; NO HTML passthrough, NO link autolink, NO tables, NO KaTeX, NO Mermaid) — entirely without a new dependency. The full CommonMark + GFM + KaTeX + Mermaid surface promised by page-playground.md §3 PG-4 remains a future RFC-blessed dependency addition. The subset is sufficient for the mockup's content (the LLM consistently returns `**bold:**` + bullet lists + inline code) and matches CLAUDE.md §13's no-new-deps-without-RFC posture. Justification: the cost of writing ~120 LOC of CommonMark-subset parser is lower than the cost of an RFC PR for a new dependency, and the subset is mechanically auditable (no shell-out, no eval, no HTML).
- **Master-plan row labels Phase 108 a V1.3-band concern** — Phase 108 lives in **V1.2** alongside 105/106 because the visual polish is part of the first-five-minutes adoption surface; streaming (107) and reasoning (107a) ride on top of it in V1.3. The detail block reflects this; the master-plan row uses `Pending (V1.2)`.

## Goals

- The Playground page **fills the viewport** (`100vh`) with **four independently-scrollable regions** that match the mockup: the persistent sidebar, the chat history (the only region that grows-and-scrolls dynamically), the right rail (Controls / Pending Interventions / Recent Artifacts), and a fixed bottom dock (composer + status bar). Whole-document scrolling is removed.
- Every assistant chat bubble renders **markdown** (in-house safe subset — see §"Findings I'm departing from") instead of raw text. The user bubble stays plain text. Streaming bubbles (Phase 107 consumer, not implemented in Phase 108) re-parse on each delta — the bubble re-renders deterministically.
- The Console **brand surfaces** — Harbor logo at `docs/rfc/assets/harbor_logo.svg` is inlined into the sidebar header; the top bar carries an identity chip showing `(tenant · user)` scope and a clear connection dot.
- The Playground header is a **real operator-facing surface**, not a page title: `<session-id>` (mono, copyable) + agent display name + a status pill (Active / Paused / Ready / Failed) + a planner pill + a token-count chip + a cost chip — the exact mockup composition.
- A **KPI strip** sits under the header — four tiles (Tokens with mini-sparkline, Cost with ceiling-percent label, p50 latency, Status) that read from the same stores the header chips read from. Numerics use tabular-nums.
- A **bottom status bar** lives inside the app shell, above the existing `ConnectionFooter`: a streaming-state chip, the Protocol version, an Events Stream Live/Off indicator, a Console build chip on the right.
- The **right rail** renders three actual cards with the mockup's visual treatment: Controls (segmented Model / Reasoning effort / Temperature / Top P / Max tokens / System prompt override / Run-as-identity), Pending Interventions (red count badge + Approve / Reject button intents), Recent Artifacts (kind icons + name + size + age).
- The **chip palette** is real: `info` / `success` / `warning` / `danger` / `accent` / `neutral` chip tokens (foreground / background / border) live in `tokens.css` and back every status pill across the page. The audit (`.stylelintrc.cjs`) keeps raw literals out.
- The page passes the **CONVENTIONS.md §5 depth bar** (already passed by Phase 73n) AND adds the new bottom-status-bar surface to the app shell without breaking the other 13 Console pages — they all keep working without re-styling.

## Non-goals

- **No Protocol method changes.** Phase 108 introduces no new wire field, no new event type, no new method, no new payload key. Everything renders from already-shipped surfaces. (Streaming events are Phase 107; reasoning projection is Phase 107a; multimodal upload is Phase 84b.)
- **No streaming wiring.** Phase 108 ships the polished bubble shape that Phase 107 will pipe deltas into. Phase 108 does not subscribe to a new event.
- **No full CommonMark / GFM / KaTeX / Mermaid renderer.** The in-house subset (headings / bullets / numbered lists / bold / italic / inline code / line breaks) is what ships. A future RFC PR can pull in a vetted markdown library for the full surface; Phase 108 does not block on it.
- **No new dependencies.** No new npm package; no new Go package. The in-house markdown subset is ~120 LOC of plain TypeScript; the sparkline is inline SVG; the KPI tile is plain CSS / Skeleton primitives.
- **No changes to the other 13 Console pages.** Phase 108 touches the app shell, but only additively — the new bottom status bar is opt-in via a layout slot the 13 pages render as `null` (the slot returns nothing when its data sources aren't wired). The polish for those pages lives in their own follow-up phases (109..121, one per page).
- **No new Console DB schema.** The bottom status bar reads from the existing connection / events subscriber; the KPI strip reads from the existing aggregations the header already reads. No new tables.
- **No mock-LLM-only data.** Every chip / KPI / pill reads from real shipped data; the disconnected state reads from the existing `connection.ts` resolver per CONVENTIONS.md §4. A page rendered offline shows the disconnected `<PageState>` exactly as today.

## Acceptance criteria

The bullets below are binding.

### Layout / shell

- [ ] **AC-1** `web/console/src/routes/(console)/+layout.svelte`'s `.console-shell` changes from `min-height: 100vh` to `height: 100vh; overflow: hidden`. `.main-column` gains `min-height: 0; overflow: hidden`. `.content` becomes `overflow: hidden; min-height: 0; flex: 1` (NOT `overflow: auto`, NOT scrollable — the inner page owns its scroll). The sidebar becomes its own scroll container (`overflow-y: auto`) when its content overflows.
- [ ] **AC-2** A new layout slot `<svelte:fragment slot="status-bar">` (or the Svelte 5 runes equivalent — a `statusBar?: Snippet` prop on the shell) is added to `(console)/+layout.svelte`, rendered above `<ConnectionFooter>` inside `.main-column`. When the slot is unprovided (every page except Playground in Phase 108), the slot renders nothing. The Connection Footer stays as-is.
- [ ] **AC-3** The 13 non-Playground Console pages render unchanged. Their existing `.content` padding still works — the AC-1 reshape preserves the `.content { padding: var(--space-6); }` rule. A Playwright smoke (`web/console/tests/shell-no-regression.spec.ts` — new) loads `/overview`, `/tools`, `/sessions`, `/settings` after the AC-1 change and asserts no horizontal overflow + no double scrollbars.

### Tokens / palette

- [ ] **AC-4** `web/console/src/lib/tokens.css` extends the existing palette IN PLACE (no per-phase append block) with the chip palette:

  ```css
  /* Chip palette — backs every status pill across the Console.
     One row per intent: foreground / background / border. */
  --chip-info-fg: <derived from --color-accent>;
  --chip-info-bg: <derived from --color-accent-soft>;
  --chip-info-border: <derived from --color-accent>;
  --chip-success-fg / -bg / -border: <from --color-success / --color-success-soft>;
  --chip-warning-fg / -bg / -border: <from --color-warning / --color-warning-soft>;
  --chip-danger-fg / -bg / -border: <from --color-danger / --color-danger-soft>;
  --chip-accent-fg / -bg / -border: <from --color-accent>;
  --chip-neutral-fg / -bg / -border: <from --color-text-muted / --color-surface-raised>;
  ```

  Every value derives from the existing base palette via `var(...)` — no new raw hex literals. The grouping comment treats this as the "chip" sub-section of the color block, not a phase stamp.
- [ ] **AC-5** Tabular-nums for every numeric chip / KPI / column: a single utility class `.tabular-nums { font-variant-numeric: tabular-nums; }` lands in `web/console/src/lib/components/ui/_utilities.css` (or extended into `tokens.css` under a typography sub-section). KPI strip, token count, cost, latency, durations all carry it.
- [ ] **AC-6** Type scale gains one new step: `--text-2xl: 1.5rem; --text-3xl: 1.875rem` for the KPI value rendering. The existing `--text-xs / sm / base / lg / xl` are untouched (preserving the 13 other pages).
- [ ] **AC-7** The existing `StatusChip` component at `web/console/src/lib/components/ui/StatusChip.svelte` consumes the new chip palette — its `kind` prop maps onto `info / success / warning / danger / accent / neutral`. Pre-existing usages keep working (the kinds it already supports — `online` / `offline` / `error` — alias onto the new palette).

### App shell brand + identity

- [ ] **AC-8** The sidebar header renders the inlined Harbor logo (`docs/rfc/assets/harbor_logo.svg`, ~3.4 KB SVG — checked into `web/console/static/harbor_logo.svg` and inlined or referenced) next to the `Harbor Console` wordmark. The wordmark stays — the logo is to the left, at 24×24 pt.
- [ ] **AC-9** The top bar's identity indicator gains a **scope chip** — `(tenant · user)` is presented as a chip-shaped surface (border + radius) rather than raw text. The session id stays in mono. The runtime URL is hidden behind a tooltip on the connection dot (the current top-right text wrapping is the cause of the cramped look in the existing screenshot).
- [ ] **AC-10** Sidebar active-state visual treatment: 2px left accent rail (`--border-emphasis-width` + `--color-accent`) + brighter text + subtle bg tint. The 13 other pages get this for free.

### Playground header + KPI strip

- [ ] **AC-11** `web/console/src/lib/components/playground/Header.svelte` is restructured to match the mock: a single row with **breadcrumb-prefixed session id** (`<runtime> / Playground / sess_…`, copy-on-click) on the left; agent display name + a Status pill + a Planner pill in the middle-left; the cost chip + token chip on the right; Cancel run + Restart buttons rightmost. Existing Header surface is replaced, not augmented (one component per concern per CONVENTIONS.md §3).
- [ ] **AC-12** A new `KpiStrip.svelte` lands at `web/console/src/lib/components/playground/KpiStrip.svelte`. Four tiles:
  - **Tokens** — large numeric (tabular-nums), mini-sparkline beside it (60-pt inline SVG fed from a circular buffer of the last 60 token-delta observations).
  - **Cost** — `$0.0834` numeric + a sub-label `87% of $0.10 ceiling` rendered in chip-warning when ≥80% of the ceiling, chip-neutral otherwise.
  - **p50 latency** — large numeric in ms with `--text-3xl`; sub-label "p50 over last 20 turns".
  - **Status** — large checkmark / spinner / cross depending on Active / Streaming / Failed; sub-label is the planner phase (`Ready` / `Reasoning` / `Calling tool` / `Idle`).
  Each tile is `var(--space-3)` padding, `var(--border-hairline)` border, `var(--radius-md)` radius, and references no raw literals.
- [ ] **AC-13** The yellow "Topology view not available on this Runtime" banner currently rendered as a full-width pre-chat block is moved to a small **info chip in the KPI strip's Status tile sub-label** (when the planner runtime has no topology). The full-width banner is gone. (Closes the visual-real-estate complaint from the operator screenshot.)

### Chat bubble — markdown + avatars

- [ ] **AC-14** `MessageBubble.svelte` gains an avatar slot: a 32×32 pt circular surface to the left of the bubble head, colour derived from a deterministic hash of `message.role + (message.agentID ?? '')`, with the role's initials inside (`U` for user, `A` for agent, `S` for system). The avatar surface uses the chip palette (accent for user, success for agent, neutral for system) — no new colour tokens.
- [ ] **AC-15** `MessageBubble.svelte` plain-text segments are rendered through a new `MarkdownInline.svelte` component under `web/console/src/lib/chat/MarkdownInline.svelte`. The component:
  - Accepts a `source: string` prop.
  - Parses an in-house safe subset: headings (`#` / `##` / `###`); ordered / unordered lists (with single-level nesting only); bold (`**` / `__`); italic (`*` / `_`); inline code (`` ` ``); fenced code blocks (already supported by `CodeBlock.svelte`, but `MarkdownInline` re-routes them to it); line breaks (`\n\n` → paragraph, single `\n` → `<br>`).
  - Rejects all HTML — `<` is escaped verbatim. No autolinking, no tables, no KaTeX, no Mermaid (those land with a future RFC-blessed library).
  - Is implemented in plain TypeScript, ~120 LOC, with a Vitest spec at `web/console/src/lib/chat/MarkdownInline.spec.ts` pinning each grammar rule.
- [ ] **AC-16** The MessageBubble's `// V1 renders plain text verbatim … a full markdown/KaTeX/Mermaid render is post-V1` comment is updated to reflect the new state: in-house safe subset shipped; full CommonMark/GFM/KaTeX/Mermaid still post-V1 pending an RFC PR for a dependency.
- [ ] **AC-17** The bubble head changes from `<role-tag> <time>` to `<agent-display-name>  ·  <human-readable timestamp>  ·  <planner-phase pill>`. The role tag is folded into the agent-display-name (agent / user / system); the timestamp uses the same format the rest of the Console uses (`2026-05-26 15:42:50 UTC`, locale-aware via `Intl.DateTimeFormat`).

### Right rail polish

- [ ] **AC-18** `ControlsCard.svelte` is restructured to match the mock: Reasoning effort becomes a segmented control (`Low | Medium | High | Default`) instead of a `<select>`; Temperature and Top P stay sliders but gain a numeric chip on the right of each slider (tabular-nums); Max tokens stays a `<select>` but inside a typed combobox shape; System prompt override stays a textarea but the "Apply to next message" button moves to a sticky bottom action on the rail (not inside the card). Drift mode remains the existing post-V1 disabled checkbox with a tooltip.
- [ ] **AC-19** `PendingInterventionsCard.svelte` gains a red count badge in its header (`Pending Interventions ●2` shape) when `interventions.length > 0`. Each row renders an avatar (the row's intent — tool approval / OAuth / HITL) plus a typed title in the matching chip-intent colour, plus an Approve (success-intent) and Reject (danger-intent) button.
- [ ] **AC-20** `PlaygroundArtifactsCard.svelte` (Recent Artifacts) renders a kind icon (text / image / pdf / audio / json — the renderer-registry's `mimeIcon`) + filename (mono) + size (tabular-nums) + age (`2m ago`) per row. The existing `↗` open-action is a button that navigates to `/artifacts/<id>`.

### Bottom dock

- [ ] **AC-21** A new `PlaygroundStatusBar.svelte` lands at `web/console/src/lib/components/playground/PlaygroundStatusBar.svelte` and is rendered into the shell's `status-bar` slot. Four indicators on a single 28-pt row: streaming-state chip (`Idle` / `Streaming` / `Paused` / `Failed`); Protocol version (`Protocol v1.0` — read from the same surface the existing breadcrumb reads); Events Stream live indicator (`● Events Stream: Live` in success-intent when subscribed, `Off` in neutral); Console build chip on the right (`Console v0.1.x` — read from `import.meta.env.VITE_CONSOLE_VERSION` or a `__CONSOLE_VERSION__` Vite-defined constant; falls back to `dev` when absent).
- [ ] **AC-22** The composer (`ChatComposer.svelte`) gets a darker recessed background (`var(--color-bg)` instead of the current `var(--color-surface)`), a hairline divider between the attach button and the textarea, and a teal-intent Send button (using the chip-accent palette — no new accent token).

### Markdown safety

- [ ] **AC-23** `MarkdownInline.spec.ts` includes an XSS-attempt test: `<script>alert(1)</script>` in a message renders the literal angle-bracket text (NOT the script element). `[click](javascript:alert(1))` renders as plain text (no autolink). `<img onerror=...>` renders as escaped text. The component never produces HTML strings that bypass Svelte's auto-escaping.
- [ ] **AC-24** A second spec asserts no `eval`, no `Function(...)`, no `innerHTML`, no `outerHTML`, no `document.write` in the rendered output's call sites — verified by a small AST-level lint or a string-grep in the parser source.

### Tests

- [ ] **AC-25** Unit (Svelte / Vitest):
  - `MarkdownInline.spec.ts` — each grammar rule (headings, bullets, numbered lists, bold, italic, inline code, fenced code, line breaks); the XSS attempts above; the no-HTML-passthrough property.
  - `KpiStrip.spec.ts` — renders four tiles; the cost ceiling-percent chip flips from `chip-neutral` to `chip-warning` at the 80% threshold; the sparkline accepts a 60-point buffer.
  - `Header.spec.ts` — the agent display name fallback chain (`agents.get()` → `agent_id` → `default agent`); status pill maps `Active` / `Paused` / `Failed`.
  - `PlaygroundStatusBar.spec.ts` — the four indicators render; absent connection routes all four to `neutral`.
  - Existing `MessageBubble.spec.ts` (or its addition) — extends to cover the avatar + markdown path.
- [ ] **AC-26** Integration (Playwright):
  - `web/console/tests/playground-polish.spec.ts` — open the Playground against a fixture-injected `ProtocolClient`, send a message whose mock response is `**By topic:** music, *games*, and \`code\`.`, assert the rendered DOM contains a `<strong>` with text `By topic:` (not the literal `**By topic:**`).
  - Same spec asserts the KPI strip's four tiles render with the right numeric formatting + sparkline SVG present.
  - Same spec asserts the bottom status bar's four indicators are present (the protocol version chip's text === the same version the breadcrumb reads).
- [ ] **AC-27** Shell no-regression spec `shell-no-regression.spec.ts` (new) loads each of the 13 non-Playground pages against the same fixture client; asserts no horizontal overflow, no double-scrollbars, and that the AC-1 layout reshape did not break their content padding.

### Drift / hygiene

- [ ] **AC-28** No `.svelte` file under `web/console/src/lib/components/playground/` or `web/console/src/lib/chat/MarkdownInline.svelte` introduces a raw color / hex / rgb literal — the existing `.stylelintrc.cjs` rejects them; CI's `npm run lint` is the gate.
- [ ] **AC-29** No new npm dependency lands in `web/console/package.json`. `git diff -- web/console/package.json web/console/package-lock.json` shows no version bumps and no new top-level entries.
- [ ] **AC-30** No new Go dependency lands. (Phase 108 is Console-only; the Go side is untouched.)
- [ ] **AC-31** Skill drift (CLAUDE.md §18) — `docs/skills/drive-the-playground/SKILL.md` is updated in the same PR to reflect the new visual surface (the operator now sees agent display name + status pill + KPI strip + bottom status bar). The skill's `metadata.surface: playground` already names this; only the prose body updates.

## Files added or changed

### Console — app shell

- `web/console/src/routes/(console)/+layout.svelte` — AC-1 reshape (`.console-shell` → fixed-height + overflow-hidden), AC-2 `status-bar` slot, AC-8 logo, AC-9 scope chip, AC-10 active rail.
- `web/console/static/harbor_logo.svg` — **NEW** (copy of `docs/rfc/assets/harbor_logo.svg`, ~3.4 KB).

### Console — tokens

- `web/console/src/lib/tokens.css` — AC-4 chip palette extension in place (no per-phase block), AC-6 `--text-2xl` / `--text-3xl`, AC-5 tabular-nums utility (or to `_utilities.css` — pick one).

### Console — Playground components

- `web/console/src/lib/components/playground/Header.svelte` — AC-11 restructure.
- `web/console/src/lib/components/playground/KpiStrip.svelte` — **NEW** AC-12.
- `web/console/src/lib/components/playground/PlaygroundStatusBar.svelte` — **NEW** AC-21.
- `web/console/src/lib/components/playground/ControlsCard.svelte` — AC-18 restructure.
- `web/console/src/lib/components/playground/PendingInterventionsCard.svelte` — AC-19 count badge + intent colours.
- `web/console/src/lib/components/playground/PlaygroundArtifactsCard.svelte` — AC-20 kind icons + age + size.

### Console — chat module

- `web/console/src/lib/chat/MessageBubble.svelte` — AC-14 avatar slot, AC-15/AC-17 bubble head, AC-16 comment update.
- `web/console/src/lib/chat/MarkdownInline.svelte` — **NEW** AC-15.
- `web/console/src/lib/chat/MarkdownInline.spec.ts` — **NEW** AC-23 / AC-24.
- `web/console/src/lib/chat/ChatComposer.svelte` — AC-22 visual recess + Send-button intent.

### Console — UI inventory

- `web/console/src/lib/components/ui/StatusChip.svelte` — AC-7 maps `kind` onto the new chip palette. Pre-existing kinds (`online` / `offline` / `error`) keep working as aliases.

### Console — Playground page wiring

- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — wires the new components in (KpiStrip + PlaygroundStatusBar via slot). No other behaviour change. Removes the topology-banner block in favour of AC-13.

### Specs

- `web/console/src/lib/chat/MarkdownInline.spec.ts` — AC-23 / AC-24.
- `web/console/src/lib/components/playground/KpiStrip.spec.ts` — AC-25.
- `web/console/src/lib/components/playground/Header.spec.ts` — AC-25.
- `web/console/src/lib/components/playground/PlaygroundStatusBar.spec.ts` — AC-25.
- `web/console/tests/playground-polish.spec.ts` — **NEW** AC-26 Playwright.
- `web/console/tests/shell-no-regression.spec.ts` — **NEW** AC-27 Playwright.

### Smoke + drift

- `scripts/smoke/phase-108.sh` — **NEW** (static-only — Console-only phase; assertions on file existence + token references + no-new-deps).

### Docs

- `docs/skills/drive-the-playground/SKILL.md` — AC-31 prose update.
- `docs/glossary.md` — three new entries: `KpiStrip` (the Playground header KPI strip component), `MarkdownInline` (the in-house safe-subset markdown renderer), `Playground status bar` (the bottom dock indicators). Alphabetised under K / M / P.

## Public API surface

- **No Protocol method, event, or wire-shape change.** Phase 108 is Console-only.
- **No Go-side public surface.**
- **Console-internal `Snippet` / slot surface:** `(console)/+layout.svelte` adds an optional `statusBar?: Snippet` prop. Existing pages omit it; only the Playground provides it in Phase 108. The 13 other pages stay source-stable.

## Test plan

### Unit (Svelte / Vitest)

Per AC-25. Files listed above. Each spec uses the existing Vitest harness (no new test runner).

### Integration (Playwright)

- `playground-polish.spec.ts` — AC-26: against a fixture `ProtocolClient`, send a message → assert the rendered markdown elements (no literal `**`) + KPI tiles + status bar.
- `shell-no-regression.spec.ts` — AC-27: load `/overview`, `/tools`, `/sessions`, `/settings`, `/artifacts`, `/memory`, `/flows`, `/mcp-connections`, `/agents`, `/events`, `/tasks`, `/background-jobs`, `/live-runtime`; assert no horizontal overflow, no double-scrollbars, the existing page-specific test markers render.

### Conformance

- N/A — Phase 108 does not touch a driver-shaped subsystem.

### Concurrency / leak

- N/A — Phase 108 does not build a reusable runtime artifact (the Console is a SPA; concurrent state lives in Svelte stores per CONVENTIONS.md §6).

## Smoke script additions

`scripts/smoke/phase-108.sh` — `PREFLIGHT_REQUIRES: static-only`. Assertions:

1. `web/console/static/harbor_logo.svg` exists.
2. `web/console/src/lib/components/playground/KpiStrip.svelte` exists.
3. `web/console/src/lib/components/playground/PlaygroundStatusBar.svelte` exists.
4. `web/console/src/lib/chat/MarkdownInline.svelte` exists.
5. `web/console/src/lib/tokens.css` defines `--chip-info-fg`, `--chip-success-fg`, `--chip-warning-fg`, `--chip-danger-fg`, `--chip-accent-fg`, `--chip-neutral-fg` (grep — exact token names).
6. `web/console/src/routes/(console)/+layout.svelte` contains `height: 100vh` (the AC-1 fixed-viewport reshape — the inverse of the old `min-height: 100vh`).
7. `web/console/package.json` has the same `dependencies` + `devDependencies` count as on `main` (grep — no new entries; this is a static-only sanity check on the no-new-deps invariant).
8. `grep -rE 'V1 renders plain text verbatim' web/console/src/lib/chat/MessageBubble.svelte` returns 0 (the AC-16 comment update).

The smoke is intentionally static — Phase 108 does not introduce any Protocol surface, so a `live-server` smoke would have nothing to assert. The integration / regression coverage lives in the Playwright specs the preflight harness already runs as part of the frontend job.

## Coverage target

- `web/console/src/lib/chat/`: 85% (existing target). The new `MarkdownInline.svelte` + its spec lift the package's coverage; the bubble-renderer changes are covered by existing + new specs.
- `web/console/src/lib/components/playground/`: 80%. New KpiStrip + PlaygroundStatusBar each get their own spec.
- `web/console/src/lib/components/ui/`: unchanged. StatusChip's `kind` extension is covered by an extension of the existing spec.

## Dependencies

- 73n — Playground page foundation (shipped — D-130). Phase 108 reshapes its visual surface; no behavioural change.
- 105 — Console first-attach UX (shipped — D-163). Phase 108 lands on top of the post-attach Playground; an attached state is the visual baseline.
- 106 — Playground displays the real assistant response (shipped — V1.2). Phase 108 expects the bubble's text to be a real model answer (markdown-rich), not a placeholder.
- D-121 / CONVENTIONS.md — the binding Console design-system foundation (shipped). Phase 108 cites it in the "Console consistency" section below.
- 83q — Playground sidebar nav (shipped — D-159). Phase 108 keeps the entry, polishes its visual.
- 83r — Disconnected-state hygiene (shipped — D-160). Phase 108 preserves the disconnected-state behaviour wholesale.

## Console consistency

Per CONVENTIONS.md §9 — this section is mandatory for every Console page phase.

Phase 108 confirms the Playground page:

- routes under `(console)/playground/` with no `/console/` URL prefix (CONVENTIONS.md §1) — unchanged from Phase 73n.
- renders inside the app shell at `(console)/+layout.svelte` (CONVENTIONS.md §2) — the new shell-level `status-bar` slot is additive and does not break the 13 other pages.
- uses the `components/ui/` inventory (CONVENTIONS.md §3) — the existing `PageHeader`, `FilterBar`, `SavedViewChips`, `DetailRail`, `RailCard`, `Pagination`, `StatusChip`, `PageState`, `ConnectionFooter` continue to back the page. The new components (`KpiStrip`, `PlaygroundStatusBar`) live in `components/playground/` per the rule "page-specific components stay in `components/<page>/`."
- routes all async state through the four-state `<PageState>` (CONVENTIONS.md §4) — unchanged from Phase 73n.
- clears the §5 depth bar — already cleared by Phase 73n; Phase 108 does not regress any item.
- talks to the Runtime only through `HarborClient` + `connection.ts` (CONVENTIONS.md §6) — unchanged from Phase 73n. No new transport, no new fetch call site, no new error class.
- introduces no raw token literals (CONVENTIONS.md §7) — the chip palette extends `tokens.css` in place; `.stylelintrc.cjs` mechanically rejects raw literals; `npm run lint` is the gate.

## Risks / open questions

- **The in-house markdown subset may render content the LLM's output meant differently.** A `*single asterisk*` between two letters can drift between bold and italic if the parser is naive. The Vitest spec pins exact behaviour for each pattern; if an operator finds a pattern that renders surprisingly, the parser is extended in a follow-up (parser is ~120 LOC — extension is cheap). The risk is bounded by the spec's coverage.
- **Sparkline data source.** The KPI strip's mini-sparkline plots a 60-point buffer of token-delta observations. Phase 108 derives this from the existing chunk-event handler (Phase 107's emit) when available; otherwise the sparkline shows the most-recent value as a static dot until Phase 107 ships. The spec covers both branches.
- **Status-bar Protocol version.** The bar reads the protocol version from `connection.ts`. If the resolver returns null (disconnected), the chip renders `Protocol —` in `chip-neutral`. No new wire field.
- **Page-by-page polish cadence.** Phase 108 is the first of 14. The cadence (109..121, one per page) will be authored after Phase 108 ships and the layout / token / chip-palette foundation lands — the subsequent phases will each follow this template. The order is set by `docs/design/console/README.md`'s page list; the user-facing rationale ("polish each page in turn until the Console feels finished") is documented in the master plan row.
- **The mock's "Search anything ⌘K" top-right surface.** Phase 108 does not add a global command palette — that's a separate phase. The top bar's right-side area in Phase 108 carries only the scope chip + connection dot.
- **CONVENTIONS.md §2 ConnectionFooter overlap.** Phase 108's new `status-bar` slot renders ABOVE the existing `ConnectionFooter` — the footer stays the single disconnected-indicator source of truth (closes D-161 / Phase 83s). The two are visually distinct: status bar is page-content (Playground-only), footer is shell-chrome (every page).

## Glossary additions

- **KpiStrip** — the four-tile Playground header KPI strip (Tokens / Cost / p50 latency / Status). Renders inline SVG sparklines and ceiling-percent indicators using the chip palette.
- **MarkdownInline** — the in-house safe-subset markdown renderer at `web/console/src/lib/chat/MarkdownInline.svelte`. Renders headings, bullets, numbered lists, bold, italic, inline code, fenced code, paragraphs. Rejects HTML, autolinks, tables, KaTeX, Mermaid (those land with a future RFC-blessed dependency).
- **Playground status bar** — the bottom dock above `ConnectionFooter` on the Playground page (Streaming / Protocol version / Events Stream / Console build). Wires into the shell via the `status-bar` slot added in Phase 108; the 13 other Console pages render the slot empty.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation: no identity surface changes — N/A
- [ ] Concurrent-reuse test — N/A (Console SPA; no reusable runtime artifact)
- [ ] Integration test — `playground-polish.spec.ts` + `shell-no-regression.spec.ts` (Playwright); skill drift handled in same PR
- [ ] Glossary updated for `KpiStrip`, `MarkdownInline`, `Playground status bar`
- [ ] `docs/skills/drive-the-playground/SKILL.md` prose reflects the new visual surface (AC-31)
- [ ] `web/console/package.json` shows no new dependency (AC-29)

## Implementation order (suggested)

1. **Tokens first** (AC-4 / AC-5 / AC-6): extend `tokens.css` with the chip palette + `--text-2xl/3xl` + tabular-nums utility. Run `npm run lint` to confirm no raw literals.
2. **App shell reshape** (AC-1 / AC-2): make the shell fixed-height + add the `status-bar` slot. Run `shell-no-regression.spec.ts` against the 13 other pages.
3. **Brand surfaces** (AC-8 / AC-9 / AC-10): logo + scope chip + active rail.
4. **MarkdownInline** (AC-15 / AC-23 / AC-24): ~120 LOC parser + ~80 LOC spec. The XSS spec runs first; the grammar spec runs after.
5. **MessageBubble polish** (AC-14 / AC-16 / AC-17): avatar slot + bubble-head reshape + comment update.
6. **Header restructure** (AC-11): the agent-name + status pill + planner pill row.
7. **KpiStrip** (AC-12 / AC-13): four tiles + the sparkline + the topology-info chip.
8. **Right rail polish** (AC-18 / AC-19 / AC-20): Controls / Pending Interventions / Recent Artifacts.
9. **Composer polish** (AC-22).
10. **PlaygroundStatusBar** (AC-21).
11. **Page-level wiring** in `playground/[session_id]/+page.svelte`: slot in the new components + remove the full-width topology banner.
12. **Playwright** `playground-polish.spec.ts` + `shell-no-regression.spec.ts`.
13. **Smoke** `scripts/smoke/phase-108.sh`.
14. **Skill drift** (AC-31).
15. **Glossary** (three entries).
16. **`make drift-audit && make preflight`** — both green.
17. Open PR.

