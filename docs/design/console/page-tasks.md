# Console page — Tasks

**Slug:** `tasks` &middot; **Sidebar cluster:** Execution &middot; **Route:** `/console/tasks`
**Mockup:** `docs/rfc/assets/console-tasks-page.png` (canonical, 2026-05-18)

## 1. Purpose

Tasks is the task-granularity counterpart to Sessions. A session is a long-lived multi-turn execution record; a task is a single Run inside it (or a background task spawned from one). Tasks answers questions one notch below Sessions: "what's running across all sessions right now?", "every task that failed in the last hour with a `tool.failed` event," "the pending background jobs the planner spawned via `SpawnTask` that haven't joined yet" (D-047). The page is a Kanban-shaped board by default (Pending / Running / Done columns) for at-a-glance pulse, with a list/table mode toggle for filtered analysis, and a per-task drilldown that shares the same Details / Input / Output / Logs panel Live Runtime uses.

## 2. Where it sits in the IA

Tasks sits second under the **Execution** cluster (Execution → Sessions, Tasks, Agents, Tools, Events, Background Jobs). It is reached from the Overview counters bar's "Tasks Running" link, from the Sessions detail's tasks-count link, from the Agents page's recent-tasks rollup, from the Background Jobs page (its filter is "background only"), and from the global search palette. Drilldown opens a task detail page or a side-panel; from there the operator can "Open parent session in Live Runtime" or "Cancel this task." Breadcrumb: `<runtime> / Tasks` (board) and `<runtime> / Tasks / <task-id>` (detail).

## 3. Functionality matrix

- **Kanban board view — Pending / Running / Paused / Done / Failed columns; per-card task summary; live drag-render as cards transition status.** `[wave-13-extends]` Requires a `tasks.list` Protocol method (NEW) with filter and cursor pagination, plus live event-bus deltas on `task.spawned` / `task.started` / `task.paused` / `task.resumed` / `task.completed` / `task.failed` / `task.cancelled` (all `[shipped]` in the event taxonomy — `tasks.TaskSpawnedPayload`, `tasks.TaskStartedPayload`, `tasks.TaskPausedPayload`, `tasks.TaskResumedPayload`, `tasks.TaskCompletedPayload`, `tasks.TaskFailedPayload`, `tasks.TaskCancelledPayload`).
- **List / table mode toggle — same data, virtualised table for filter-driven analysis.** `[wave-13-extends]` Same `tasks.list` data; client-side render shape.
- **Per-card / per-row metadata — task id, parent session, type (foreground / background), status, started, duration, identity (truncated), error class (when Failed), tool count, parent task id (when spawned-by-planner).** `[wave-13-extends]` `tasks.list` payload shape.
- **Filters — status, type (foreground / background), source / agent, latency-above, identity, error-class, started-in-window, parent task id (drill into a `SpawnTask` group).** `[wave-13-extends]` `tasks.list` query payload.
- **Free-text search.** `[wave-13-extends]` `search.tasks` Protocol method (NEW per Brief 11 §CC-4 — tasks are high-cardinality, runtime-side).
- **Per-task detail panel — Details / Input / Output / Logs.** `[wave-13-extends]` Same `tasks.get` + `state.history` panel Live Runtime uses (Phase 73 `Pending`). Details = task metadata + parent session + agent + planner snapshot at spawn time.
- **Per-task event log strip — every `task.*` and `tool.*` event for this task id, time-ordered.** `[shipped]` Subscribe filtered to `(tenant, user, session, run=task.RunID)` via the events bus.
- **Per-task control history — submitted controls (Cancel / Pause / Resume / Prioritize / Inject / Redirect / User-message / Approve / Reject).** `[shipped]` Subscribe to `control.received` / `control.applied` / `control.rejected` filtered to the task's run id.
- **Per-task interventions — pending pauses on this task.** `[shipped]` Same surfaces as Live Runtime — `pause.requested` / `pause.resumed` / `tool.approval_*` / `tool.auth_*` events.
- **Bulk actions — Cancel selected, Pause selected.** `[shipped]` `cancel` / `pause` Protocol methods per row.
- **Per-task patches (the Phase 21 `task.patch_applied` / `task.patch_rejected` events).** `[shipped]` Subscribe; render in the per-task detail's history.
- **TaskGroup view — when a task spawned a group via `SpawnTask`, show the group's members + their `task.group_resolved` / `task.group_cancelled` outcomes.** `[shipped]` Subscribe to `task.group_created` (`tasks.TaskGroupCreatedPayload`), `task.group_sealed` (`tasks.TaskGroupSealedPayload`), `task.group_resolved` (`tasks.TaskGroupResolvedPayload`), `task.group_cancelled` (`tasks.TaskGroupCancelledPayload`).
- **Per-task cost — total tokens + dollar amount derived from `llm.cost.recorded` events scoped to the task.** `[shipped]` Client-side aggregation.
- **"Open parent session in Live Runtime."** `[shipped]` Local navigation.
- **"Open parent task" (for child tasks spawned by `SpawnTask`).** `[shipped]` Local navigation.
- **Prioritize a task (raise / lower numeric priority).** `[shipped]` `prioritize` Protocol method (`types.ControlRequest` with `Payload.priority`). Task-level priority is a `[shipped]` runtime concept; session-level priority is `[deferred]`.
- **No Priority field rendered at the SESSION-card level on this page.** `[deferred]` D-065 dropped session-level priority from V1. Task-level priority is shipped via `task.prioritised` event (`tasks.TaskPrioritisedPayload`) and the `prioritize` Protocol method — those are RENDERED on the task card.
- **Background-acknowledged badge for tasks the planner abandoned per `task.background_acknowledged`.** `[shipped]` `tasks.TaskBackgroundAcknowledgedPayload`.
- **Saved filter chips.** `[shipped]` Console-local per D-061.

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page, board mode):
  - Row 1 — filter bar + saved-filter chips + search box + mode toggle (Board / List).
  - Row 2 — five-column Kanban board (Pending / Running / Paused / Done / Failed) with virtualised columns.
