# Phase 73n — Console Playground page + chat module first consumer + `runs.set_overrides`

## Summary

Ships THREE bundled deliverables: (1) the shared chat module at `web/console/src/lib/chat/` per D-091's "encapsulate first, extract on second consumer" — the Playground is the first consumer; the future packed dev UI in `harbor dev` (post-V1) is the second, (2) the `runs.set_overrides` Protocol method enabling Playground's reasoning-effort / temperature / system-prompt-override controls per Brief 11 §PG-5, (3) the Playground page UI: chat-style stream + composer + right-rail Controls / Pending Interventions / Recent Artifacts + trace toggle (consuming Phase 74 `topology.snapshot`).

## RFC anchor

- RFC §5.1 (Protocol layer general)
- RFC §6.4 (Tools)
- RFC §6.13 (typed event bus)
- RFC §7 (Console layer)
- RFC §7.4 (CLI / dev surface)

## Briefs informing this phase

- brief 11 (Console feature surface — "Playground view", §PG-1 chat surface, §PG-2 multimodal upload, §PG-3 MCP-Apps compliance, §PG-4 rich-output renderers, §PG-5 drift/overrides)
- brief 12 (deployment + two-surface model — "shared chat / playground library — encapsulate first, extract on second consumer")

## Brief findings incorporated

- brief 11 §"Playground view": the Playground is a real Harbor session — every message round-trips through the SHIPPED `user_message` Protocol method (Phase 54); the runtime emits real events (`tool.invoked` etc.); the page composes those. NO parallel chat protocol.
- brief 11 §PG-5: reasoning-effort / temperature / system-prompt-override are runtime parameters; the Console invokes them via a NEW `runs.set_overrides` Protocol method. Drift-mode toggle is `[deferred-post-V1]` per the brief — visible-but-disabled with a "Post-V1" tooltip.
- brief 12 §"shared chat / playground library": the chat module lives at `web/console/src/lib/chat/` with a typed `ProtocolClient` interface injected by the caller (no Console-private singleton). The MCP-Apps renderer registry lives at `web/console/src/lib/chat/renderers/` per D-062.

## Findings I'm departing from (if any)

None.

## Goals

- Ship the chat module as a self-contained package at `web/console/src/lib/chat/` with: chat-stream renderer, message bubbles (user / agent), tool-call trace cards, diff-view cards, artifact-reference cards, code blocks via Shiki, Markdown + GFM + KaTeX + Mermaid renderers per Brief 11 §PG-4, multimodal upload + voice input + token-count preview composer.
- **Extend** the canonical MCP-Apps renderer registry at `web/console/src/lib/chat/renderers/` per D-062 — the ONLY path for MCP-sourced tool output rendering. The **registry SKELETON is shipped by Phase 73l (Stage 2.1)**: dispatch table (`index.ts`) + mime renderers (markdown, code, image, pdf, audio, json). 73n Playground (Stage 2.3) lands AFTER 73l per Wave 13 staging, so this phase EXTENDS 73l's directory with chat-bubble / tool-call / diff / artifact-reference renderers using the same dispatch contract — it does NOT re-ship the dispatcher. Bespoke per-server renderers are §13-forbidden.
- Ship 1 NEW Protocol method: `runs.set_overrides` (reasoning effort / temperature / system-prompt override / max-tokens for the next message in a session). Scoped to the operator's session identity.
- Ship the Playground page UI matching `docs/rfc/assets/console-playground-page.png`.
- Trace toggle consumes Phase 74's `topology.snapshot`.
- Per-page Playwright spec covering chat-stream round-trip, multimodal upload, reasoning-effort override, intervention Approve / Reject, artifact preview.

## Non-goals

- Drift mode (Brief 11 §PG-5 deferred — Post-V1). Toggle renders visible-but-disabled.
- Voice-output (server-side TTS). Voice-INPUT is shipped; voice-output is post-V1.
- Multi-turn session forking. Post-V1.
- Cross-session message replay. Post-V1.

## Console consistency

