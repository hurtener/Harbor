# Console page — Live Runtime

**Slug:** `live-runtime` &middot; **Sidebar cluster:** Runtime &middot; **Route:** `/console/live-runtime`
**Mockup:** `docs/research/console-mockup-runtime-view.png` (legacy location per Brief 12; canonical home is `docs/rfc/assets/console-*.png` once migrated)

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