- **Main canvas** (per-page, list mode):
  - Row 1 — filter bar + saved-filter chips + search box + mode toggle.
  - Row 2 — tasks table (virtualised, cursor pagination).
- **Main canvas** (per-page, detail mode):
  - Row 1 — task detail header (id + parent session link + agent link + status + started + duration).
  - Row 2 — tab strip: Details | Input | Output | Logs | Events | Control History | Interventions | Group (when applicable).
  - Row 3 — selected tab content (full canvas).
- **Right rail** (per-page, detail mode): Parent session card; parent task card (when child); cost rollup for this task.
- **Bottom dock** (per-page): empty.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Kanban board | `tasks.list` (NEW) + `task.*` event deltas | Drag → not supported (read-only board); click card → detail | `[wave-13-extends]` |
| Tasks table (list mode) | `tasks.list` (NEW) | Sort (local UI state); click row → detail | `[wave-13-extends]` |
| Filter bar | local UI state → `tasks.list` query | Apply / Clear | `[wave-13-extends]` |
| Search box | `search.tasks` (NEW) | Submit | `[wave-13-extends]` |
| Saved-filter chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Task detail header | `tasks.get` (NEW Phase 73 method) | Copy id; open parent session; open parent task | `[wave-13-extends]` |
| Details tab | `tasks.get` | local UI state | `[wave-13-extends]` |
| Input tab | `tasks.get` (post-redaction) | Copy as JSON (local) | `[wave-13-extends]` |
| Output tab | `tasks.get` (post-redaction); `ArtifactStub` → `artifacts.get_ref` (NEW) | Open artifact (deep-link) | `[wave-13-extends]` |
| Logs tab | `state.history` (NEW Phase 73 method) | Scroll / copy / show-debug | `[wave-13-extends]` |
| Events tab | `events.EventBus` subscription filtered by `(tenant, user, session, run)` | Export JSONL | `[shipped]` |
| Control History tab | `control.received` / `control.applied` / `control.rejected` events filtered to run | Click row → expand payload | `[shipped]` |
| Interventions tab | `pause.requested` / `pause.resumed` / `tool.approval_*` / `tool.auth_*` events filtered to run | "Resume" → `resume`; Approve → `approve`; Reject → `reject` | `[shipped]` |
| Group tab | `task.group_created` / `task.group_sealed` / `task.group_resolved` / `task.group_cancelled` events | Click member → task detail | `[shipped]` |
| Cost rollup (right rail) | `llm.cost.recorded` events aggregated client-side per task | none | `[shipped]` |
| Cancel button | `cancel` Protocol method | Submit `types.ControlRequest` | `[shipped]` |
| Pause / Resume buttons | `pause` / `resume` Protocol methods | Submit | `[shipped]` |
| Prioritize control | `prioritize` Protocol method | Submit `types.ControlRequest` with `Payload.priority` | `[shipped]` |
| Bulk-action toolbar | `cancel` / `pause` per selected task | Iterate per row | `[shipped]` |

