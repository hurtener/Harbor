# Console page — Playground

**Slug:** `playground` &middot; **Sidebar cluster:** session-level surface (not a sidebar entry) &middot; **Route:** `/console/playground/<session-id>` or `/console/live-runtime?playground=1&session=<session-id>` (Wave 13 to pick)
**Mockup:** `docs/rfc/assets/console-playground-page.png` (canonical, 2026-05-18)

## 1. Purpose

Playground is the operator's chat-style interactive surface — the page-shape Brief 11 §PG-1 through §PG-7 describes. It is a session-level surface, not a sidebar entry: every Playground session is a Harbor session (Phase 8) producing canonical events / artifacts identically to a programmatic invocation. The Playground is a Protocol *client*, never a runtime side channel. The operator opens it to: hand-craft a multi-turn conversation with a configured agent; upload images / PDFs / audio / video / arbitrary binaries as multimodal inputs; inspect tool-call traces inline; preview MCP-Apps content rendered by the canonical renderer registry; toggle reasoning summaries; compare two agents / models side-by-side; turn a Playground session into a Live Runtime workbench session with one click. Brief 11 §"Playground" calls this the "most architecturally significant extension" Brief 11 flags beyond the original mockup; this spec extends that brief into a Wave 13 mockup-authoring brief.

## 2. Where it sits in the IA

Playground is NOT a top-level sidebar entry — it is a session-level surface, reached from: an Agent detail's "Open in Playground" button (Brief 11 §"Agents view"), a Live Runtime composer's "Open as chat" toggle (one of the panels in the Live Runtime workbench is Playground-shaped per D-062), a fresh "Start chat" CTA on the Overview's quick links, or a deep-link from a Session detail's "Continue as chat" action. Per D-091 the chat + playground + MCP-Apps renderer + file-upload + trace-toggle components ship as a self-contained module at `web/console/src/lib/chat/`. Breadcrumb: `<runtime> / Playground / <session-id>` OR (in the Live-Runtime-embedded variant) `<runtime> / Live Runtime / <session-id>` with a Playground tab active.

## 3. Functionality matrix

