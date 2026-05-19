# Console page — Background Jobs

**Slug:** `background-jobs` &middot; **Sidebar cluster:** Execution &middot; **Route:** `/console/background-jobs`
**Mockup:** `docs/rfc/assets/console-background-jobs-page.png` (canonical, 2026-05-18)

## 1. Purpose

Background Jobs is the queue view for long-running tasks that don't belong to a foreground session — the work the planner spawned via `SpawnTask` (D-047) that the user isn't actively waiting on. Examples: a background indexer that crawls and embeds a corpus, a scheduled report-generation task, a long-poll-shaped wait against an external service, an `AwaitTask` join that's still pending. The page answers: "what's still running in the background?", "what's the ETA on the longest-running job?", "did the planner's `SpawnTask` ever join via `AwaitTask`, or did it become an orphan?". Where Tasks renders the full taxonomy with a Kanban shape, Background Jobs is a focused queue UI for the background subset with cancel / requeue / retry actions and per-job progress drilldown.

## 2. Where it sits in the IA

Background Jobs sits last under the **Execution** cluster (Execution → Sessions, Tasks, Agents, Tools, Events, Background Jobs). It is reached from the sidebar, from the Overview footer counter "Background Jobs," and from any Task detail's "View as background job" link. Internally it is a filtered projection of the Tasks page — same `tasks.list` Protocol surface with `type=background` filter — but renders with queue-shaped affordances (priority badge per task, ETA, cancel-bulk, requeue). Breadcrumb: `<runtime> / Background Jobs`.

## 3. Functionality matrix

- **Queue list — all background tasks across all sessions in scope, default sort by priority then started.** `[wave-13-extends]` Filtered projection of `tasks.list` Protocol method (NEW; see page-tasks.md) with `type=background`. Live deltas via `task.spawned` / `task.started` / `task.background_acknowledged` / `task.completed` / `task.failed` / `task.cancelled` events.
- **Per-row metadata — job id, type (e.g. "indexer", "report", "long-poll"), status, started, ETA (when known), # related sessions, parent task / spawned-by, priority.** `[wave-13-extends]` `tasks.list` payload extended with `IsBackground` boolean (today the `task.background_acknowledged` event signals the planner abandoned the task to background — `tasks.TaskBackgroundAcknowledgedPayload`).
- **Filters — status, type, identity, age, priority, has-pending-await.** `[wave-13-extends]` `tasks.list` query payload.
- **Free-text search.** `[wave-13-extends]` `search.tasks` Protocol method (NEW per page-tasks.md).
- **Per-job detail — Details (same shape as Tasks detail), Progress (custom for background — artifacts produced so far, sub-task progress, ETA), Events, Control History.** `[wave-13-extends]` `tasks.get` (NEW Phase 73 method); progress card aggregates `artifacts.list` filtered to the job + `task.group_resolved` count when grouped.
- **Cancel job.** `[shipped]` `cancel` Protocol method.
- **Pause job.** `[shipped]` `pause` Protocol method.
- **Resume job.** `[shipped]` `resume` Protocol method.
- **Prioritize job (raise / lower numeric priority).** `[shipped]` `prioritize` Protocol method (`types.ControlRequest` with `Payload.priority`); emits `task.prioritised` (`tasks.TaskPrioritisedPayload`).
- **Retry / Requeue — re-spawn the job with the same inputs.** `[wave-13-extends]` Invoke `start` Protocol method (shipped) but the original `Query` / `Description` is fetched via `tasks.get` (NEW Phase 73 method) — the bullet's net feasibility depends on Wave 13. The new job id is minted by the runtime.
- **Bulk actions — Cancel selected, Pause selected, Prioritize selected.** `[shipped]` Per-row method invocations.
- **`AwaitTask` orphan detector — highlight background jobs whose parent task is no longer alive (a planner `SpawnTask` that was never joined via `AwaitTask`).** `[wave-13-extends]` Derived from `tasks.list` (parent-task field) cross-checked against the runtime's active-task set; surfaces the §13 binding rule that `SpawnTask` + `AwaitTask` MUST emit in the same phase (Phase 47 / D-056 closed this for ReAct).
- **Per-job artifacts-so-far rollup.** `[wave-13-extends]` `artifacts.list` (NEW Phase 73 method) filtered to job's `(tenant, user, session)` + the job's run id.
- **No Priority field at the SESSION level on parent-session badges.** `[deferred]` D-065 dropped session-level priority from V1. Task-level priority IS rendered on Background Jobs rows (via `task.prioritised` event and the `prioritize` Protocol method) — this is the task / job level, not session level.
- **Saved filter chips (e.g. "background jobs older than 1h").** `[shipped]` Console-local per D-061.

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page, queue mode):
  - Row 1 — filter bar + saved-filter chips + search box + Cancel-all-selected button (control-claim gated).
  - Row 2 — queue list (virtualised; per-row priority badge, ETA, age, identity, parent indicator).
