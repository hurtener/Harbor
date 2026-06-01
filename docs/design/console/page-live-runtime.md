# Console page — Live Runtime

**Slug:** `live-runtime` &middot; **Sidebar cluster:** Runtime &middot; **Route:** `/console/live-runtime`
**Mockup:** `docs/rfc/assets/console-live-runtime-page.png` (canonical, 2026-05-18). Supersedes the legacy `docs/research/console-mockup-runtime-view.png` which pre-dates D-065 compliance (the legacy shows a `Priority: Normal` field V1 drops) and lacks several Brief 11 §LR-4 / §LR-5 details (Cost, Last error, Tenant in the right rail; sparkline-rich Recent Artifacts; bottom-dock Trace tab). The legacy file stays at `docs/research/` as a research-era artifact (Brief 12 cites it by that path) but is no longer the canonical spec source.

## 1. Purpose

Live Runtime is the operator's present-tense workbench — the page they open when a question has the word "now" in it. The operator initiates, observes, and steers a live execution through the same Protocol surfaces a production caller uses: spawn a task, watch its topology graph build out node-by-node, click into the node currently emitting tokens, inspect its logs, approve or reject a paused step, redirect a run mid-flight, inject context into the trajectory. Live Runtime is the spiritual replacement of the predecessor's Playground (Brief 11 reference): the chat/testing surface is one panel among many, not the whole page. Sessions (the next page in the IA) is the past-and-active investigative counterpart — Live Runtime is for steering what is happening right now (D-062).

## 2. Where it sits in the IA