- **PG-1 — Conversation surface — message list + composer, roles (user / assistant / tool / system / agent-internal), streaming tokens render live as they arrive, reasoning summaries / tool-call traces inline-collapsible per message, per-message timestamps / model / token usage / cost.** `[shipped]` Spawns the session via `start` Protocol method (`types.StartRequest`); subscribes via `events.EventBus` filtered to the session for live deltas (`planner.decision`, `planner.finish`, `tool.invoked`, `tool.completed`, `tool.failed`, `llm.cost.recorded`). The user's reply submits via `user_message` Protocol method (`types.ControlRequest` with `Payload.message`).
- **PG-2 — File upload (multimodal input) — drag-and-drop + paste + file-picker for images (PNG/JPG/WEBP/HEIC), PDFs, audio (MP3/WAV/M4A), video (MP4/WEBM), arbitrary binary.** `[wave-13-extends]` Files become `ArtifactRef`s in the session's artifact store (Phases 17–19, `Shipped`); upload via the artifact store — but a Console-facing `artifacts.put` Protocol method is NEW (Phase 73 spec'd `artifacts.list` / `artifacts.get` / `artifacts.get_ref` / `artifacts.delete` only). Wave 13 must add `artifacts.put`. LLM-edge translation (Phase 33 / D-021) maps `ArtifactRef`s to multimodal `ContentPart`s for the LLM per `Multimodality V1 inputs`; Phase 32 safety net (D-026) catches if a Playground submission ever has raw bytes reaching the LLM-client edge.
- **PG-3 — Full MCP-Apps compliance — canonical renderer registry for `string` (markdown + code highlight + link preview) / `ImageRef` (inline image) / `AudioRef` (audio player) / `LinkRef` (preview card) / `EmbeddedRef` (recursive dispatch).** `[shipped]` Renderers live at `web/console/src/lib/chat/renderers/` per D-091; subscribe to `tool.completed` (`tools.ToolCompletedPayload`) which carries the typed `ToolResult.Value`. Forbidden: bespoke per-MCP-server renderers — every MCP server's output flows through these canonical shapes (Brief 11 §PG-3).
- **PG-4 — Rich output rendering — Markdown (CommonMark + GFM + tables + task lists + footnotes + KaTeX + Mermaid), code blocks with highlight.js / Shiki + per-block copy, JSON / YAML / TOML with collapsible tree, CSV / TSV with sortable table, tool-call traces as collapsible cards, streaming indicators, citations, artifact references as preview cards, diff view.** `[wave-13-extends]` Renderer mechanism is Console-local (shipped); the artifact-reference preview cards depend on `artifacts.get_ref` (NEW Phase 73 Protocol method).
- **PG-5 — Controls + tooling — model selector (from agent's allowed list), reasoning effort slider, tool toggle (temporarily disable a tool for this session), temperature / top-p (dev mode), system prompt override (gated), "Run as another identity" (admin-only impersonation), save / share / fork session, export transcript.** `[wave-13-extends]` Most controls submit via `user_message` extended payload or via a NEW `runs.set_overrides` Protocol method that accepts model / temperature / system-prompt / tool-toggle overrides for the next planner step. Wave 13 to scope.
- **PG-5 (impersonation) — admin only — `actor=admin, requester=admin, impersonating=user_id` captured on the request.** `[wave-13-extends]` Requires identity-impersonation field on `types.StartRequest` (NEW field) or a new `types.IdentityScope` extension; fully audited via the existing `audit.admin_scope_used` event.
- **PG-6 — Side-by-side comparison — two Playgrounds with the same input, different agents / models / planners.** `[deferred]` Brief 11 §PG-6 foreshadows Evaluations (D-064, post-V1). The Playground page mockup may gesture as a "Compare" affordance but the back-end comparison UX is post-V1.
- **PG-7 — Trace toggle — overlay topology inline with chat messages.** `[wave-13-extends]` Requires the Phase 74 topology projection events (currently `Pending`); see page-live-runtime.md §3. The trace toggle is the same `topology.snapshot` projection rendered as inline mini-topology cards per chat message.
- **Cancel run mid-stream.** `[shipped]` `cancel` Protocol method.
- **Pause run mid-stream.** `[shipped]` `pause` Protocol method.
- **Approve / Reject pending pauses (HITL / tool-side OAuth / tool-side approval) inline in the chat.** `[shipped]` Subscribe to `pause.requested` / `tool.approval_requested` / `tool.auth_required` filtered to session; resolve via `approve` / `reject` / `resume` Protocol methods.
- **Redirect a Playground run.** `[shipped]` `redirect` Protocol method (`types.ControlRequest` with `Payload.goal`).
- **Cost / ceiling indicator — live tally of cost spent in this Playground session, with a warning when nearing the operator's identity ceiling (Phase 36a).** `[shipped]` Subscribe to `llm.cost.recorded`; cross-check against `governance.budget_exceeded` events for ceiling proximity.
- **MCP `DisplayMode` honoring — `inline` (chat-scroll widget), `fullscreen` (new tab within the agent/session view; multiple fullscreen apps yield multiple tabs), `pip` (split-screen, default 50/50, resizable) per D-062.** `[wave-13-extends]` The wire field exists per D-062 in `internal/protocol/types/`, but a Console-side honoring renderer is part of the chat module's Wave 13 scope.
- **Drift mode (fork a session, edit a past user message, re-play).** `[deferred]` Brief 11 §PG-5; post-V1 (foreshadows Evaluations, D-064).
- **No Priority field rendered on session metadata.** `[deferred]` D-065 dropped session-level priority from V1.
- **Playground is NOT served by `harbor dev` directly.** `[shipped]` Per D-091, the Console (including the Playground) is served by `harbor console` subcommand via `embed.FS`, NOT `harbor dev`. A future packed dev UI in `harbor dev` (post-V1) will reuse the chat module via the `web/shared/chat/` extraction — but the V1 Playground lives in the full Console.
- **No anonymous Playground sessions.** `[shipped]` Per Brief 11 §"Constraints on the Playground": identity is mandatory; Playground sessions carry the operator's identity (or the impersonated identity).
- **No free dev mode — Playground sessions count against identity ceilings.** `[shipped]` Per Brief 11 §"Constraints on the Playground" + D-022 + Phase 36a.

## 4. Page anatomy

- **Sidebar** (shared, when reached as a standalone session-level page); when embedded inside Live Runtime, the Live Runtime's panel structure surrounds it.
- **Top bar** (shared).
- **Main canvas** (per-page, standalone mode):
  - Row 1 — session header (session id + agent picker + status badge + cost rollup + control buttons: Pause / Cancel / Redirect).
  - Row 2 — conversation message list (virtualised, scroll-to-bottom on new message; per-message renderer dispatch via the chat module).
  - Row 3 — composer (text + file-upload drop zone + model selector + reasoning effort + tool toggles + system-prompt override gate + "Run as identity" gate).
- **Right rail** (per-page): Controls panel (model selector / reasoning / tool toggles / temperature in dev mode); Pending interventions list (Approve / Reject inline); Recent Artifacts (3 newest produced in session, deep-link to Artifacts page).
- **Bottom dock** (per-page): empty (the Live-Runtime-embedded variant uses the bottom dock for the Event Stream).
- **Footer** (shared).

When Playground is embedded inside Live Runtime, the message list takes the place of the topology canvas while the rest of Live Runtime's right rail + bottom dock stay; the operator toggles between Topology and Playground at the page's tab strip.

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Session header | `sessions.inspect` (NEW Phase 73 method) + live cost aggregation | Copy session id; Pause / Cancel / Redirect buttons → method calls | `[wave-13-extends]` |
| Message list (per-message renderers) | `planner.decision` / `planner.finish` events + `tool.invoked` / `tool.completed` / `tool.failed` event payloads; rich content via the canonical renderer registry | Click tool-call card → expand trace; click artifact reference → Artifacts page; click citation → external | `[shipped]` |
| Composer | local input + `start` (first turn) / `user_message` (subsequent turns) Protocol methods | Submit (Cmd-Enter) | `[shipped]` |
| File upload (drop zone) | `artifacts.put` (NEW for Console-facing upload) | Drag-and-drop / paste / file-picker; thumbnail preview before send | `[wave-13-extends]` |
| Model selector | local + agent's allowed model list from `agents.get` (NEW) | Set (local UI state); applied via `runs.set_overrides` (NEW) on next turn | `[wave-13-extends]` |
| Reasoning effort slider | local UI state + `runs.set_overrides` (NEW) | Set | `[wave-13-extends]` |
| Tool toggles | local UI state + `runs.set_overrides` (NEW) per-tool override | Set | `[wave-13-extends]` |
| Temperature / top-p (dev mode) | local UI state + `runs.set_overrides` (NEW) | Set | `[wave-13-extends]` |
| System-prompt override | local UI state + `runs.set_overrides` (NEW) + gated on agent permits | Set; admin-gated | `[wave-13-extends]` |
| "Run as identity" (impersonation) | extended `types.IdentityScope` (NEW impersonation field) | Set; admin-only; emits `audit.admin_scope_used` | `[wave-13-extends]` |
| Cancel button | `cancel` Protocol method | Submit | `[shipped]` |
| Pause / Resume buttons | `pause` / `resume` Protocol methods | Submit | `[shipped]` |
| Redirect composer | `redirect` Protocol method | Submit | `[shipped]` |
| Pending interventions list (right rail) | `pause.requested` / `tool.approval_requested` / `tool.auth_required` events filtered to session | Approve → `approve`; Reject → `reject`; Resume → `resume` | `[shipped]` |
| Recent Artifacts (right rail) | `artifacts.list` (NEW Phase 73 method) filtered to session, sorted newest, cap 3 | Click → preview / Artifacts page | `[wave-13-extends]` |
| Cost rollup indicator | `llm.cost.recorded` aggregation + `governance.budget_exceeded` events | Click → Settings → Governance posture | `[shipped]` |
| MCP-Apps `inline` widget | `tool.completed` payload's typed content; renderer registry | Renderer-dispatched (inert content; if interactive form, posts back via `user_message` (shipped) or a follow-up `tools.invoke` (NEW)) | `[wave-13-extends]` |
| MCP-Apps `fullscreen` tab | same | Tab opens within the session view per D-062 | `[shipped]` |
| MCP-Apps `pip` split | same | Split panel, resizable | `[shipped]` |
| Trace toggle | `topology.snapshot` events (NEW Phase 74) | Toggle on/off; renders mini-topology per message | `[wave-13-extends]` |
| Export transcript | client-side aggregation | Submit → file download (Markdown / JSONL) | `[shipped]` |

## 6. Controls + actions

- **Toolbar (session header):** Pause / Cancel / Redirect; Copy session id; Open in Live Runtime workbench (deep-link).
- **Composer-action:** Submit (Cmd-Enter); attach files; switch model; adjust reasoning / tool toggles / temperature (dev); override system prompt (gated); switch identity (admin impersonation).
- **Message-action:** expand reasoning summary; expand tool-call trace; copy message; quote-reply.
- **Intervention-row-action (right rail):** Approve / Reject / Resume inline.
- **Keyboard shortcuts:** `Cmd-Enter` Submit; `Cmd-/` show shortcuts; `Esc` cancel composer focus; `j` / `k` next / previous message; `t` toggle trace; `Cmd-K` global search.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Fresh session — empty | Just opened, no first message yet | Empty conversation panel + composer placeholder "Type a message or attach a file" | Compose and submit |
| Initial loading | `start` in flight | Composer disabled; spinner; "Spawning session…" | Auto |
| Streaming | Token stream open | Live cursor on the latest assistant message; "Stop" button replaces "Send" | Click "Stop" → `cancel` |
| Protocol error — `CodeNotFound` on session | Session id missing | "Session not found"; "Start new session" link | Click link |
| Protocol error — `CodeScopeMismatch` on impersonation | Operator submitted "Run as identity" without `admin` | Inline error on the identity selector | Request admin scope |
| Protocol error — `CodePayloadInvalid` on user message | Text > RFC §6.3 bound (4096 chars / 16 KiB) | Inline error on composer | Edit |
| Protocol error — `CodeIdentityRequired` on file upload | Identity tuple missing | Inline error on drop zone | Re-attach |
| Protocol error — `CodeAuthRejected` | JWT expired | Banner + re-auth | Re-enter passphrase |
| File upload too large | Exceeds artifact-store cap | Inline error: "File too large — split or compress" | Reduce |
| Cost ceiling reached | `governance.budget_exceeded` event observed | Banner: "Cost ceiling reached — composer disabled until ceiling reset" + link to Settings | Visit Settings |
| Mock LLM banner | Runtime booted with `--mock` per D-089 | Yellow ribbon: "[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]" | Switch runtime |

## 8. Multi-tenant / multi-runtime nuances

A Playground session is tied to the operator's identity (or impersonated identity for admins). Default scope keeps every Playground session inside `(tenant, user, session)` per CLAUDE.md §6 rule 1; `admin` enables impersonation per Brief 11 §PG-5 (`actor=admin, requester=admin, impersonating=user_id`) with full audit emit. In multi-runtime mode, switching the active runtime via the sidebar switcher per D-091 closes the current Playground session (sessions don't migrate across runtimes) and lands the operator back on a fresh "pick an agent" CTA. The Playground itself is served by the same chat module the future packed dev UI will reuse (`web/shared/chat/` after extraction per D-091); in V1 it lives at `web/console/src/lib/chat/`. Important: the Console is served by `harbor console` subcommand via `embed.FS`, NOT `harbor dev` — the Playground is not directly accessible from a `harbor dev` browser session in V1.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — start / continue / cancel / pause / redirect own Playground sessions; manage own message stream; upload artifacts; approve / reject pending pauses on own sessions.
- `admin` (`auth.ScopeAdmin`) — impersonate another identity ("Run as identity"); override system prompt (when the agent permits); fan out comparison views.
- `console:fleet` (`auth.ScopeConsoleFleet`) — post-V1 cross-runtime comparison.
- **Control-plane verbs (Approve / Reject / Resume on interventions, Hard Cancel)** require the more-elevated control claim per D-066 — strictly higher than ordinary identity scope; the Console hides the buttons when the JWT lacks it, and the Protocol re-checks server-side (`CodeScopeMismatch`).