- **Main canvas** (per-page, detail mode):
  - Row 1 — job detail header (id + parent session link + parent task link when child + status + started + ETA).
  - Row 2 — tab strip: Details | Progress | Events | Control History.
  - Row 3 — selected tab content.
- **Right rail** (per-page, detail): Artifacts-so-far rollup + Parent task card + Related sessions list.
- **Bottom dock** (per-page): empty.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Queue list | `tasks.list` (NEW) with `type=background` + live `task.*` deltas | Click row → detail; bulk-select | `[wave-13-extends]` |
| Filter bar / search | local UI state → `tasks.list` query / `search.tasks` (NEW) | Apply / Submit | `[wave-13-extends]` |
| Saved-filter chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Job detail header | `tasks.get` (NEW) | Copy id; click parent session → Live Runtime / Sessions; control buttons → method calls | `[wave-13-extends]` |
| Details tab | `tasks.get` | local UI state | `[wave-13-extends]` |
| Progress tab | `artifacts.list` filtered + `task.group_resolved` events when grouped + custom ETA model | local UI state | `[wave-13-extends]` |
| Events tab | `events.EventBus` filtered to job's run id | export JSONL | `[shipped]` |
| Control History tab | `control.received` / `control.applied` / `control.rejected` events | expand payload | `[shipped]` |
| Artifacts-so-far rollup (right rail) | `artifacts.list` (NEW) filtered | Click artifact → Artifacts page preview | `[wave-13-extends]` |
| Cancel / Pause / Resume / Prioritize buttons | `cancel` / `pause` / `resume` / `prioritize` Protocol methods | Submit `types.ControlRequest` | `[shipped]` |
| Retry / Requeue button | `start` Protocol method | Submit `types.StartRequest` cloned from original | `[shipped]` |
| Bulk-action toolbar | per-row method invocations | Iterate per row | `[shipped]` |
| Orphan detector | `tasks.list` + active-task cross-check (Console-side) | Click orphan badge → diagnostic dialog | `[wave-13-extends]` |

## 6. Controls + actions

- **Toolbar:** filter bar + saved-filter chips + search box + bulk-select toggle.
- **Row-action (list):** click → detail; right-click → Cancel / Pause / Prioritize / Retry.
- **Header-action (detail):** Cancel / Pause / Resume / Prioritize / Retry (all gated on control claim).
- **Keyboard shortcuts:** `g b` Background Jobs; `j` / `k` next / previous; `Enter` open detail; `Esc` back; `c` Cancel; `p` Pause; `R` Retry.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty queue | No background jobs in scope | Empty-state: "No background jobs running" + link to Tasks page | Visit Tasks |
| Filtered empty | Filters yield zero | "No jobs match these filters" + Clear | Clear |
| Initial loading | `tasks.list` in flight | Skeleton rows | Auto |
| Protocol error — `CodeNotFound` on detail | Job id missing | "Job not found"; back link | Back |
| Protocol error — `CodeScopeMismatch` on control | Operator submitted without control claim | Inline error | Request elevated scope |
| Protocol error — `CodePayloadInvalid` on Prioritize | Out-of-range priority | Inline error | Adjust |
| Protocol error — `CodeIdentityRequired` / `CodeAuthRejected` | Identity / auth dropped | Banner + recover | Re-attach |