This is a Console page phase. It is **binding** on the shared Console
design-system foundation defined in `docs/design/console/CONVENTIONS.md`
(D-121 in `docs/decisions.md`). `CONVENTIONS.md` is the cross-cutting
authority for every Console page; a page PR that diverges from a convention
below is **rejected on sight**. The Playground reuses the shared chat module (`web/console/src/lib/chat/`, D-091) and still mounts inside the shared app shell and routes all async state through the four-state `<PageState>`; the chat module talks to the Runtime only through the injected `HarborClient`/`ProtocolClient`, never a hand-rolled `fetch`.

The page MUST:

- **Route under `(console)/`.** The page lives at
  `web/console/src/routes/(console)/playground/` and is served at `/playground` with
  **no `/console/` URL prefix** (the `(console)` route group is a
  layout-grouping device and does not appear in the URL). Detail views live at
  `(console)/playground/[id]/` and are served at `/playground/<id>`. All inter-page
  links use the unprefixed form; a link to `/console/<anything>` is a bug.
- **Render inside the shared app shell.** The page renders as a child of
  `(console)/+layout.svelte` — the single app shell carrying the sidebar,
  breadcrumb, identity/connection indicator, and footer. It never ships a
  standalone layout.
- **Use the shared `components/ui/` inventory.** It composes the cross-page
  primitives in `web/console/src/lib/components/ui/` — `PageHeader`,
  `FilterBar`, `DataTable`, `DetailRail`/`RailCard`, `BulkActionBar`,
  `SavedViewChips`, `Pagination`, `StatusChip`, `ConnectionFooter`,
  `PageState`. It **never forks a primitive that already exists**;
  page-specific components go in `components/playground/`.
- **Route all async state through the four-state `<PageState>`.** Every async
  surface flows through `<PageState>`'s four mutually-exclusive states —
  Disconnected / Loading / Error / Empty. The Error state ships a working
  **Retry** that re-invokes the loader and suppresses any stale primary view;
  **Disconnected** ("no Runtime attached") is detected via `connection.ts`
  returning `null` and is **never conflated with Error**.