## 10. Out of V1 (deferred)

- **Side-by-side comparison (PG-6).** Brief 11 §PG-6 — foreshadows Evaluations (D-064, post-V1). Mockup may gesture as a "Compare" affordance.
- **Drift mode (PG-5).** Brief 11 §PG-5 — post-V1; foreshadows Evaluations.
- **Free dev mode that bypasses ceilings.** Forbidden — Brief 11 §"Constraints on the Playground" + Phase 36a + the §13 "no test stubs as production defaults" amendment.
- **MCP-Apps interactive HTML rendering without sanitisation.** Default-deny per Brief 11 §"Open architectural questions" #8; per-source trust toggle is `[wave-13-extends]` for MCP Connections page; for Playground default behaviour is sanitised render.
- **Embedding Playground in `harbor dev` browser session.** D-091 — Console is served by `harbor console` subcommand via `embed.FS`, NOT `harbor dev`. The future packed dev UI in `harbor dev` (post-V1) will host a Playground-shaped subset using the `web/shared/chat/` module.
- **Priority field on session metadata.** D-065 dropped from V1.
- **Save Playground as an evaluation case.** D-064 — Evaluations is post-V1.

## 11. References

- Brief 11 §"Playground / direct interaction" §PG-1 through §PG-7 (the entire playground feature inventory), §"Constraints on the Playground" (identity / audit / cost / policy uniformity), §"Live Runtime view" (the chat is one panel among many, per D-062).
- Brief 12 §"The shared chat / playground library" (`web/console/src/lib/chat/` + future extraction to `web/shared/chat/`), §"Why `harbor console`, not `harbor dev`, serves the Console", §"Open architectural questions Brief 11 raised, resolved here" (session `Kind` = `production | dev`).
- RFC-001-Harbor.md §5.2 (task control + state snapshots), §6.3 (steering + pause/resume), §6.4 (Tool catalog), §6.5 (LLM client + context-window safety net), §6.8 (Sessions = multi-turn), §6.9 (SessionManager), §6.10 (Artifacts), §7 (Console).
- Decisions: D-008 (sessions are multi-turn), D-021 (multimodality inputs V1), D-022 (`ArtifactRef`), D-026 (context-window safety net), D-061 (Console DB local-only), D-062 (Live Runtime ≠ Sessions; MCP-Apps `DisplayMode`), D-064 (Evaluations post-V1), D-065 (no session priority), D-066 (control claim), D-072 (Protocol task control surface — the ten methods including `user_message`), D-083 (tool-side OAuth — `auth.BindingScope`), D-086 (tool-side approval gates), D-089 (`harbor dev` LLM-default + mock escape hatch), D-091 (Console deployment posture + shared chat module), D-092 (Svelte 5 + runes), D-093 (`protocol.ts` generated).
- Phase plan: phase 17–19 (Artifacts — `Shipped`), phase 26 (Tool catalog — `Shipped`), phase 28 (MCP southbound driver — `Shipped`), phase 30 (tool-side OAuth — `Shipped`), phase 31 (tool-side approval — `Shipped`), phase 32 (LLM client core + StreamSink — `Shipped`), phase 33 (bifrost integration + multimodality — `Shipped`), phase 36a (Cost accumulator + ceilings — `Shipped`), phase 50 (Pause/Resume Coordinator — `Shipped`), phase 52–53 (Steering — `Shipped`), phase 54 (Protocol task control surface — `Shipped`), phase 60 (Protocol wire transport — `Shipped`), phase 72 (Console subscription — `Pending`), phase 73 (state inspection — `Pending`), phase 74 (topology projection — `Pending`).
- Glossary terms used: `Console`, `Live Runtime`, `Pause/Resume Coordinator`, `DisplayMode`, `Scope claim`, `Fleet control / fleet observation`, `Runtime lens`, `tool.approval_requested`, `tool.auth_required`, `HARBOR_DEV_TOKEN`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-playground-page.png` against §3-§7.

### Refinements to §4 page anatomy

- **Header**: breadcrumb `<runtime> / Playground / <session-id>` + agent picker dropdown (e.g., "Research Agent v3.2") + model badge (e.g., "Bedrock Claude 3.5 Sonnet") + token-count chip ("12,345 tokens") + cost chip ("$4.05") + **Cancel run** + **Restart** buttons. No Priority field anywhere (D-065).
- **Main canvas — chat-style stream**: alternating user / agent message bubbles with avatars. Agent messages contain:
  - Free text (Markdown / GFM / KaTeX / Mermaid per Brief 11 §PG-4)
  - Expandable **tool-call trace cards** (showing tool name + args + duration + result snippet)
  - **Diff view cards** (when output diffs a previous artifact)
  - **Artifact-reference cards** (mime icon + filename + size + preview thumbnail + Open button → Artifacts page)
  - **Code blocks** with copy / Shiki-highlight per language
  - Streaming indicator (blinking cursor while tokens still arriving)
- **Bottom — chat input composer**: multimodal upload (image/PDF/audio attach button) + free-text textarea + voice input button + **Send** button (Cmd-Enter). Token-count preview as the operator types.
- **Right rail — three stacked cards**:
  - **Controls** card: model selector / reasoning effort slider / temperature slider / max-tokens input / system-prompt-override textarea + drift-mode toggle (deferred — D-065-style "post-V1" tooltip)
  - **Pending Interventions** card (mirrors Live Runtime's shape): Approve / Reject with reason + countdown.
  - **Recent Artifacts** card (mime icons + size + age, capped to 3-5).
- **Footer**: `Streaming | last bucket <X tokens/s> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`.

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Token-count chip (header) | live aggregation of `llm.cost.recorded` and chat-stream token deltas | Click → drill into cost breakdown | `[shipped]` (events) |
| Cost chip (header) | `llm.cost.recorded` aggregated for this session | Click → drill into Settings → Governance | `[shipped]` |
| Restart button (header) | spawns a fresh `start` with the same agent + system prompt | Confirm → `start` Protocol method | `[shipped]` |
| Cancel-run button (header) | `cancel` Protocol method (soft default; Cmd-Shift-Backspace = hard) | Submit → `cancel` (`Payload.hard` toggles) | `[shipped]` |
| Drift-mode toggle (right-rail Controls) | local UI flag (deferred surface) | Toggle (no-op in V1; tooltip "Post-V1") | `[deferred]` (Brief 11 §PG-5 — drift is post-V1) |
| Reasoning-effort slider | local UI state → `runs.set_overrides` (NEW) | Slide → submit override on next message | `[wave-13-extends]` |
| Token-count preview (input box) | local tokenizer estimate | Live update as operator types | `[shipped]` (local) |
| Multimodal attach button | local file → Artifact upload via `artifacts.put` (NEW Phase 73) | Submit → artifact id + auto-include as `ArtifactStub` in next message | `[wave-13-extends]` |
| Voice input button | browser SpeechRecognition / file-upload audio | Transcribe → insert into textarea (local UI state) | `[shipped]` (local) |

### Refinements to §3 functionality matrix

- **Restart button** — add as `[shipped]` (`start` Protocol method with same agent / system prompt).
- **Cancel-run button** — add as `[shipped]` (`cancel` Protocol method).
- **Token-count chip in header** — add as `[shipped]` (derived from `llm.cost.recorded` event token field).
- **Drift-mode toggle (visible-but-disabled)** — the toggle renders in the Controls card with a tooltip explaining post-V1 deferral per Brief 11 §PG-5 and the existing `[deferred]` bullet in §3.

### No mockup violations of binding carve-outs

- **D-065** — no Priority field. Confirmed.
- **D-091** — page lives under `/console/playground/<session-id>` (served via `harbor console`, not `harbor dev`). The Controls card's reasoning / temperature / system-prompt overrides live in the shared `web/console/src/lib/chat/` module per D-091's "encapsulate first, extract on second consumer."
- **D-062** — Playground is a *session*, not a side channel. Every message ↔ `user_message` Protocol method; every tool call ↔ real `tool.invoked` event. Confirmed.
- **D-066** — Cancel + Approve/Reject are control-scope-gated.
- **D-061** — Console DB holds local state only (drift-mode toggle position, controls-panel collapse state); session / message / artifact / cost data sources from Protocol.
- **§4.5 rule 4 (Skeleton component library)** — the chat-stream rendering, controls panel, and right-rail cards should map onto Skeleton primitives (Stack, Card, RangeSlider, Textarea, Avatar, ActionIcon); the artifact-reference card may need a small custom wrapper around Skeleton's Card primitive per §4.5 rule 4's "lean on Skeleton; justify wrappers in PR."