Live Runtime sits second in the **Runtime** sidebar cluster, immediately after Overview. The operator typically reaches it from Overview (via the live counters bar's "Tasks Running" link), from Sessions (via "Continue this session" / "Convert to live"), or from the global search palette. Within Live Runtime, drilling into a node opens the per-task detail in the bottom-right dock; drilling into a session-level artifact card opens the Artifacts page filtered to this session; drilling into the Interventions sub-panel opens an inline approve/reject UI. Breadcrumb: `<runtime> / Live Runtime / <session-id>`.

## 3. Functionality matrix

- **Topology graph (LR-1) — directed graph of the live run's nodes with type tag, status pill, latency, edge to downstream nodes, click-to-select.** `[wave-13-extends]` Requires the Phase 74 `topology.snapshot` event topic (currently `Pending` per master plan). Wave 13 must land the topology projection events the Console renders against — the node graph + per-node state deltas + edge changes on engine construction.
- **Topology status legend — Running / Completed / Paused / Failed counts in the canvas corner.** `[wave-13-extends]` Derived from the same `topology.snapshot` projection's node states.
- **Tab strip — Topology / Timeline / Metrics / Health.** `[wave-13-extends]` Topology + Timeline both consume the Phase 74 projection. Metrics consumes Phase 56's metric registry (a `metrics.snapshot` Protocol surface is pending). Health consumes a `runtime.health` snapshot (NEW).
- **Status / Filters / Time-range / Pause (Console-side) toolbar.** `[shipped]` Local UI state only; time-range bounds the event subscription, Pause halts the Console's live updates without affecting the runtime.
- **Streaming event stream dock — `HH:MM:SS [type] event_type source_descriptor`.** `[shipped]` Subscribe to `events.EventBus` via Phase 60 SSE (`/v1/events`) filtered to the session's `(tenant, user, session)`; renders `task.started`, `task.completed`, `task.failed`, `tool.invoked`, `tool.completed`, `tool.failed`, `planner.decision`, `planner.finish`, `pause.requested`, `pause.resumed`, `control.received`, `control.applied`, `control.rejected`. Replay-from-cursor on reconnect via the Phase 6 `events.Replayer`.
- **Per-task detail pane (bottom-right dock): Details / Input / Output / Logs tabs.** `[wave-13-extends]` Logs are stored on `state.StateStore` (Phase 7); a `state.history` Protocol method is `Pending`. Input / Output are post-redaction snapshots via `tasks.get` (NEW — currently a `Pending` 73-cluster method). The pane consumes them; Wave 13 must land at minimum `tasks.get` and `state.history`.
- **Session detail panel (right rail): Session ID, Status, Started, Duration, Tasks count, Events count, Agent, User.** `[wave-13-extends]` Requires `sessions.inspect` Protocol method (Phase 73, `Pending`). Counters derive from `session.opened`, `task.spawned`, and observed event count.
- **Current Step sub-panel (right rail): step name + type tag, run id + elapsed, model, input/output token counts.** `[wave-13-extends]` `sessions.inspect` extended with current-run-step context, or a `runs.current_step` projection emitting on planner-step boundaries.
- **Recent Artifacts sub-panel — 3 newest artifacts for this session with filename / size / age.** `[wave-13-extends]` `artifacts.list` Protocol method (Phase 73, `Pending`) filtered to the session, sorted by created-at DESC, capped to 3.
- **Interventions sub-panel — pending pauses, with Approve / Reject / Resume buttons.** `[shipped]` Subscribe to `pause.requested` (`PauseRequested` event; `pauseresume.PauseRequestedPayload`) + `tool.approval_requested` (`approval.ToolApprovalRequestedPayload`) + `tool.auth_required` (`auth.ToolAuthRequiredPayload`); resolve via `approve` / `reject` / `resume` Protocol methods (D-072: all three are canonical control methods today).
- **Start a new run — composer with goal, optional file uploads, model selector.** `[shipped]` Spawns via the `start` Protocol method (`types.StartRequest` / `types.StartResponse`). File uploads route through the Artifact store before becoming the run's input per Brief 11 §PG-2; chat/playground module lives at `web/console/src/lib/chat/` per D-091.
- **Redirect a live run — composer to rewrite the goal.** `[shipped]` Invokes the `redirect` Protocol method (`types.ControlRequest` with `Payload.goal`).
- **Inject context into a live run.** `[shipped]` Invokes the `inject_context` Protocol method.
- **Send a user message into a live run.** `[shipped]` Invokes the `user_message` Protocol method.
- **Cancel a live run (soft default, hard via toggle).** `[shipped]` Invokes the `cancel` Protocol method (`Payload.hard` toggles the cancellation context).
- **Pause / Resume control buttons on the run.** `[shipped]` Invokes `pause` / `resume` Protocol methods; routes through the unified `pauseresume.Coordinator`.
- **No Priority field on the session detail panel.** `[deferred]` D-065 dropped session-level priority from V1. The `prioritize` Protocol method exists for task-level priority (D-072), but session-level priority is out.
- **Multi-agent / agent picker on Start composer.** `[wave-13-extends]` `agents.list` Protocol method (NEW, see page-agents.md) is required for the dropdown.
- **Trace toggle — overlay topology inline with chat messages.** `[wave-13-extends]` Brief 11 §PG-7; requires the same topology projection plus a chat ↔ topology correlation key (run id is sufficient).
- **Drift mode (fork a run, edit a past user message, re-play).** `[deferred]` Brief 11 §PG-5; not V1 scope.
- **No persistence of operator-laid topology overrides.** `[deferred]` Brief 11 §LR-1; auto-layout (dagre / ELK / d3-force) for V1; persistence is post-V1.

## 4. Page anatomy

- **Sidebar** (shared): runtime switcher + cluster nav + footer counter strip.
- **Top bar** (shared): breadcrumb (`<runtime> / Live Runtime / <session-id>`) + global search + notifications + identity scope chip.
- **Main canvas** (per-page):
  - Row 1 — tab strip (Topology | Timeline | Metrics | Health) + status legend + filters + time-range picker + Pause-updates button (right-aligned).
  - Row 2 — main view region (the topology graph at default; other tabs swap the view): full-bleed canvas.
- **Right rail** (per-page, persistent):
  - Session detail header card.
  - Current Step sub-panel.
  - Recent Artifacts sub-panel (3 rows + "View All Artifacts" link).
  - Interventions sub-panel (live list of pending pauses with Approve / Reject / Resume).
- **Bottom dock** (per-page):
  - Left (≈40%): Event Stream — live, filtered, time-ordered.
  - Right (≈60%): Per-task detail pane (Details / Input / Output / Logs tabs) when a node is selected; otherwise the Start / Redirect / Inject composer.
- **Footer** (shared): micro-counters.

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Topology graph | `topology.snapshot` event (NEW) | Click node → bottom-dock per-task detail; pan/zoom (local UI state only) | `[wave-13-extends]` |
| Status legend | `topology.snapshot` node-state aggregate | Click count → filter graph by status (local UI state only) | `[wave-13-extends]` |
| Timeline tab | `topology.snapshot` + `task.started` / `task.completed` ordering | Click swimlane row → bottom-dock detail | `[wave-13-extends]` |
| Metrics tab | `metrics.snapshot` (NEW; Phase 56 derivation) | Local UI state (sort / window) | `[wave-13-extends]` |
| Health tab | `runtime.health` snapshot (NEW; Phase 56 derivation) | Local UI state | `[wave-13-extends]` |
| Filters / time-range / Pause-updates | local UI state | Re-scope the event subscription via filter | `[shipped]` |
| Event Stream dock | `events.EventBus` subscription via `/v1/events` SSE (Phase 60) | Click row → bottom-dock detail (for `task.*` / `tool.*` events) | `[shipped]` |
| Per-task detail — Details | `tasks.get` (NEW Phase 73 method) | local UI state (tab switch / copy) | `[wave-13-extends]` |
| Per-task detail — Input | `tasks.get` (NEW) — post-redaction args | Copy as JSON (local UI state) | `[wave-13-extends]` |
| Per-task detail — Output | `tasks.get` (NEW) — post-redaction result; `ArtifactStub` materialised via `artifacts.get_ref` | Open artifact (deep-link to Artifacts page) | `[wave-13-extends]` |
| Per-task detail — Logs | `state.history` (NEW Phase 73 method) keyed by task id | Scroll / copy / "Show debug" (local UI state) | `[wave-13-extends]` |
| Session detail header | `sessions.inspect` (NEW Phase 73 method) | Click Session ID → copy; click Agent → Agents page detail; click User → no-op in V1 | `[wave-13-extends]` |
| Current Step sub-panel | `sessions.inspect` extended fields (NEW) | "View Details" → bottom-dock detail for that step | `[wave-13-extends]` |
| Recent Artifacts sub-panel | `artifacts.list` (NEW Phase 73 method) filtered to session | Click row → Artifact preview; "View All Artifacts" → Artifacts page filtered to session | `[wave-13-extends]` |
| Interventions sub-panel | `pause.requested` / `pause.resumed` / `tool.approval_requested` / `tool.approved` / `tool.rejected` / `tool.auth_required` events | Approve → `approve` method; Reject → `reject` method; Resume → `resume` method; "View" → bottom-dock detail | `[shipped]` |
| Start composer | local input + `start` Protocol method | Submit → `start` (`types.StartRequest`) | `[shipped]` |
| Redirect composer | local input + `redirect` Protocol method | Submit → `redirect` (`types.ControlRequest` with `Payload.goal`) | `[shipped]` |
| Inject-context composer | local input + `inject_context` Protocol method | Submit → `inject_context` (`types.ControlRequest`) | `[shipped]` |
| User-message composer | local input + `user_message` Protocol method | Submit → `user_message` (`types.ControlRequest` with `Payload.message`) | `[shipped]` |
| Cancel button | `cancel` Protocol method | Soft cancel (default) or hard cancel (toggle); `types.ControlRequest` with `Payload.hard` boolean | `[shipped]` |
| Pause / Resume buttons | `pause` / `resume` Protocol methods | Submit → `pauseresume.Coordinator.Request` / `.Resume` via Phase 60 | `[shipped]` |
| Trace toggle | `topology.snapshot` (NEW) + chat correlation by run id | Local UI state | `[wave-13-extends]` |

## 6. Controls + actions

- **Toolbar (main canvas tabs):** Tab switch (Topology / Timeline / Metrics / Health); status legend pills act as filters; time-range picker (Last 30 min default); Pause-Console-updates toggle.
- **Row-action (Event Stream):** click row → drill into per-task detail (for `task.*` / `tool.*` events); copy row as JSON.
- **Panel-action (Interventions sub-panel):** Approve / Reject / Resume per row; "View" opens bottom-dock detail.
- **Panel-action (per-task detail):** Tab switch (Details / Input / Output / Logs); Expand to full screen; Copy current tab.
- **Composer actions:** Start (Cmd-Enter); Redirect; Inject Context; User Message; Cancel (soft); Hard Cancel (Cmd-Shift-Backspace).
- **Keyboard shortcuts:** `Space` toggle Pause-updates while focused on the Event Stream; `j` / `k` next / previous event row; `Enter` drill into selected event; `Esc` close per-task detail.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| No live run yet | Session has no spawned task | Empty topology canvas with "Start a run" composer prominent | Compose and submit a `start` |
| Initial loading | Topology subscription opening | Canvas skeleton + counter-zeros legend; Event Stream shows "Connecting…" | Auto-resolves on first `topology.snapshot` |
| Protocol error — `CodeNotFound` on session | Session id in URL does not exist (or no live inbox) | Banner: "Session not found — perhaps it ended"; offer "Open in Sessions (history)" link | Navigate to Sessions page |
| Protocol error — `CodeScopeMismatch` on a control | Operator submitted Pause / Approve etc. without the required scope | Inline error on the button: "Insufficient scope (`scope_mismatch`)" | Request elevated scope from admin |
| Protocol error — `CodePayloadInvalid` on Redirect / Inject | Composer text exceeded RFC §6.3 bounds (4096 chars / 16 KiB) | Inline error: "Payload too large — trim and retry" | Edit composer |
| Protocol error — `CodeIdentityRequired` | Connection lost identity propagation | Banner: "Identity scope missing — re-attach the runtime" | Open Settings → Connected Runtimes |
| Protocol error — `CodeAuthRejected` | JWT expired mid-stream | Banner: "Authentication expired — re-enter passphrase" + auto-reconnect on re-auth | Re-enter WebCrypto passphrase per D-091 |
| Unauthorized — admin-fan-in attempted without `admin` | Operator toggled "All sessions" filter without `admin` claim | Hide the toggle; on submission, server returns `CodeScopeMismatch` | Request elevated scope |

## 8. Multi-tenant / multi-runtime nuances

Live Runtime is always scoped to a specific session in a specific runtime — never aggregated. Cross-tenant viewing is meaningless here (a single session belongs to one tenant). The page does, however, change behavior in two scope-driven ways: (1) the Start composer's "Run as another identity" option is gated on `admin` and requires explicit impersonation per Brief 11 §PG-5 — when used, the impersonating identity is captured on the request and audited; (2) the Interventions sub-panel renders Approve / Reject / Resume buttons only when the operator's scope claim includes the more-elevated control claim per D-066. In multi-runtime mode, the runtime switcher in the sidebar selects the active runtime context; switching mid-run is allowed but cancels the current subscriptions and re-handshakes against the new runtime.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple from the JWT — sufficient to observe a live run, render its topology + event stream + per-task detail, start a new run within the same session, redirect / inject / cancel one's own run.
- `admin` (`auth.ScopeAdmin`) — required to impersonate another identity in the Start composer ("Run as another identity"); required to elevate the Event Stream to fan-in across sessions in the same tenant.
- `console:fleet` (`auth.ScopeConsoleFleet`) — required to elevate Event Stream beyond one tenant; rare on this page (per-session by construction) but possible during cross-tenant debugging.
- **Control-plane verbs Approve / Reject / Resume / Pause / Cancel (hard)** require the more-elevated control claim per D-066; the Console hides the buttons when the JWT lacks it, and the Protocol re-checks server-side returning `CodeScopeMismatch`.

## 10. Out of V1 (deferred)

- **Drift mode** (Brief 11 §PG-5) — fork-and-edit-replay; post-V1.
- **Side-by-side comparison** (Brief 11 §PG-6) — two Live Runtime canvases on the same input; post-V1; foreshadows Evaluations (D-064, post-V1).
- **Operator-laid topology layout persistence** — automatic layout for V1; per-session operator overrides are post-V1.
- **Per-session priority field** — D-065 dropped from V1.
- **No Evaluations entry from this page** — D-064.
- **No Flows-editor entry from a topology node** — D-063 makes Flows a viewer in V1, not an editor.

## 11. References

- Brief 11 §"Live Runtime view" (LR-1 through LR-6), §PG-1 through §PG-7 (Playground / chat is one panel here), §CC-1 (multi-runtime), §CC-2 (identity-aware UI).
- Brief 12 §"The shared chat / playground library" (`web/console/src/lib/chat/`).
- RFC-001-Harbor.md §5 (Protocol), §6.3 (steering + pause/resume), §6.13 (event bus), §7 (Console), §7.1 (runtime-lens principle).
- Decisions: D-002, D-062 (Live Runtime ≠ Sessions), D-065 (no session priority), D-066 (control claim), D-067 (Pause/Resume Coordinator), D-070 (steering inbox), D-072 (Protocol task control surface — the ten methods), D-077 (versioning), D-078 (SSE + REST wire), D-091 (Console deployment posture).
- Phase plan: phases 50–53 (steering / pause-resume — `Shipped`), phase 54 (task control surface — `Shipped`), phase 60 (Protocol wire transport — `Shipped`), phase 72 (Console subscription — `Pending`), phase 73 (state inspection — `Pending`), phase 74 (topology projection events — `Pending`).
- Glossary terms used: `Live Runtime`, `Pause/Resume Coordinator`, `Runtime lens`, `Scope claim`, `Fleet control / fleet observation`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-live-runtime-page.png` against §3-§7 above. The agent-authored spec (sections 1-11) is the v1.0 surface; this section adds the mockup-derived specifics that should drive the SvelteKit implementation.

### Refinements to §4 page anatomy

- **Header status counter strip** above the tab strip, showing 5 chips: `Pending <N>` / `Running <N>` / `Completed <N>` / `Paused <N>` / `Failed <N>`. The spec puts the status legend in the canvas corner; the mockup elevates these to a top-of-page bar so they survive every tab switch. Status legend in the canvas corner stays as a per-tab affordance (graph-only).
- **Breadcrumb is three-segment**: `<runtime-name> / Live Runtime / <run-id>` (where `<run-id>` is the truncated 8-char id like `01KVR3D...`). Clicking the runtime name opens the runtime switcher; clicking "Live Runtime" navigates to a "pick a run" landing; clicking the run-id copies it.
- **Right rail expands to include `Cost` and `Last error`** fields plus `Tenant` (in addition to Session ID / Status / Started / Duration / Tasks / Events / Agent / User the spec already lists). Cost is computed from `llm.cost.recorded` events aggregated for the run. Last error is the most recent `task.failed` / `tool.failed` / `planner.error` payload's error message + class. Tenant is the run's identity-tuple tenant (admin views may show non-default tenants).
- **Recent Artifacts sub-panel renders mime icons + filename + size + relative timestamp** per row (not just filename / size / age the spec describes). Visual richness matches Brief 11 §LR-5's "preview cards" intent.
- **Interventions sub-panel renders a per-intervention card** with: title (e.g., "Tool approval needed"), reason (the reason string from the `pause.requested` / `tool.approval_requested` payload), countdown (time-until-auto-timeout when one applies), and Approve / Reject buttons. The spec's "View" button is integrated into the card (the whole card is clickable).
- **Bottom dock adds a "Trace" tab** in addition to Details / Input / Output / Logs (so the operator can see the per-task trace span tree without leaving the page). `[wave-13-extends]` requires a `tasks.trace` Protocol method or the existing `state.history` extended with span correlation.
- **Footer carries Protocol version + Events Stream connection state + Console version**: `Connected to <runtime> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`. Replaces the spec's generic "micro-counters" footer with concrete connection posture.
- **Topology canvas controls**: Pause-stream toggle (top-right of canvas) + Reset-zoom button + filter chips (above canvas) for `Status: All|Pending|Running|Completed|Paused|Failed` and tool-name filter + time-range. Replaces the spec's lone "Pause-updates" + "filters" mentions with the concrete control surface.

### Refinements to §3 functionality matrix

- **Cost field on right rail** — add as `[shipped]` (derived from `llm.cost.recorded` events aggregated client-side; the event ships per `internal/llm/events.go::EventTypeLLMCostRecorded`).
- **Last-error field on right rail** — add as `[shipped]` (latest `task.failed` / `tool.failed` / `planner.error` event payload — all shipped).
- **Tenant field on right rail** — add as `[shipped]` (identity tuple from `events.Event.Identity.TenantID`).
- **Trace tab in bottom dock** — add as `[wave-13-extends]` (Wave 13 to decide: extend `state.history` with span correlation OR new `tasks.trace` method).
- **Status counter strip in header** — derive from the topology projection's node states (already `[wave-13-extends]` via Phase 74); refine to note this is a header-level vs canvas-level concern.
- **Topology shows error/reject paths** (not just happy-path nodes) — the mockup's graph includes a `Reject` (failed) terminal node alongside the happy `Aggregate → Upload Artifact`. The spec's topology bullet implicitly covers this via "status pill"; explicit here: failed nodes render with a red border + failure code tag.

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Header status counter strip | derived from `topology.snapshot` event (NEW) | Click chip → filter graph to that status (local UI state) | `[wave-13-extends]` |
| Cost field (right rail) | `llm.cost.recorded` events aggregated client-side for the run | Click → drill into Cost overview (deep-link to Settings → Governance) | `[shipped]` |
| Last-error field (right rail) | most recent `task.failed` / `tool.failed` / `planner.error` event payload | Click → bottom-dock detail filtered to errors | `[shipped]` |
| Tenant field (right rail) | `events.Event.Identity.TenantID` | Click → no-op (single-tenant scope) or open scope-elevation dialog (multi-tenant) | `[shipped]` |
| Trace tab (bottom dock) | `tasks.trace` (NEW) OR `state.history` with span correlation (NEW) | Expand / collapse spans (local UI state); click span → focus on its node in topology | `[wave-13-extends]` |
| Intervention countdown | derived from `pause.requested` payload's optional `expires_at` field | Visual countdown only; expires resolves server-side via auto-reject | `[shipped]` (event surface) |
| Filter chips above canvas | local UI state | Re-scope the topology rendering + event subscription | `[shipped]` |
| Reset-zoom button | local UI state | Reset zoom + pan to default | `[shipped]` |

### Refinements to §7 states

- The "No live run yet" state should still show the runtime context + breadcrumb + (empty) status counter strip + Start composer — operator gets immediate orientation rather than a blank canvas.
- The "Initial loading" state shows skeleton placeholders for topology + status counters + right rail; Event Stream shows the actual "Connecting…" line (matches Brief 11 §LR-2 streaming-indicator pattern).

### No mockup violations of binding carve-outs

- **D-065 (no session priority)** — mockup explicitly omits the field. Confirmed delta from the legacy mockup. Spec §3 bullet "No Priority field" stands.
- **D-061 (Console DB local-only)** — mockup honors. Cost / Last error / Tenant all derive from Protocol events; nothing is mirrored Console-side.
- **D-091 (multi-runtime deployment)** — mockup's breadcrumb pattern + footer's "Connected to <runtime>" makes the multi-runtime context explicit.
- **D-066 (control-scope on intervention verbs)** — mockup shows the elevated-operator view (Approve/Reject visible). Spec's existing rule (buttons hidden when JWT lacks the claim) preserved.
- **D-062 (Live Runtime ≠ Sessions, Agents ≠ chatbots)** — mockup shows the topology workbench, not a chat surface. Confirmed.
- **Runtime-lens principle (§7.1)** — every panel sources from a Protocol surface (events / snapshots). No standalone Console-only features.

## 13. Reframe — single-runtime capability-adaptive cockpit (Phase 108e, 2026-06-01)

**This section supersedes the topology-first composition** of §4 (page anatomy, "the topology graph at default") and §12 for the page's *spine*. The per-datum source map (§5), the binding carve-outs (§12 "No mockup violations"), and D-061/D-062/D-066/D-164 all stand and are reused. Recorded as **D-177** (supersedes the composition half of D-126).

**Why.** `topology.snapshot` exists only on engine-graph runtimes; the dominant V1 shape is planner/RunLoop (`harbor dev`, most scaffolded agents), which returns `unknown_method` (D-164). A topology-first page is therefore an empty "topology not available" hero on the common runtime, and the remainder (steer-a-run + event table) duplicates the Playground (D-062). The 108d build confirmed it: sparse, Playground-overlapping, weak use of space.

**The reframe.** Live Runtime is the **single-runtime operations cockpit** — the Overview(fleet) → one-runtime drill-down (a runtime switcher selects context; it never fans in — that is Overview). The page composes itself from the runtime's advertised `runtime.info` capabilities (Phase 84a): an always-present **spine** plus **capability-gated** panels. A new shape (engine, multi-agent, workflow, distributed) adds a panel entry to the registry, never a page rewrite. The free-floating Start/Redirect/Inject/User-message composer is **removed**; run-level steering is a drill into a session → Playground.

### Capability → panel map

| Panel | Capability gate | Protocol source | Shape |
|---|---|---|---|
| Runtime posture header (name · version · protocol · capability chips · switcher) | always | `runtime.info` (84a) | spine |
| Activity counters (Pending/Running/Completed/Paused/Failed) | always | `tasks.list` strip / event-fold | spine |
| Needs attention (pauses / approvals / auth-required) | always | `pause.list` + `pause.requested` / `tool.approval_requested` / `tool.auth_required`; resolve via approve/reject/resume (D-072, D-066-gated) | spine |
| Live event stream | always | `GET /v1/events` SSE (§6.13) | spine |
| Active sessions (drill → Sessions / Playground) | `sessions_list` (else event-fold of `session.opened`) | `sessions.list` / `sessions.inspect` | spine (degrades to event-derived) |
| Cost & governance posture | `governance_posture` / `llm.cost.recorded` | 72g posture + `llm.cost.recorded` aggregate | gated → honest "not advertised" |
| Health | `runtime_health` | `runtime.health` (72f) | gated → honest |
| Topology (graph + legend + timeline) | `topology_snapshot` | `topology.snapshot` (74) | gated → absent on planner |
| (future) multi-agent / workflow / distributed | new capability key | future surfaces | gated, additive |

**Honest states (CLAUDE.md §13).** A gated-absent panel renders "this runtime does not advertise `<X>`", never synthetic data. Topology node run-state (legend counts + failed-node styling) is Console-derived from the event stream — the wire projection carries no per-node state — empty → zeros.

**Layout bar.** Viewport-locked (no full-page scroll; only inner regions scroll), shared baseline grid, full-bleed, deliberate negative space — and validated in the EMPTY/info state (what dev runtimes render), not only the populated one. Plan: `docs/plans/phase-108e-live-runtime-capability-cockpit.md`.