- **Clear the §5 depth bar.** The page is not "done" until it has all of:
  a `PageHeader`; a `FilterBar`; a primary `DataTable` or canvas; a
  `DetailRail` or a tabbed detail route; Console-DB-backed `SavedViewChips`;
  real `Pagination` (page / size / total, prev / next — not a fake "load
  more"); a `ConnectionFooter`; and the full four-state `PageState`.
- **Talk to the Runtime only through `HarborClient` + `connection.ts`.** All
  Protocol calls go through the single typed `HarborClient` (adding a
  namespace, never a new top-level client); the connection resolves through
  `web/console/src/lib/connection.ts`. **No `fetch` in `.svelte` files, no
  direct `localStorage` access, no hand-rolled per-page client.**
- **Introduce no raw token literals.** No raw color / spacing / type-scale
  literals in `.svelte` files — design tokens from `tokens.css` only
  (Stylelint enforces this; `npm run lint` fails CI on a violation).
- **Ship no stubbed action presented as done.** Every action either invokes
  the real Protocol method or renders **disabled-with-tooltip** explaining
  why. A button that fakes success with a feedback string is a §13-class
  silent-degradation violation.

See `docs/design/console/CONVENTIONS.md` §9 for the per-phase callout
contract and D-121 for the rationale.

## Acceptance criteria

- [ ] `web/console/src/lib/chat/` ships as a self-contained module: (a) no imports of other Console internals from inside the chat module; (b) a typed `ProtocolClient` interface that the caller injects (never a Console-private singleton); (c) the renderer registry directory at `web/console/src/lib/chat/renderers/` already EXISTS (shipped by Phase 73l with dispatch table + mime renderers); this phase adds chat-bubble renderers to that directory under the same dispatch contract.
- [ ] Chat-bubble renderers ADDED to 73l's registry (Phase 73l ships the dispatch table + mime renderers: markdown / code / image / pdf / audio / json with their `marked` / `katex` / `mermaid` / `shiki` / image / pdf-viewer / audio-waveform / json-tree implementations). 73n adds: tool-call trace card renderer, diff-view renderer, artifact-reference card renderer.
- [ ] `internal/protocol/methods/methods.go` declares `runs.set_overrides`.
- [ ] `internal/protocol/types/runs.go` defines `RunOverrides` (reasoning effort, temperature, max tokens, system prompt override) — single source of truth (D-002).
- [ ] `runs.set_overrides` enforces identity-mandatory (`session_id` required; tenant/user inferred from auth); the override applies to the NEXT message in the session, not retroactively.
- [ ] Playground page UI (`web/console/src/routes/playground/[session_id]/+page.svelte`) renders: header with breadcrumb / agent picker / model badge / token-count chip / cost chip / Cancel-run / Restart buttons; main canvas chat-style stream; bottom composer with multimodal attach + textarea + voice input + Send (Cmd-Enter); right rail with Controls / Pending Interventions / Recent Artifacts; footer with streaming indicator + Protocol/Console versions.
- [ ] Every message invokes the SHIPPED `user_message` Protocol method (Phase 54) — NO parallel chat protocol.
- [ ] Cancel-run invokes SHIPPED `cancel` (`Payload.hard` toggles on Cmd-Shift-Backspace).
- [ ] Restart invokes SHIPPED `start` with the same agent + system prompt.
- [ ] Multimodal attach uploads via `artifacts.put` (Phase 73l surface) and auto-includes the resulting `ArtifactStub` in the next message.
- [ ] Drift-mode toggle in Controls is visible-but-disabled with a "Post-V1" tooltip per Brief 11 §PG-5.
- [ ] Approve / Reject in Pending Interventions invoke SHIPPED `approve` / `reject` (Phase 54).
- [ ] Trace toggle consumes Phase 74 `topology.snapshot` for the active run.
- [ ] No Priority field anywhere (D-065).
- [ ] All data flows through the typed Protocol client (D-093). NO hand-rolled `fetch`.
- [ ] Design tokens only (§13). Skeleton component primitives used per CLAUDE.md §4.5 rule 4; any custom wrappers are justified in the PR description.
- [ ] `svelte-check --fail-on-warnings` passes (D-092).
- [ ] Per-page Playwright spec at `web/console/tests/playground-page.spec.ts` covers: chat-stream round-trip, multimodal upload, reasoning-effort override applied to next message, intervention Approve / Reject scope-claim degradation, artifact preview.
- [ ] **"Run as identity" selector in the header consumes 72b's `IdentityScope` triplet** (Brief 11 §PG-5). When the operator has the `auth.ScopeAdmin` claim, the header renders a "Run as identity" dropdown that surfaces tenants / users / sessions the admin can impersonate; selecting a triple populates `IdentityScope.Impersonating` on the next `user_message` / `start` Protocol call. Operators WITHOUT `auth.ScopeAdmin` do NOT see the selector (it renders as absent, not disabled — minimizes UI clutter for non-admin operators). This satisfies 72b's binding cross-reference and lands the consumer alongside the primitive in the same wave (§13 primitive-with-consumer).
- [ ] `scripts/smoke/phase-73n.sh` asserts `runs.set_overrides` round-trip + chat module assets served.
- [ ] **Chat module encapsulation invariant verified by grep:** no `from '$lib/'` imports inside `web/console/src/lib/chat/` that reach OUTSIDE the chat module (typed via tsconfig path aliases — only `$lib/chat/*` allowed within).
- [ ] **Concurrent-reuse test:** N≥100 concurrent `runs.set_overrides` calls against shared session state under `-race` (D-025).
- [ ] **Integration test:** `test/integration/playground_overrides_test.go` — real runtime + `runs.set_overrides` round-trip + next-message override application; under `-race`.

## Files added or changed

```text
internal/protocol/methods/methods.go                     # +runs.set_overrides
internal/protocol/types/runs.go                          # +RunOverrides
internal/protocol/transports/stream/runs_handler.go
internal/protocol/transports/stream/runs_handler_test.go
internal/runtime/runs/protocol/overrides.go
internal/runtime/runs/protocol/overrides_test.go
internal/runtime/runs/protocol/concurrent_reuse_test.go
test/integration/playground_overrides_test.go
web/console/src/lib/chat/index.ts                        # public exports — only ProtocolClient interface + ChatPanel + ChatComposer
web/console/src/lib/chat/types.ts                        # ProtocolClient typed interface (injected by caller)
web/console/src/lib/chat/ChatPanel.svelte
web/console/src/lib/chat/ChatComposer.svelte
web/console/src/lib/chat/MessageBubble.svelte
web/console/src/lib/chat/ToolCallTraceCard.svelte
web/console/src/lib/chat/DiffViewCard.svelte
web/console/src/lib/chat/ArtifactReferenceCard.svelte
web/console/src/lib/chat/CodeBlock.svelte                # uses Shiki
web/console/src/lib/chat/StreamingIndicator.svelte
web/console/src/lib/chat/renderers/tool_call_trace.ts    # NEW renderer (chat-bubble specific); dispatch table at index.ts shipped by 73l
web/console/src/lib/chat/renderers/diff_view.ts          # NEW renderer (chat-bubble specific)
web/console/src/lib/chat/renderers/artifact_reference.ts # NEW renderer (chat-bubble specific)
# NOTE: index.ts + markdown.ts + code.ts + image.ts + pdf.ts + audio.ts + json.ts are shipped by Phase 73l (Stage 2.1). This phase EXTENDS that directory; the dispatch table is registered via 73l's exported register() helper, not re-shipped.
web/console/src/routes/playground/[session_id]/+page.svelte
web/console/src/lib/components/playground/Header.svelte
web/console/src/lib/components/playground/ControlsCard.svelte    # reasoning effort / temperature / max-tokens / system-prompt-override + drift-mode (disabled)
web/console/src/lib/components/playground/PendingInterventionsCard.svelte
web/console/src/lib/components/playground/RecentArtifactsCard.svelte
web/console/src/lib/components/playground/TraceToggle.svelte    # consumes topology.snapshot from Phase 74
web/console/tests/playground-page.spec.ts
web/console/src/lib/protocol.ts                                  # REGENERATED via make protocol-ts-gen
scripts/smoke/phase-73n.sh
docs/glossary.md                                                 # +runs.set_overrides, +RunOverrides, +"chat module", +"MCP-Apps renderer registry"
```

## Public API surface

```go
// internal/protocol/types/runs.go
type RunOverrides struct {
    SessionID            string
    ReasoningEffort      *string  // optional — values per LLM provider taxonomy
    Temperature          *float64 // optional
    MaxTokens            *int     // optional
    SystemPromptOverride *string  // optional
}

type RunSetOverridesRequest struct {
    Overrides RunOverrides
}

type RunSetOverridesResponse struct {
    AppliedAt time.Time
}
```

## Test plan

- **Unit:**
  - `runs/protocol/overrides_test.go` — identity rejection, scope-claim gating, override application to the next message only (not retroactively).
- **Integration:**
  - `test/integration/playground_overrides_test.go` — real runtime + `user_message` + `runs.set_overrides` round-trip. Override applied to next message, NOT to past messages. Cross-session override rejected.
- **Conformance:**
  - `runs.set_overrides` runs against the Protocol conformance suite.
- **Concurrency / leak:**
  - `concurrent_reuse_test.go` — N=100 concurrent `runs.set_overrides` on a shared session-state map under `-race`.
- **UI (Playwright):**
  - `playground-page.spec.ts` — chat-stream round-trip; multimodal upload via `artifacts.put`; reasoning-effort slider invokes `runs.set_overrides`; intervention Approve / Reject scope-claim degradation; trace toggle reveals `topology.snapshot`; Cancel-run invokes `cancel`; Restart invokes `start`.

## Smoke script additions

`scripts/smoke/phase-73n.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'runs/set_overrides' '{"overrides": {"session_id": "sess-fix", "reasoning_effort": "high"}}'` → round-trips.
- `protocol_call 'runs/set_overrides' '{"overrides": {"session_id": "cross-tenant-sess"}}'` → 403 (identity scope).
- Page route /console/playground/<session-id> — SKIPped until 73m's `harbor console` lands.

## Coverage target

- `internal/runtime/runs/protocol`: 85%.
- `internal/protocol/transports/stream`: 80%.
- `web/console/src/lib/chat/`: 80% (high — this module ships for 2 consumers).
- `web/console/src/routes/playground/`: 70%.

## Dependencies

**Same-wave (Wave 13):**

- Phase 72 (events.subscribe scope foundation)
- Phase 72b (`IdentityScope` admin-impersonation extension — "Run as identity" header selector)
- Phase 74 (topology.snapshot — trace toggle)
- **Phase 73l (Stage 2.1; ships the canonical renderer registry SKELETON + mime renderers at `web/console/src/lib/chat/renderers/`. This phase EXTENDS that directory with chat-bubble renderers under the same dispatch contract. Also supplies `artifacts.put` for the multimodal upload pipeline.)**
- Phase 75 (Playwright harness baseline)

**Already shipped (pre-Wave 13):**

- Phase 54 (Protocol task control surface — `Shipped`; supplies `start`, `cancel`, `approve`, `reject`, `user_message`)
- Phase 60 (Protocol wire transport — `Shipped`)
- Phase 61 (Protocol auth — `Shipped`)

## Risks / open questions

- **Chat module encapsulation drift.** The module's encapsulation invariant (no imports outside `$lib/chat/`) is enforced by grep at PR time + by tsconfig path alias config. If a developer needs a shared utility (e.g., a date formatter), the right path is to add it INSIDE the chat module's own utils, not to reach outside. Coordinator MUST grep this before merge.
- **`runs.set_overrides` next-message semantics.** The override applies to the next `user_message` only. If the operator sets an override and then cancels without sending, the override is dropped. Documented behavior; test asserts it.
- **Shiki bundle size.** Shiki ships every grammar — bundle weight is ~5MB without tree-shaking. The chat module imports only `light` + `dark` themes + the top 20 languages. Custom languages register lazily per Brief 11 §PG-4 footnote.
- **Mermaid + KaTeX render performance.** Heavy diagrams / equations can block the main thread. Renderers run in a Web Worker per Brief 11 §PG-4 recommendation; falls back to disabled-with-loading if the worker is unavailable.
- **Multi-runtime Playground.** D-091 says every runtime is a remote peer. The Playground's `<session-id>` URL anchors to ONE runtime; switching runtimes via the Connected-Runtimes card (Phase 73m) requires reloading the page. Acceptable for V1.

## Glossary additions

- **`runs.set_overrides`** — Protocol method applying reasoning effort / temperature / max-tokens / system-prompt-override to the next message in a session. Scoped to the session's identity.
- **`RunOverrides`** — Protocol projection of next-message override parameters.
- **chat module** — Self-contained Console SvelteKit module at `web/console/src/lib/chat/` shipping the chat panel, composer, renderers, and typed `ProtocolClient` interface. First consumer: Playground (this phase). Second consumer (post-V1): packed dev UI in `harbor dev`.
- **MCP-Apps renderer registry** — Canonical render-dispatch table at `web/console/src/lib/chat/renderers/` per D-062. The ONLY path for MCP-sourced tool output rendering.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` passes
- [ ] `svelte-check --fail-on-warnings` passes
- [ ] `npm run lint` passes in `web/console/`
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation paths changed — `runs.set_overrides` touches identity; integration test asserts cross-session rejection
- [ ] **Chat module encapsulation invariant verified** — grep finds no imports outside `$lib/chat/` from inside the chat module (typed via tsconfig path aliases + manual grep at PR review)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `runs.set_overrides` against shared session-state under `-race` (D-025)
- [ ] **Integration test passes** — `playground_overrides_test.go` (§17)
- [ ] **Per-page Playwright spec lands in this phase's PR**
- [ ] Glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review
