# Research Brief 11 — Harbor Console feature surface

**Date:** 2026-05-12
**Status:** research input documenting the *intended* Console feature surface, derived from the operator's mockup (`console-mockup-runtime-view.png`, attached) plus the playground / direct-interaction extension the operator called out verbally. **Not a settled design.** The Console lives in its own repo / monorepo slot (`web/console/` per CLAUDE.md §4.5). Phases 72–75 (Console subscription / state / topology / Playwright) ship the Protocol-side and rendering substrate; this brief enumerates what *features* the substrate must support so those phases' acceptance criteria can be sharpened.

## Why this brief exists

The Console is one of three V1 products in the Harbor ecosystem (Runtime, Protocol, Console — RFC §1) but its feature surface lives only as "observability + control plane UI" in the RFC and as Phases 72–75 in the master plan. The mockup the operator shared communicates a much richer intent than the current phase plans encode. This brief decomposes the mockup into discrete features, identifies which runtime primitives each one consumes, and flags the gaps between the mockup's implied capabilities and what's currently planned.

The output of this brief is *not* a Console design spec. It is:

1. A feature-by-feature inventory of what the mockup shows.
2. The Protocol surface each feature requires (which phase ships it).
3. The features the mockup implies for other sidebar sections it doesn't expand.
4. The playground / direct-interaction extension and its constraints.
5. Open architectural questions that block Console phase plans being authored cleanly.

## Reference artifact

`docs/research/console-mockup-runtime-view.png` — the operator's mockup of the Live Runtime view. The agent/user names visible in that mockup are illustrative placeholders; this brief refers to agents and users abstractly. The mockup is one view among many implied by the sidebar; the brief covers the visible view in detail and sketches the implied views.

## Architectural ground rules (binding from CLAUDE.md §4.5)

These are settled and constrain every feature below. Listed here so the brief is self-contained for a future implementor:

1. **SvelteKit + Tailwind + GSAP**; `@sveltejs/adapter-static`. Not React, not Next, not Vue. Decision is closed.
2. **Decoupled deployment**: Runtime ships headless; Console can be co-located, remote, or third-party. Runtime binary does not embed Console source.
3. **Design tokens in one place** (`web/console/src/lib/tokens.css`). No raw color/spacing literals in `.svelte` files (forbidden).
4. **Lean on a component library** (default: Skeleton). No hand-rolled primitives the library already covers.
5. **Typed Protocol client** (`web/console/src/lib/protocol.ts`). No raw `fetch` in `.svelte` files.
6. **`svelte-check` + `npm run check` + `npm run lint` + `npm run build` in CI.**
7. **`npm` only**; lockfile committed; no build artifacts in git.
8. **The Console NEVER reads internal Runtime types.** All data flows through canonical Protocol events / state snapshots / topology / artifacts (by reference) / traces / metrics.

Every feature below MUST satisfy 8: if the data isn't on the Protocol, the feature can't ship, and the Protocol-side phase needs to add it.

## Layout decomposition (from the mockup)

The mockup has three structural regions plus the bottom panel:

### Left sidebar — navigation + global context

Sections (top to bottom):

- **RUNTIME** — current connected runtime indicator. Mockup shows "Local Runtime • Running" with a chevron suggesting a switcher. Implies multi-runtime support (Console can attach to multiple Harbor runtimes simultaneously, switch between them).
- **OVERVIEW** — operational views: Live Runtime / Sessions / Tasks / Agents / Tools / Events / Background Jobs.
- **BUILD** — authoring views: Flows / Memory / MCP Connections.
- **ANALYZE** — historical / quality views: Evaluations / Artifacts.
- **SETTINGS** — Console-wide configuration.
- **Footer**: persistent live counters — `Events / sec`, `Tasks Running`, `Background Jobs`, `MCP Connections`. User profile at bottom (admin scope visible).

### Top bar — global search + status

- **Global search** (⌘K) across sessions / events / artifacts / tools / agents / flows. Implies a search index over Protocol-emitted entities.
- **Notification center** (bell icon, badge count = 3). Implies a typed notification taxonomy on the Protocol (budget-exceeded, run-paused, run-failed, tool-auth-required, …).
- **Help** (question mark) — Console-local docs.
- **User menu** (avatar).

### Main viewport — the selected view's content

Mockup shows the **Live Runtime** view in detail. Other views (Sessions, Tasks, Agents, …) are implied; this brief sketches them under §"Features the mockup implies."

### Bottom dock — event stream + per-task drill-down

- **Event Stream**: live feed of canonical events, filterable. Always visible while a runtime is connected.
- **Per-task pane**: contextual to the selected node in the topology — shows Details / Input / Output / Logs for the active step. Replaces the right sidebar when a node is selected? Or coexists? (Open question.)