## 6. Controls + actions

- **Toolbar:** mode toggle (Board / List); filter bar; saved-filter chips; search box; bulk-select.
- **Card-action (board):** click → detail; right-click → Cancel / Pause / Prioritize.
- **Row-action (list):** click → detail; bulk-select → Cancel / Pause.
- **Panel-action (detail tabs):** copy / export / open-artifact / approve-reject.
- **Keyboard shortcuts:** `g t` go to Tasks; `j` / `k` next / previous; `Enter` open detail; `Esc` back; `c` Cancel selected; `p` Pause selected (gated on control claim).

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty board | No tasks for the scope | Empty-state illustration + "Start a run in Live Runtime" CTA | CTA |
| Filtered empty | Filters yield zero | "No tasks match these filters" + "Clear filters" | Clear |
| Initial loading | `tasks.list` in flight | Skeleton columns / rows | Auto |
| Pagination loading | Cursor next-page | Spinner inside the column / at row tail | Auto |
| Protocol error — `CodeInvalidRequest` | Malformed filter | Inline on filter bar | Adjust |
| Protocol error — `CodeNotFound` on detail | Task id missing | "Task not found"; back link | Back |
| Protocol error — `CodeScopeMismatch` on bulk action | Operator submitted Cancel / Pause / Prioritize without control claim | Inline error per row; partial completion shown | Request elevated scope |
| Protocol error — `CodePayloadInvalid` on Prioritize | Out-of-range priority | Inline error on the priority composer | Adjust |
| Protocol error — `CodeIdentityRequired` / `CodeAuthRejected` | Identity / auth dropped | Banner + recover | Re-attach |

## 8. Multi-tenant / multi-runtime nuances

Tasks lists are tenant-scoped by default — every `tasks.list` query carries the operator's `(tenant, user, session)` and the runtime `WHERE`-clauses by tenant per CLAUDE.md §6. With `admin`, the tenant facet appears in the filter bar and cross-tenant queries elevate the subscription (with `audit.admin_scope_used` emit on the server). In multi-runtime mode, the runtime switcher picks the active runtime; tasks are not cross-runtime aggregated in V1.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — list / inspect own tasks; render their event / control history.
- `admin` — cross-tenant list; fan-in event subscription across sessions.
- `console:fleet` — post-V1 cross-runtime aggregator.
- **Control-plane verbs (Cancel / Pause / Resume / Prioritize / bulk variants)** require the more-elevated control claim per D-066.

## 10. Out of V1 (deferred)

- **Session-level priority on parent-session badges.** D-065 — only task-level priority ships. (Task-level Prioritize stays in V1.)
- **Cross-runtime task aggregator.** D-091 — post-V1.
- **"Replay task" surface (re-run a completed task with mutations).** Brief 11 §PG-5 — post-V1 (foreshadows Evaluations, D-064).
- **Drag-to-reorder priority on the board.** UI choice not supported — the priority control surfaces via the explicit `prioritize` method, not drag-drop.

## 11. References