## 8. Multi-tenant / multi-runtime nuances

Background jobs are tenant-scoped: each `tasks.list` call carries the operator's `(tenant, user, session)` and the runtime `WHERE`-clauses by tenant per CLAUDE.md §6. `admin` elevates the queue across tenants (with `audit.admin_scope_used` server-side emit). Multi-runtime swaps the queue when the runtime switcher changes. Background jobs are particularly affected by Phase 6 background-task persistence (D-006) — V1 is in-process per-runtime, so a runtime restart will lose in-flight background jobs unless the planner's checkpoint via Phase 7 StateStore covers them; the page surfaces this in the Empty state when no jobs are listed AFTER a restart.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — queue / inspect own jobs.
- `admin` — fan-in across tenants.
- `console:fleet` — post-V1 cross-runtime aggregator.
- **Control-plane verbs (Cancel / Pause / Resume / Prioritize / Retry / bulk variants)** require the control-scope claim per D-066.

## 10. Out of V1 (deferred)

- **Durable background-job persistence across runtime restarts.** D-006 — V1 in-process; durable post-V1 (Phase 87+ band).
- **Scheduled / cron-shaped jobs.** Not in V1 — background here means "planner-spawned via `SpawnTask`," not "operator-scheduled." A future scheduler would be a separate subsystem.
- **Cross-runtime background-jobs aggregator.** D-091 — post-V1.
- **Session-level priority badges on parent-session indicators.** D-065 dropped from V1.

## 11. References