## Live Runtime view — feature inventory

The view is the mockup's centerpiece. Decomposing into discrete features:

### LR-1. Topology graph

A directed graph of the run's nodes (or session's tasks, or both — see open questions). Each node carries:

- A **name** (the node's display label — e.g. "Search Skills").
- A **type tag** (mcp / api / llm / planner / web / stream — matching `TransportKind` from `internal/tools/tools.go` + new tags for `planner` and `stream` and `web` source).
- A **status pill** (Running / Completed / Paused / Failed) with status counts in a side legend (4 / 6 / 1 / 0 in the mockup).
- A **latency** when completed.
- An **edge** to downstream nodes (the runtime's DAG).
- A **selection state** (click to drill into the bottom-dock per-task pane).
- **Visual encoding** for special states: the Human Approval node is rendered with a dashed border + warning color to communicate "awaiting user action."

Protocol surface required:

- Topology projection events (Phase 74 — `Console topology projection events`).
- Per-task status events (existing event bus + Phase 72 subscription).
- Streaming updates as new nodes/edges/state changes land.
- Cursor-resumable subscription so reconnect catches up cleanly (Phase 6 — `Bus replay + ring buffer + cursor`; Phase 72 surfaces it on the Protocol).

Open questions:

- Is the graph **per-run** (one DAG instance for one trigger) or **per-session** (the union of all runs in this session)? The mockup looks per-session given multiple node types coexist; but task IDs in the event stream all carry one `task_1a2b3c` prefix → per-run is also plausible. Recommendation: per-run primary, with a "show all session tasks" toggle.
- Layout strategy: hand-laid (operator places nodes) vs. automatic (dagre / ELK / d3-force). Mockup looks automatic. Recommendation: automatic, with persistence of operator overrides per session.
- Node grouping: should the Console group N successive tool calls of the same source into one collapsed "tool calls × N" node? Default no; expansion is the user's job.

### LR-2. Tab strip — Topology / Timeline / Metrics / Health

Four sibling views over the same session/run:

- **Topology** (LR-1) — graph.
- **Timeline** — horizontal swimlane / Gantt of the same nodes, ordered by start time. Useful for parallel-execution visualization.
- **Metrics** — per-session aggregates: total cost, total tokens (in/out), p50/p95 latency, tool-call counts. Some come from Phase 36a (cost accumulator); some from Phase 56 (metrics + OTLP).
- **Health** — current runtime's resource indicators (goroutine count, GC pause, queue depths, dropped-events rate). Comes from Phase 56 metrics.

### LR-3. Status counts legend + filters + time range + pause

Top-right of the topology canvas:

- **Status counts** (Running / Completed / Paused / Failed) with counts.
- **Filters** dropdown — likely by node type / status / source / minimum latency / has-error.
- **Time range** — "Last 30 min" default. Implies the event subscription is time-bounded; replay needs to materialize the topology for an older window.
- **Pause** button — pauses the *Console's* live updates, not the runtime. Useful when the operator is reading a fast-moving stream.

### LR-4. Session detail panel (right sidebar)

Persistent right column showing:

- **Session ID** with copy button.
- **Status** (Running / Paused / Completed / Failed).
- **Started** (relative time).
- **Duration** (live-updating).
- **Tasks** (count).
- **Events** (count).
- **Agent** (which configured agent owns this session — opens to brief 09's agent-identity territory; the field implies first-class agents).
- **User** (email / identifier of the originating user).
- **Priority** (Normal / High / Low — implies a priority dimension that doesn't exist in the current master plan).

Then **Current Step** sub-panel:

- Step name + type tag.
- Run-ID + elapsed-time-for-this-step.
- Model name (when LLM step) — comes from the model configured in Phase 33 / 34.
- Input tokens / Output tokens — comes from `BifrostLLMUsage` via Phase 33.
- "View Details" → opens the bottom-dock per-task pane.

Then **Recent Artifacts** sub-panel:

- 3 most recent artifacts produced by this session, with filename / size / age.
- "View All Artifacts" link → goes to the Artifacts view filtered to this session.
- Comes from Phases 17–19 (Artifact store + drivers); the Protocol exposes artifacts by reference only — clicking a row opens the artifact viewer.

Then **Interventions** sub-panel:

- A live list of pending pause records for this session.
- Shows: type (Human Approval / OAuth-required / Tool approval), elapsed time paused, requester (agent or system), reason.
- "View" → opens the intervention detail; "Resume" → action button gated by JWT scope.
- Comes from Phase 50 (Pause/Resume Coordinator) + Phase 54 (Protocol task control surface).

Open questions for the right sidebar:

- Should the right sidebar be **session-scoped** (current selection) or **run-scoped** (the run currently being inspected)? The "Current Step" content suggests run-scoped, but the metadata above suggests session-scoped. Recommendation: session-scoped with a "current run within session" navigator.
- The "Priority" field doesn't exist in the master plan — it's a feature the mockup invents. Does Harbor honor a per-session priority dimension? Not currently. **Open question**: add it (touches Phase 20 task service + Phase 14 routers) or drop it from the Console design? Recommendation: drop it from V1; revisit if operator load patterns demand it.

### LR-5. Event Stream (bottom-left dock)

Live, filtered, time-ordered list:

- Format per row: `HH:MM:SS [type-icon] event_type source_descriptor`
- Event types visible: `task.started`, `tool.call`, `tool.result`. Implies the canonical event taxonomy from Phase 5 + later phases is what backs this.
- Filter dropdown — by event type / source / identity / search text.
- "Live" indicator pulse when streaming.

Protocol surface:

- Phase 72 (Console subscription protocol surface) ships the wire.
- Phase 6 (replay + cursor) lets the stream pick up from a cursor on reconnect.

Open question: does the stream respect identity-scope filtering inherent in the JWT, or does an admin see every tenant's events by default? Recommendation: respect identity by default, with a per-view "elevate to fleet view" gesture that requires admin scope (mirrors §6.5 of CLAUDE.md — the elevated subscription).

### LR-6. Per-task detail pane (bottom-right dock)

When a node is selected, this pane shows:

- **Step name + type + status + live indicator + close button**.
- **Tab strip**: Details / Input / Output / Logs.
- **Logs tab** (shown in mockup): time-stamped log entries with severity (info / debug). Streaming chunk events appear inline as `[debug] Chunk received (X.X KB)`.
- **Details tab** (implied): metadata about the descriptor — tool name, source, schema, configured policy, identity at invocation time.
- **Input tab** (implied): the call arguments (post-redaction).
- **Output tab** (implied): the result (post-redaction; rendered with rich-output support — see playground section).
- **Expand** + **Copy** buttons.

Protocol surface:

- Per-task structured log retrieval (likely via Phase 57 durable event log + Phase 60 protocol transport).
- Per-task input/output retrieval — these are stored on the StateStore (Phase 7) + audit-redacted before exposure (Phase 3).

Open question: Logs are time-stamped to the millisecond and include chunk-receipt events — these come from `slog` (Phase 4). Does the Console subscribe to the runtime's log stream as a Protocol event topic, or fetch logs on-demand keyed by task? Recommendation: on-demand by task (subscribing to all logs is too noisy); the Logs tab pulls + streams the task's specific log slice.

## Features the mockup implies for other sidebar sections

The mockup focuses on Live Runtime. The sidebar lists 13 more views. Each gets a feature sketch and a phase mapping.

### Sessions view (OVERVIEW)

A list / table of sessions with filtering and drill-down:

- Filters: status, agent, user, tenant, started-in-window, has-pending-intervention, has-failed-task, cost-above-threshold.
- Per-row: session ID, agent, user, status, started, duration, tasks, total-cost, total-tokens.
- Click → opens the Live Runtime view scoped to that session.
- Bulk actions: cancel selected, pause selected (gated on scope).
- Pagination / virtual scroll for large operators.

Backing: Phase 8 (SessionRegistry) + Phase 72 (Protocol subscription) + Phase 36a (cost data).

### Tasks view (OVERVIEW)

Like sessions but at the task granularity. Useful for "find every failed task in the last hour" / "what's currently running across all sessions":

- Filters: task type, status, source, latency-above, identity, error-class.
- Per-row: task ID, parent session, type, status, started, duration, identity.
- Click → opens the per-task detail pane scoped to history (not live).

Backing: Phase 20 (TaskRegistry) + Phase 21 (TaskGroup) + Phase 72.

### Agents view (OVERVIEW)

This is the most under-specified view in the master plan and the most important one for brief 09 + the playground (below):

- List of configured agents in the current tenant.
- Per-agent: name, description, owner (admin who created), # sessions today, # users authorized, # tool attachments, # MCP connections, # agent-bound credentials configured (count of `ScopeAgent` OAuth bindings with valid tokens).
- Click → opens the **Agent Detail** view:
  - Configured tool attachments (MCP / HTTP / A2A / in-process / flow).
  - Per-attachment: auth status (no auth / headers / OAuth user-bound / OAuth agent-bound + token freshness).
  - Per-attachment OAuth setup: "Connect Outlook" / "Reconnect (token expired)" / "Revoke" buttons — gates per binding scope (admin can configure ScopeAgent; users configure their own ScopeUser bindings via their own page).
  - Configured planner choice + planner config.
  - Configured model + cost ceilings (per Phase 36a).
  - Configured memory strategy (per Phase 24).
  - Configured skills (per Phase 37 etc.).
  - "Open in Playground" button → see §"Playground / direct interaction" below.
  - "Recent sessions" — list of recent sessions for this agent.
  - "Permissions" — which users can invoke this agent.

Backing: this view does not currently have a phase. **Recommendation**: a new phase (or expansion of Phase 73) covering "agent management surface" — depends on the agent-identity RFC stub (brief 09).

### Tools view (OVERVIEW or BUILD — undecided)

Browse the registered tool catalog:

- Filter by source / transport / side-effect class / loading mode / tag.
- Per-row: name, source, transport, side-effect, schema hash, examples count, last-invoked.
- Click → tool detail: full schema, description, examples, recent invocations, policy, source provenance.
- "Try this tool" → playground-like form that lets the operator invoke the tool directly with hand-crafted args (gated on developer scope; uses identity propagation; events emit).

Backing: Phase 26 (catalog) + Phase 72.

### Events view (OVERVIEW)

The event stream from the bottom dock but as a full-screen, query-driven view:

- Time-range picker, type filter, identity filter, free-text search.
- Save / share filtered views.
- Export to JSONL / CSV (per-row rendered, post-redaction).
- Aggregate counts visualization (rate-over-time).

Backing: Phase 57 (durable event log) + Phase 6 (replay + cursor) + Phase 60.

### Background Jobs view (OVERVIEW)

Long-running tasks that don't belong to a foreground session:

- Filters: status, type, identity, age.
- Per-row: job ID, type, status, started, ETA, # related sessions.
- Click → progress detail (artifacts produced so far, sub-task progress).
- Bulk actions: cancel, retry, requeue.

Backing: Phase 20–21 + Phase 72. Phase 20's `TaskRegistry` already unifies foreground/background.

### Flows view (BUILD)

Read-only inspector + (optionally) editor for declared Flows (Phase 26a Flow-as-Tool, Phase 100 Recipe loader):

- Per-flow: visual DAG (nodes + edges), source-of-truth view (YAML / Go code reference), test history.
- "Run this flow" → playground-like invocation.
- Read-only V1; editor / DSL is post-V1.

Backing: Phase 26a + Phase 100.

### Memory view (BUILD)

Inspect memory state per identity / session / agent:

- Filter by identity (the visible scope respects the JWT — only sees what the JWT scope allows).
- Memory items list (per-item: content, ttl, created, last-accessed, scope).
- Memory strategy debugger — show how a strategy (Phase 24) selected items for a given session.
- Manual operations: add memory, edit, evict (admin-only).

Backing: Phases 23–25.

### MCP Connections view (BUILD)

The shipped MCP southbound surface (Phase 28) deserves a control plane:

- List of configured MCP servers.
- Per-server: name, transport, URL/command, state (connected/disconnected/error), last-discovery time, tool count, recent latency, error rate.
- "Refresh discovery" button → calls Phase 28's `Provider.Discover` again.
- "View tools" → drill into the tool list.
- "View resources / prompts" — separate tab, since Phase 28 maps these to tool wrappers but the source-of-truth view should distinguish them.
- "Configure OAuth" — brief 09's UI: pick BindingScope, complete the flow.
- "Test connection" button — performs a ping or list-tools round-trip.

Backing: Phase 28 + brief 09 + Phase 73.

### Evaluations view (ANALYZE)

Out-of-scope for V1 per the master plan, but the sidebar lists it:

- Eval suites, runs, scores.
- A/B comparisons (planner X vs planner Y on the same input set).
- Regression detection.

Backing: post-V1. Mention here so the brief is complete; Phase 80+ would address.

### Artifacts view (ANALYZE)

Browse the artifact store:

- Filter by mime type, size, identity, session, source-task, time.
- Per-row: filename, size, age, source-task, content-hash.
- Click → preview (per-mime-type renderer; see playground for the renderer types).
- Download / share / delete (admin-only).
- "Where used" → which sessions / tasks reference this artifact.

Backing: Phases 17–19 + Phase 73.

### Settings view (SETTINGS)

Console-wide + per-user settings:

- **Connected runtimes**: list of attached runtimes, add new, remove.
- **API tokens**: per-user OAuth bindings for tools (the user's `ScopeUser` tokens — separate UI from admin's `ScopeAgent` setup in the Agents view).
- **Theme**: light / dark / system.
- **Notifications routing**: which notification types should email / Slack / web-push the user.
- **Keybindings**: customisable shortcuts.
- **Density**: comfortable / compact.
- **Time zone + locale**.

Backing: Console-only state + Phase 73.

## Playground / direct interaction (operator's verbal addition)

This is the **most architecturally significant extension** the operator flagged that the mockup doesn't show. Two access patterns:

1. A **top-level Playground** view (could live under OVERVIEW or as a standalone sidebar entry).
2. A **per-agent Playground** accessible from the Agent Detail view ("Open in Playground" button).

Both lead to the same surface. The Playground is a chat-style interface for direct interaction with a configured agent (or a one-off ad-hoc invocation against the raw planner/LLM/tools), with these capabilities:

### PG-1. Conversation surface

- Chat-shaped UI (message list + composer).
- Roles: user, assistant, tool, system, agent-internal.
- Streaming tokens render live as they arrive.
- Reasoning summaries / tool-call traces inline-collapsible per message.
- Per-message: timestamps, model, token usage, cost.
- Each conversation is a Harbor *session* (Phase 8) and produces canonical events / artifacts identically to a programmatic invocation — the Playground IS a session, not a side-channel.

### PG-2. File upload (multimodal input)

- Drag-and-drop + paste + file-picker for: images (PNG/JPG/WEBP/HEIC), PDFs, audio (MP3/WAV/M4A), video (MP4/WEBM), arbitrary binary (uploaded as opaque artifacts).
- Files become `ArtifactRef`s in the session's artifact store (Phases 17–19).
- LLM-edge translation (Phase 33 / D-021) maps `ArtifactRef`s to multimodal `ContentPart`s for the LLM (image-capable models receive the image; others see a text description).
- File previews in the composer before send (thumbnail / page count / duration / size).
- Per-attachment audit-redaction pass before send (Phase 3) — even Playground inputs are redacted into events.

### PG-3. Full MCP-Apps compliance

The MCP spec includes "MCP Apps" — server-sent rich UI primitives that go beyond plain text:

- `EmbeddedResource` — server attaches a resource the client should render inline.
- `ResourceLink` — server returns a URI the client may fetch / preview.
- `ImageContent` / `AudioContent` — typed binary content with explicit MIME.
- `prompt` returns — interactive prompt definitions the server wants the LLM/UI to surface.
- Future MCP-Apps additions (the spec is evolving): server-sent UI fragments, structured forms, interactive widgets.

The Console MUST render every typed content shape Harbor's MCP driver (Phase 28) lowers from MCP into `ToolResult.Value`:

- `string` → markdown render with code-block highlighting + link preview.
- `ImageRef` → inline image with download fallback.
- `AudioRef` → audio player with download fallback.
- `LinkRef` → preview card.
- `EmbeddedRef` → recursive render dispatched on the embedded resource's type (markdown, code, image, structured data, …).

When MCP-Apps gain new content types, the Console renderer registry adds one renderer per type. Forbidden: bespoke per-MCP-server renderers — every MCP server's output flows through the canonical content shapes Phase 28 normalises.

### PG-4. Rich output rendering (assistant-side)

The Playground's message renderer supports:

- **Markdown** (CommonMark + GFM): tables, task lists, footnotes, math (KaTeX), Mermaid diagrams.
- **Code blocks** with syntax highlighting (highlight.js / Shiki), per-block copy, per-block "open in editor".
- **JSON / YAML / TOML** with collapsible tree view.
- **CSV / TSV** with sortable / filterable table view (paginated for large files).
- **Tool-call traces** as collapsible cards: tool name, args (redacted), result (renderer-dispatched), latency, cost, identity.
- **Streaming indicators**: cursor / typing dots while a stream is open.
- **Citations**: structured citation rendering for tools that produce them (web search, document QA).
- **Artifact references**: when an `ArtifactRef` appears in output, render as a preview card with "open full" → goes to Artifacts view.
- **Diff view**: when an output is "X vs Y", render a side-by-side or inline diff (useful for plan revisions).

### PG-5. Controls + tooling

- **Model selector** — pick from configured models in the agent's allowed list (or all models in dev mode).
- **Reasoning effort** slider for thinking-class models.
- **Tool toggle**: temporarily disable a tool for this session (testing the planner without one source).
- **Temperature / top-p** (in dev mode).
- **System prompt override** (gated; not all agents permit).
- **Run as another identity** (admin only — impersonation, with full audit; the request's `(actor=admin, requester=admin, impersonating=user_id)` is captured).
- **Save session** / **share session** (read-only link) / **fork session** (replay from a cursor with edits) / **export transcript** (markdown / JSONL).
- **Drift mode** — Console deliberately mutates a fork's history (edit a past user message) and re-plays from that point. Useful for prompt engineering.

### PG-6. Side-by-side comparison

- Open two Playgrounds with the same input, different agents / models / planners — compare outputs in parallel columns. Useful for evaluation work; foreshadows the Evaluations view.

### PG-7. Trace toggle

A "show traces" toggle that overlays the Topology view (LR-1) inline with the chat — every assistant message expands to a mini topology of the run that produced it. Brings the Live Runtime view into the Playground without leaving the chat.

### Constraints on the Playground

- **Identity is mandatory** — Playground sessions carry the operator's identity (or the impersonated identity); no anonymous Playground.
- **Audit is uniform** — Playground produces canonical events identical to programmatic invocations. CLAUDE.md §13 forbids parallel implementations; the Playground is a *client* of the Protocol, not a side channel.
- **Cost is uniform** — Playground sessions count against the operator's identity ceilings (Phase 36a). No "free dev mode" that bypasses ceilings.
- **The Playground does not bypass policy** — tool calls go through `ToolPolicy`, identity flows, audit redaction runs. The only Playground privileges are the developer-scope toggles (system prompt override, temperature exposure, impersonation) which themselves require admin scope.
- **File uploads route through the artifact store** before becoming LLM input. Heavy uploads (≥ heavy-output threshold) are materialised via D-022, not inlined. Phase 32's safety net (D-026) catches if a Playground submission ever has raw bytes reaching the LLM-client edge.
- **MCP-Apps content is rendered, not executed.** The Console renders embedded resources / image / audio / link as inert content; if an MCP server returns an "interactive form" descriptor, the Console renders the form UI and posts the form's submitted values as the next tool call's args. The form payload itself is data, not code.

## Cross-cutting features (touch every view)

### CC-1. Multi-runtime context

The "Local Runtime • Running" indicator in the sidebar implies multi-runtime support:

- The Console can connect to N runtimes simultaneously (a local dev runtime + a staging runtime + a production runtime).
- A runtime-switcher in the top-left chooses the active context.
- Each view's data is scoped to the active runtime.
- Cross-runtime fleet view is a separate "All Runtimes" mode (admin only) that aggregates over connected runtimes.

Protocol surface: each runtime is a separate Protocol endpoint. The Console manages connection state per endpoint.

### CC-2. Identity-aware UI

Every view respects the JWT's identity scope:

- Tenant-scoped users see only their tenant's data.
- Admin-scoped users see fleet across tenants (with an explicit "elevated view" indicator).
- User-scoped users see only their own sessions / artifacts / memory / tokens.
- Per-feature gates (impersonation, agent management, OAuth admin) require admin scope.

CLAUDE.md §6 makes this mandatory — the Console enforces UI gates *and* the Protocol enforces server-side; the UI gates are convenience (don't show buttons that would 403), not security.

### CC-3. Notifications

Notification center backing:

- Each notification is a Protocol-emitted event of type `notification.*` carrying severity, identity scope, summary, deep-link.
- The Console subscribes to its own user's notification topic on connection.
- Routing config (email / Slack / web-push) is per-user (Settings view).
- Notification triggers (a non-exhaustive starter list):
  - `governance.budget_exceeded` (Phase 36a)
  - `tool.auth_required` (Phase 30)
  - `tool.approval_required` (Phase 31)
  - `task.failed` (with severity above a threshold)
  - `agent.credentials_expired` (Phase 30 / brief 09)
  - `runtime.health_degraded`
- Each notification has a "snooze" / "dismiss" / "mute this trigger" set of actions.

### CC-4. Global search (⌘K)

Cross-entity quick search:

- Indexes: session IDs (and metadata), task IDs, agent names, tool names, flow names, MCP server names, artifact filenames, event types, user/tenant identifiers.
- Result types render with type-specific previews (e.g. a session result shows agent + status + age).
- Keyboard navigation; recent searches; pinned searches.
- Backing: a Console-side index built from the Protocol's catalog endpoints + a per-runtime "search" Protocol method (not currently planned). **Open question**: is the search index Console-side (build by polling) or runtime-side (a Protocol search endpoint)? Recommendation: runtime-side for sessions/tasks (cardinality is high); Console-side for slow-moving catalog data (tools / agents / flows / MCP servers).

### CC-5. Keyboard navigation

The mockup shows `⌘K` for search. Likely a fuller keyboard surface:

- `g s` → Sessions, `g t` → Tasks, `g a` → Agents, etc. (Vim-style "go to" prefixes).
- `j` / `k` → next / previous row in a list.
- `Enter` → drill into selection.
- `Esc` → close panel / clear search.
- `p` → pause live updates; `Space` (in stream) → pause/resume.

Settings view exposes the bindings (per Skills `keybindings-help`).

### CC-6. Theme / density / accessibility

- Light / dark / system theme (tokens-driven; CLAUDE.md §4.5 §3).
- Comfortable / compact density.
- WCAG AA color contrast in both themes (tested in `npm run check`).
- Reduced-motion support (GSAP animations respect `prefers-reduced-motion`).
- Keyboard-only navigation works for every action (no mouse-only paths).

## Phase-mapping summary (which feature ships when)

| Feature cluster | Protocol surface | Phase(s) |
|---|---|---|
| Topology view + event stream + per-task drill-down (LR-1, LR-5, LR-6) | Subscription + topology projection + per-task state | 5, 6, 60, 72, 74, 56 |
| Session detail panel (LR-4) | State snapshot + per-session metadata | 7, 8, 72, 73 |
| Tabs: Topology / Timeline / Metrics / Health (LR-2) | Metrics + traces + health snapshot | 55, 56, 72 |
| Interventions queue (LR-4 sub-panel) | Pause/resume coordinator + control surface | 50, 53, 54, 72 |
| Sessions view | Session list + filters | 8, 72 |
| Tasks view | Task list + filters | 20, 21, 72 |
| Agents view | Agent management surface (no current phase) | **new phase or expansion of 73** |
| Tools view | Catalog browse + tool detail | 26, 72 |
| Events view | Event log + replay + filters | 6, 57, 60, 72 |
| Background Jobs view | Task list filtered to background | 20, 21, 72 |
| Flows view | Flow descriptor exposure | 26a, 100 |
| Memory view | Memory inspector | 23, 24, 25, 73 |
| MCP Connections view | MCP provider state + OAuth config | 28, 30, 72, 73 |
| Artifacts view | Artifact list + by-ref retrieval | 17, 18, 19, 73 |
| Evaluations view | Eval surface (post-V1) | post-V1 |
| Settings view | Console-side + per-user prefs | Console-only + 73 for runtime-bound prefs |
| Playground (PG-1 .. PG-7) | Session lifecycle + multimodal + MCP-Apps content | 8, 9, 17–19, 26, 28, 32, 33, 60, 72 + **brief 10 (code-mode)** for code-mode planner sessions |
| Notifications (CC-3) | Notification event topic (no current phase) | **new event type registration**, fits inside 72 |
| Search (CC-4) | Search endpoint(s) | **new Protocol method(s)** — fits inside 60 expansion or a follow-up phase |
| Multi-runtime (CC-1) | Per-connection state (Console-only) | Console-only |

**Two genuinely new things the master plan doesn't currently cover:**

1. **Agent management surface** (Agents view + Agent Detail + per-agent Playground entry-point). Depends on the agent-identity RFC stub (brief 09).
2. **Notification taxonomy + routing** — currently events have a fan-out shape but no "this event is also a user-facing notification" classification. The Console invents this; the runtime should formalise the seam.

Both deserve master-plan entries the Console phase plans (72–75) reference. Without them, Console phases ship with under-specified Protocol surface and the Console either fudges (Console-only invention that won't survive contact with a third-party Console implementation) or stalls.

## Open architectural questions

Listed here for the Console-phase plan authors to settle. Each blocks one or more features above.

1. **Agent as a Protocol-addressable principal.** The Agents view requires agents to exist on the Protocol with addressable IDs. This is the same question brief 09 raises for OAuth binding scopes. **RFC-territory; recommend a stub PR.**
2. **Notification taxonomy.** Events have severity but no "notify the user" classification. Either add a `Notify` annotation on event types or introduce a separate `notification.*` topic. **Recommend: separate topic, populated by a runtime-internal "event → notification" mapper for the small subset of event types that surface to users.**
3. **Search index location.** Console-side polling-built vs. runtime-side endpoint. **Recommend: hybrid; runtime owns high-cardinality search (sessions, events, tasks), Console owns slow-moving catalog search (tools, agents, flows, MCP servers).**
4. **Topology projection granularity.** Per-run vs per-session. **Recommend: per-run primary; per-session as an aggregate composed in the Console.**
5. **Playground session priority + isolation.** Playground sessions count toward identity ceilings. Should they have a separate "dev session" tag so cost dashboards can split? **Recommend: yes, expose as `session.kind = dev | production`. Add the `Kind` field to the session metadata; doesn't change existing acceptance criteria.**
6. **Multi-runtime context — per-runtime auth.** Each connected runtime is its own auth context (its own JWT). The Console manages N tokens. Does the Console store them encrypted at rest in browser localStorage / IndexedDB? **Recommend: yes; use the WebCrypto API; document the threat model (anyone with browser access can decrypt).**
7. **Cross-runtime fleet view scope.** Is fleet view a Console-side aggregator (it polls N runtimes and merges) or a server-side responsibility (one runtime acts as gateway)? **Recommend: Console-side aggregator for V1; gateway is post-V1.**
8. **Console-rendered MCP-Apps that include arbitrary HTML.** Some MCP servers may return raw HTML / SVG fragments. The Console MUST sanitise (DOMPurify or equivalent) — but operators may want to opt into raw rendering for trusted servers. **Recommend: default-deny; explicit per-source trust toggle in MCP Connections view, with a warning banner when enabled. Audit when toggled.**
9. **Per-tenant theming.** Some operators may want their Console scoped to their brand. **Recommend: defer to post-V1; the design-tokens architecture supports it cleanly when it lands.**
10. **Read-only mode for shared session links.** Phase 8 sessions are owner-scoped. A "share read-only link" feature requires either issuing a scoped JWT or a session-share token surface. **Recommend: separate Protocol primitive (share-token); post-V1.**

## What this brief does NOT do

- It does not propose a Console design (visual / interaction / motion). The mockup is the reference; design is downstream.
- It does not pick component implementations (which Skeleton primitives back which views).
- It does not write the SvelteKit route tree.
- It does not propose specific Protocol method signatures.
- It does not enumerate every event type the Console will subscribe to — the canonical event taxonomy lives in Harbor's `events` package; the Console subscribes to all of it.
- It does not settle the agent-identity RFC question; that's brief 09's territory and an RFC-PR's job.

## Findings summary

- ✓ The Live Runtime view in the mockup decomposes into 6 feature clusters (LR-1 .. LR-6), each backed by Protocol surface already covered by Phases 5–6, 50–54, 60, 72–74.
- ✓ 13 sidebar entries imply a much richer Console feature surface than Phases 72–75 currently spec. Three views (Agents, Memory inspector deep, MCP Connections control plane) need additional Protocol work or new phase entries.
- ✓ The Playground (PG-1 .. PG-7) is the operator's largest additive feature. It is a Protocol *client* (uses existing session / artifact / event / tool / MCP surfaces); it is not a runtime side channel.
- ✓ Full MCP-Apps content rendering composes cleanly with Phase 28's content-shape normalisation — the Console adds one renderer per canonical shape, and new MCP content types ride the same pipe.
- ⚠ Notification taxonomy and Search endpoints are net-new Protocol surface the master plan does not currently cover.
- ⚠ Agent management is the largest semantic gap — the Agents view assumes a first-class agent principal that doesn't yet exist (brief 09 flags the same blocker).
- ⚠ Multi-runtime context is Console-only state but introduces an auth-storage question (per-runtime JWTs in browser storage) that needs an explicit design decision.
- ⚠ The session "Priority" field shown in the mockup is not currently a Harbor primitive. Recommend dropping from V1 unless real load patterns demand it.
- ✗ The Evaluations view is out of V1 scope and stays out; the sidebar entry is a placeholder.

## Source artifacts referenced

- Mockup PNG: `docs/research/console-mockup-runtime-view.png` (saved 2026-05-12).
- CLAUDE.md §4.5 — Console / Protocol-client conventions (binding).
- RFC §5 (Harbor Protocol), §7 (Console — observability + control plane UI).
- Master-plan phases 5–6 (events), 7–8 (state + sessions), 17–19 (artifacts), 20–21 (tasks), 26–29 (tools), 30 (OAuth — brief 09), 32–34 (LLM), 36a (cost), 50–54 (pause/resume + steering), 55–56 (telemetry), 60–62 (Protocol), 72–75 (Console).
- Brief 09 (MCP OAuth — agent identity territory).
- Brief 10 (code-mode — implies Topology + Playground extensions for code-mode programs).
- Skills referenced (Console motion / animation): `frontend-design`.

## Re-discussion checklist

When the Console phase plans (72–75) are authored — or when an "Agent management surface" phase is added — return to this brief and confirm:

- [ ] Agent-identity RFC stub landed (shared with brief 09's blocker).
- [ ] Notification taxonomy decided (separate `notification.*` topic recommended).
- [ ] Search-index location decided (hybrid recommended).
- [ ] Topology projection granularity decided (per-run primary recommended).
- [ ] Session `Kind` field added if Playground sessions need cost-dashboard separation.
- [ ] Multi-runtime auth-storage threat model documented.
- [ ] MCP-Apps content renderer registry sketched; raw-HTML opt-in toggle scoped.
- [ ] Agent management surface scoped (own phase or expansion of 73).
- [ ] Playground phase added to the master plan (currently no entry).
- [ ] Confirm CLAUDE.md §4.5 conventions are referenced in each Console phase plan.
- [ ] Open question on whether the "Priority" field is added to session metadata or dropped from the UI.