- Brief 11 §"Tasks view", §"Per-task detail pane" (shared component with Live Runtime).
- Brief 12 §"The two-surface model".
- RFC-001-Harbor.md §5.2 (state snapshots), §6.8 (Tasks unified foreground/background), §6.13 (events), §7 (Console).
- Decisions: D-006 (background-task persistence in-process at V1), D-030 (TaskRegistry surface split), D-032 (wake-on-resolution = planner-concrete), D-047 (`SpawnTask` / `AwaitTask` / `RequestPause` shapes), D-061 (Console DB local-only), D-062 (14-page IA), D-065 (no session priority), D-066 (control claim), D-072 (Protocol task control surface).
- Phase plan: phase 20 (TaskRegistry — `Shipped`), phase 21 (TaskGroup + retain-turn + patches — `Shipped`), phase 54 (task control surface — `Shipped`), phase 73 (state inspection — `Pending`).
- Glossary terms used: `TaskRegistry`, `GroupCompletion`, `Console`, `Scope claim`, `Fleet control / fleet observation`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-tasks-page.png` against §3-§7.

### Refinements to §4 page anatomy

- **Sub-header strip**: filter chips — `Saved filters` + `Status` + `Identity` (admin) + `Date range: Last 24h` + `More filters` + free-text search + `Refresh` + `Export`. Plus list/board view-mode toggle.
- **Main canvas — Kanban-style 4-column board**: Pending / Running / Paused / Failed (the mockup also shows what looks like a 5th implicit "Done" but the visible columns are these 4). Each card carries: task id (truncated) / type icon / identity chip / duration / parent session id / sub-task count. Cards are draggable across status columns (operator-driven control; back-end calls the matching control method — `pause` to move into Paused, `resume` to move out, `cancel` to move to Failed). Each card has a quick-action menu (⋯).
- **Selected-task detail bar** (between board and bottom dock): id / status / identity / started / duration / **`Pause` / `Resume` / `Cancel` / `Prioritize` / `Approve` / `Reject`** action buttons (control-scope gated per D-066).
- **Bottom dock**: tabs **Task metadata | Inputs | Logs | Events | Errors | Output**. Output tab renders artifact-card stubs (mime + size + preview when small + presigned-URL Download button).
- **Right rail**: Summary card (id, status, started, duration, parent session, cost) + **Parent Session** card (clickable card to navigate to session detail) + **Cost Breakdown** card (per-step cost) + Recent Activity + Recent Artifacts.
- **Footer**: standard `Connected to | Protocol v | Events Stream | Console v`.

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Kanban card-drag-to-status-column | local UI gesture + control-method invocation | Drag Pending → Running (no-op; Pending → Running is server-controlled); Drag Running → Paused (invokes `pause`); Drag Running → Failed (invokes `cancel`) | `[shipped]` (the underlying control methods); `[wave-13-extends]` (the per-task list query `tasks.list` that populates columns) |
| Selected-task action bar | local selection + control-method invocations | All 6 buttons → matching Protocol method; gated on control-scope per D-066 | `[shipped]` |
| Parent Session card (right rail) | `events.Event.Identity` of the selected task + `sessions.inspect` (NEW) | Click → Sessions page detail | `[wave-13-extends]` |
| Cost Breakdown card (right rail) | `llm.cost.recorded` events aggregated per planner step within this task | Local UI state (chart vs table) | `[shipped]` |
| Errors tab (bottom dock) | filtered Events stream to `*.failed` / `*.error` types within this task | Click row → detail | `[shipped]` |
| Output tab (bottom dock) | `tasks.get` result field + `ArtifactStub` references resolved via `artifacts.get_ref` (NEW) | Click artifact → preview | `[wave-13-extends]` |

### No mockup violations of binding carve-outs

- **D-065** — Task-level priority (the `prioritize` control) IS shipped per D-072; the Kanban + the per-task Prioritize button render this. Session-level priority is NOT rendered anywhere. Mockup honors the carve-out.
- **D-061** — task list / detail / cost all source from Protocol; saved filters are Console-local.
- **D-066** — Pause/Resume/Cancel/Prioritize/Approve/Reject buttons are control-scope-gated; mockup shows elevated-operator view.
- **D-047** — Kanban columns include Pending (covers spawned-but-not-acquired) — matches D-047's TaskRegistry state machine.