- Brief 11 §"Background Jobs view".
- Brief 12 §"The two-surface model".
- RFC-001-Harbor.md §6.8 (Tasks unified foreground/background), §7 (Console).
- Decisions: D-006 (background-task persistence in-process at V1), D-030 (TaskRegistry surface split), D-047 (`SpawnTask` / `AwaitTask` / `RequestPause` shapes), D-061 (Console DB local-only), D-065 (no session priority), D-066 (control claim), D-072 (Protocol task control surface).
- Phase plan: phase 20 (TaskRegistry — `Shipped`), phase 21 (TaskGroup + retain-turn + patches — `Shipped`), phase 47 (parallel exec + ReAct emission upgrade — `Shipped`), phase 54 (task control surface — `Shipped`), phase 73 (state inspection — `Pending`).
- Glossary terms used: `TaskRegistry`, `GroupCompletion`, `Console`, `Scope claim`, `Fleet control / fleet observation`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-background-jobs-page.png` against §3-§7.

### Refinements to §4 page anatomy

- **Sub-header strip.** Saved-filter chips (`Saved filters`, `Active only`, `High-priority`, `Stuck > 1h`, `Recently failed`) followed by faceted filter chips (`Tenant` ▾, `Session` ▾, `Status` ▾ — `Queued` / `Running` / `Paused` / `Completed` / `Failed` / `Cancelled` — `Has pending approval` ▾, `More filters` ▾). Right side: `Refresh`, `Cancel selected` (bulk control, gated by `tasks.control` scope), `Pause selected` (bulk control, gated by `tasks.control` scope).
- **Main jobs table (primary surface).** Columns in mockup order: checkbox / **Job ID** (short hash + copyable) / **Title / Description** (one-line summary from the planner's spawn payload) / **Parent session** (chip with deep-link to `/console/sessions/<id>`) / **Status** chip / **Progress** mini-bar (when planner emits a numeric progress hint via the existing event stream) / **Started** (relative timestamp) / **Last activity** (relative timestamp) / **Tags** (parent task type / planner-emitted labels) / row-action menu. Virtualised; pagination footer `Page N of M | Show rows ▾`.
- **Bulk-action toolbar.** Activates when ≥1 row is checked: `Cancel`, `Pause`, `Resume`, `Prioritize` (task-level only per D-072 — session-level priority remains dropped per D-065). All four invoke shipped Phase 54 control verbs; all four require the `tasks.control` claim per D-066 and degrade to disabled-with-tooltip when missing.
- **Right rail — Selected job detail panel (full height when a row is selected).** Header: short hash + status chip + copy-id button. Tabbed sub-panels in mockup order:
  - **Details** — full `task_id`, parent `run_id` + `session_id` + identity triple, planner of origin, spawn timestamp, attempt count, last-attempt outcome.
  - **Progress** — timeline of state transitions (`Queued → Running → Paused → Resumed → …`) with timestamps; current `Progress` percentage when emitted.
  - **Logs** — `task.*` / `tool.*` events filtered to this job (links into Events page with the filter pre-applied).
  - **Pending approvals** — when the job's planner emitted `tool.approval_requested` or HITL `pause`, lists the open requests with Approve / Reject buttons (gated by `tools.approve` / `tasks.control` claims).
  - **Artifacts for this Job** — list of artifacts produced by this task with mime-icon + filename + size; clicking opens via `artifacts.get`.
  - **Related Sessions** — sibling tasks in the same `TaskGroup` (D-030 / D-047) and the parent session, with status pills.
- **Footer.** `Connected to <runtime> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`.

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Saved-filter chips (`Active only`, `High-priority`, `Stuck > 1h`, `Recently failed`) | Console-local saved views (D-061) | Apply / pin / unpin | `[Console-local]` (D-061) |
| Faceted filter chips (Tenant / Session / Status / Has pending approval / More filters) | `tasks.list` filter params | Toggle facet | `[wave-13-extends]` (extended `tasks.list` filter shape) |
| Progress mini-bar column | Planner-emitted progress events (no contract change — read from `events.subscribe`) | None | `[wave-13-extends]` (planner-progress event already on the bus; surfacing it requires a `tasks.list` enrichment) |
| Tags column (parent task type / planner-emitted labels) | `tasks.list` row metadata | None | `[wave-13-extends]` (`tasks.list` row shape extension) |
| Bulk `Cancel` / `Pause` / `Resume` / `Prioritize` | Selected row IDs | Invoke Phase 54 verb per row | `[shipped]` (`cancel` / `pause` / `resume` / `prioritize` — Phase 54) |
| Pending-approvals tab in right rail (per-job) | `tool.approval_requested` events filtered by `task_id` | Approve (`approve`) / Reject (`reject`) | `[shipped]` (`approve` + `reject` — Phase 54) |
| Artifacts-for-this-Job tab | `artifacts.list?task_id=…` | Open artifact via `artifacts.get` | `[wave-13-extends]` (`artifacts.list` filter by `task_id`) |
| Related Sessions tab (sibling tasks in same `TaskGroup`) | `tasks.list?group_id=…` | Click row → navigate to sibling job | `[wave-13-extends]` (`tasks.list` filter by `group_id`) |
| Stuck > 1h saved-filter chip | Console-local rule over `last_activity_at` | Apply filter | `[Console-local]` (D-061; pure client-side derivation) |

### No mockup violations of binding carve-outs

- **D-061 (Console DB local-only).** Saved-filter chips, the `Stuck > 1h` derivation, sort preferences, and column visibility are Console-local. The mockup never persists a Protocol-mutating shadow of background jobs — every row round-trips through `tasks.list`.
- **D-065 (no session-level priority).** The Prioritize bulk action targets `task_id` only per D-072; no session-priority field appears on rows or in filters. The `High-priority` saved-filter chip filters on task-level priority (the only V1 surface), not session-level.
- **D-066 (control-scope claims).** Cancel / Pause / Resume / Prioritize / Approve / Reject are all gated by the appropriate claim and disabled-with-tooltip when missing; observation (list, detail, logs, artifacts) requires only the read scope.
- **D-091 (`harbor console` deployment).** Footer carries Protocol + Console versions and the connected-runtime label.
- **§13 forbidden practices.** The bulk control verbs invoke the same Phase 54 Protocol methods as the task / session views (no parallel implementation); approvals route through `approve` / `reject` (no shadow approval queue); the heavy-content threshold is respected for artifact rendering (`artifacts.get` link, no inline bytes — closes D-026).
