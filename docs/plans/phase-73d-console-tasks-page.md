# Phase 73d — Console Tasks page (kanban + bulk control)

## Summary

Bundles the Tasks page Protocol surface and UI into a single phase per the Wave 13 staging (`docs/plans/wave-13-decomposition.md` §5). Net-new Protocol additions: `tasks.list` row-shape extension (priority + status + type filters + cursor pagination) and `tasks.get` enrichments (parent-session card, parent-task link, cost rollup metadata, planner-snapshot reference). UI: a kanban 4-column board (Pending / Running / Paused / Failed) + list/table mode + per-task detail tabs (Task metadata / Inputs / Logs / Events / Errors / Output) + the bulk-action toolbar. The toolbar invokes the EXISTING Phase 54 shipped control verbs (`cancel`, `pause`, `resume`, `prioritize`, `approve`, `reject`) — NO new control method is introduced. Stage 2.2 phase (parallel with 73c / 73e / 73g / 73b).

## RFC anchor

- RFC §5.2 (state snapshots row + task control row)
- RFC §6.8 (Tasks — unified foreground/background)
- RFC §7 (Console layer)

## Briefs informing this phase

- brief 11 (Console feature surface — "Tasks view", §CC-4 cross-entity search)
- brief 12 (deployment + two-surface model — shared chat library + Protocol-client posture)

## Brief findings incorporated

- brief 11 §"Tasks view": the page is "like sessions but at the task granularity. Useful for 'find every failed task in the last hour' / 'what's currently running across all sessions'." The filter shape (status, type, source, latency-above, identity, error-class) and per-row fields (task ID, parent session, type, status, started, duration, identity) this phase ships map verbatim — `tasks.list` is the Protocol surface that answers exactly those questions.
- brief 11 §CC-4: tasks are **high-cardinality runtime-side** — the runtime owns the index, not Console-side substring matching. `search.tasks` lands in Phase 72c (Stage 1); this phase consumes it from the page's search box. No Console-side fan-in matching.
- brief 11 §"Per-task detail pane": the per-task detail uses the same Details / Input / Output / Logs panel Live Runtime uses. This phase reuses that surface — `tasks.get` enrichments feed the shared detail panel, NOT a Tasks-bespoke renderer.
- brief 12 §"the two-surface model": the page is a Protocol client. NO Console-side shadow of runtime task state (D-061); the kanban columns derive from `task.*` event-bus deltas + `tasks.list` for the initial paint, never a Console DB cache.

## Findings I'm departing from (if any)

None.

## Goals

- Ship a complete, mockup-aligned Tasks page (`/console/tasks`) as a Protocol client per D-091 (served by `harbor console`, NEVER `harbor dev`).
- Land the `tasks.list` Protocol method (row-shape extension on the existing Phase 73 `tasks.get`/`sessions.inspect` cluster): cursor pagination + a filter struct keyed on status / kind / parent task / identity / time-window / error-class / latency-above.
- Enrich `tasks.get` with the parent-session reference, parent-task reference, cost-rollup metadata, and planner-snapshot reference required by the per-task detail and right-rail cards.
- Wire the bulk-action toolbar to the EXISTING Phase 54 `cancel` / `pause` / `resume` / `prioritize` / `approve` / `reject` Protocol methods — no parallel implementation (§13 forbidden practice).
- Render the kanban-style 4-column board (Pending / Running / Paused / Failed) per the mockup, with the optional list-mode toggle.
- Render the per-task detail bottom-dock tabs (Task metadata | Inputs | Logs | Events | Errors | Output) and the right-rail Summary / Parent Session / Cost Breakdown / Recent Activity / Recent Artifacts cards.
- Per-page Playwright spec at `web/console/tests/tasks-page.spec.ts` covers kanban load, card drag-to-column → control verb invocation, bulk-action toolbar, prioritize scope-claim degradation.
- D-025 concurrent-reuse contract: N≥100 concurrent `tasks.list` calls against a single shared TaskRegistry under `-race`.
- §17 integration test: `test/integration/tasks_page_test.go` — real TaskRegistry + real Protocol transport + bulk control verb propagation + identity propagation under `-race`.

## Non-goals

- **Session-level priority field.** D-065 — explicitly NOT rendered anywhere on this page. Task-level priority IS rendered per D-072 (the `prioritize` Protocol method, shipped Phase 54).
- **Drag-to-reorder priority on the board.** The mockup explicitly disclaims drag-as-priority — the priority control surfaces via the `prioritize` control method invoked from the per-task action bar, not from card drag-and-drop. Drag-across-columns invokes the matching status-control verb (`pause` / `resume` / `cancel`), not `prioritize`.
- **"Replay task" surface (re-run a completed task with mutations).** Brief 11 §PG-5 / page-tasks.md §10 — post-V1 (foreshadows Evaluations, D-064).
- **Cross-runtime task aggregator.** D-091 — post-V1.
- **Editing a task's input from the page.** The page is observation + control, not authoring. New-run authoring lives in 73n Playground.
- **New control-plane verbs.** The bulk toolbar consumes the 10 shipped Phase 54 methods. Any "new control verb" temptation is rejected on sight per §13 ("no parallel implementations").
- **Per-task event-log aggregation.** `events.aggregate` (Phase 72a) feeds Live-Runtime / Events page sparklines; the Tasks page consumes `task.*` event-bus deltas directly via `events.subscribe`, not aggregate.

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` declares `tasks.list` and (if not already shipped under Phase 73) `tasks.get` as Method constants. The 10 shipped Phase 54 control methods (`start`, `cancel`, `pause`, `resume`, `redirect`, `inject_context`, `approve`, `reject`, `prioritize`, `user_message`) are NOT redeclared.
- [ ] `internal/protocol/types/tasks.go` defines `TaskRow`, `TaskFilter`, `TaskListRequest`, `TaskListResponse`, `TaskListCursor`, `TaskDetail`, `TaskParentSessionRef`, `TaskParentTaskRef`, `TaskCostRollup`, `TaskPlannerSnapshotRef` wire types — single source of truth (D-072 layout, RFC §5.2).
- [ ] `tasks.list` accepts a `TaskFilter` covering `Statuses []TaskStatus` (Pending / Running / Paused / Complete / Failed / Cancelled), `Kinds []TaskKind` (foreground / background), `ParentTaskID *TaskID`, `Identities []identity.Identity` (admin-elevated), `Since` / `Until` time-window, `ErrorClasses []string`, `LatencyAbove *time.Duration`, and a `Search string` free-text routed to the runtime `search.tasks` index (Phase 72c). Returns paginated `TaskRow` rows + cursor + aggregate counters (Pending / Running / Paused / Failed) for the filtered view.
- [ ] `tasks.list` enforces identity-mandatory: a request with an incomplete `(tenant, user, session)` triple fails closed with `CodeIdentityRequired` (NEVER silently downgrade — §13 forbidden-practice + CLAUDE.md §6 rule 9). Cross-tenant queries (multiple distinct tenants in `Filter.Identities`) require the `admin` scope claim (D-079) and emit `audit.admin_scope_used`; a non-admin cross-tenant request fails closed with `CodeScopeMismatch`.
- [ ] `tasks.list` row shape (`TaskRow`) carries: `ID` (TaskID), `Kind`, `Status`, `Priority` (the task-level priority shipped via Phase 54 `prioritize`, D-072), `Identity` (quadruple — truncated for display on the card), `ParentSessionID`, `ParentTaskID *TaskID`, `Description`, `Query`, `StartedAt`, `UpdatedAt`, `DurationMS`, `ErrorClass *string` (set on Failed), `ToolCount int`, `BackgroundAcknowledged bool`, `GroupID *TaskGroupID`. The shape is a **projection** of `internal/tasks.Task` — never the internal Go type (CLAUDE.md §8 — Console never reads internal runtime objects).
- [ ] **NO session-level priority field** appears anywhere in `TaskRow`, `TaskListRequest`, `TaskListResponse`, `TaskDetail`, or any new wire type — D-065 carve-out. Only `TaskRow.Priority` (task-level) ships. A reviewer who finds a `SessionPriority` field MUST reject the PR on sight.
- [ ] `tasks.get` enriched payload (`TaskDetail`) carries: the full `Task` projection + `ParentSession TaskParentSessionRef` (id, agent name, status, started-at, latest-event-at) + `ParentTask *TaskParentTaskRef` (id + status + kind when child) + `Cost TaskCostRollup` (aggregated `llm.cost.recorded` per planner step within this task; total tokens + dollar amount) + `PlannerSnapshot *TaskPlannerSnapshotRef` (id + summary for the planner-checkpoint at spawn time, scoped via the existing Phase 73 `state.load_planner_checkpoint`). Heavy payload content is referenced via `ArtifactStub` (D-026) — `tasks.get` MUST NOT inline bytes that exceed the heavy-content threshold.
- [ ] `tasks.get` identity-mandatory + cross-tenant: same posture as `tasks.list`. A request for a TaskID outside the caller's tenant returns `CodeNotFound` (NEVER reveal cross-tenant existence to a non-admin) — same shape `tasks.TaskRegistry.Get` already enforces.
- [ ] Bulk-action toolbar in the SvelteKit page invokes the EXISTING shipped Phase 54 control methods (`cancel`, `pause`, `resume`, `prioritize`, `approve`, `reject`) — verified by `grep -E 'methods\.MethodCancel|methods\.MethodPause|methods\.MethodResume|methods\.MethodPrioritize|methods\.MethodApprove|methods\.MethodReject' web/console/src/lib/components/tasks/` returning matches AND no new method-name string is introduced anywhere under `web/console/`.
- [ ] **Card-drag-to-column behavior is wired to the same Phase 54 verbs**: Running → Paused invokes `pause`; Paused → Running invokes `resume`; Running → Failed invokes `cancel`; Pending → Running is a server-controlled transition (no client-issued verb — drag is a no-op with an inline "Tasks transition to Running automatically" toast).
- [ ] Bulk actions enforce the `tasks.control` scope claim (D-066 — control is a more-elevated tier than observation): a non-elevated operator sees the bulk toolbar buttons in a disabled state with a tooltip ("requires control scope"); when an elevated operator submits, each per-row call goes through the shipped Phase 54 `ControlSurface.Dispatch` which already enforces `CheckScope` (D-072 §3) — partial-completion is rendered inline (per-row pass/fail), never a silent batch abort.
- [ ] **Prioritize scope-claim degradation**: the per-task Prioritize composer is gated on the same `tasks.control` claim; an out-of-range priority value returns `CodePayloadInvalid` (the surface maps `steering.ErrPayloadInvalid` per Phase 54).
- [ ] The Tasks page SvelteKit route (`web/console/src/routes/tasks/+page.svelte`) renders against `console-tasks-page.png` with: sub-header strip (filter chips + saved-filters + search + Refresh + Export + Board/List toggle) + main canvas (kanban 4-column board OR virtualised list) + selected-task action bar (6 Phase 54 control buttons) + bottom dock (6 tabs) + right rail (Summary / Parent Session / Cost Breakdown / Recent Activity / Recent Artifacts cards).
- [ ] The page goes through the **typed Protocol client** at `web/console/src/lib/protocol.ts` (D-093 generated from `CanonicalWireTypes`); NO hand-rolled `fetch` calls in `.svelte` files (§13).
- [ ] Saved-filter chips, board-vs-list mode preference, and pin/sort preferences persist in Console DB per D-061 (NEVER mutate runtime entities). Schema lives in `web/console/src/lib/db/saved_filters_tasks.ts` on top of the Phase 72h base schema.
- [ ] Design tokens only — no raw color / spacing / type-scale literals in `.svelte` files (§13 + Stylelint enforcement).
- [ ] Per-page Playwright spec `web/console/tests/tasks-page.spec.ts` covers: (a) kanban 4-column board renders with cards in correct status columns from a seeded `tasks.list` fixture, (b) dragging a Running card into the Paused column invokes the `pause` Protocol method (mocked transport assertion), (c) bulk-action toolbar appears when ≥2 row checkboxes are selected, (d) Prioritize button is disabled with the scope-claim tooltip when the operator lacks `tasks.control`.
- [ ] **Concurrent-reuse test passes** — `internal/tasks/protocol/list_concurrent_test.go` runs N=100+ concurrent `tasks.list` calls (overlapping + disjoint filters) against a single shared `TaskRegistry` under `-race`, asserting no data races, no context bleed (each goroutine's filter is preserved on its returned rows), no goroutine leaks (D-025).
- [ ] **§17 integration test passes** — `test/integration/tasks_page_test.go` wires the real `tasks/inprocess` TaskRegistry + real Phase 54 `ControlSurface` + real `events/drivers/inmem` bus + real Protocol transport. Seeds N foreground + M background tasks across two tenants; asserts: (i) tenant A's `tasks.list` returns only tenant-A rows, (ii) tenant A's bulk `pause` on tenant-A rows succeeds, (iii) tenant A's bulk `pause` targeting tenant-B task IDs fails per-row with `CodeNotFound`, (iv) a `task.paused` event flows through the bus for each succeeded `pause`, (v) ≥1 failure mode (forced `Inbox.Enqueue` payload-invalid → `CodePayloadInvalid`), (vi) N≥10 concurrent subscriber/control stress against the seam.
- [ ] `scripts/smoke/phase-73d.sh` asserts `tasks.list` + `tasks.get` round-trip + cross-tenant rejection + control-scope gating on bulk actions.
- [ ] `docs/glossary.md` adds: `tasks.list`, `tasks.get`, `TaskRow`, `TaskCostRollup`, `TaskPlannerSnapshotRef`, "kanban board view (Tasks page)".

## Files added or changed

```text
internal/protocol/methods/methods.go                       # +tasks.list (and tasks.get if not yet shipped under Phase 73)
internal/protocol/types/tasks.go                           # +TaskRow, TaskFilter, TaskListRequest, TaskListResponse, TaskListCursor, TaskDetail, TaskParentSessionRef, TaskParentTaskRef, TaskCostRollup, TaskPlannerSnapshotRef
internal/protocol/errors/errors.go                         # confirm CodeScopeMismatch / CodeIdentityRequired / CodeNotFound / CodePayloadInvalid coverage (no new codes expected; reuse Phase 54's)
internal/protocol/transports/stream/tasks_handler.go       # method dispatch + identity/scope checks delegating to internal/tasks/protocol
internal/protocol/transports/stream/tasks_handler_test.go
internal/tasks/protocol/list.go                            # tasks.list implementation: filter normalisation + cursor pagination + aggregates + cross-tenant gating
internal/tasks/protocol/get.go                             # tasks.get enrichment: ParentSession / ParentTask / Cost / PlannerSnapshot composition
internal/tasks/protocol/list_test.go
internal/tasks/protocol/get_test.go
internal/tasks/protocol/list_concurrent_test.go            # D-025 — N>=100 concurrent calls against shared TaskRegistry
test/integration/tasks_page_test.go                        # cross-package: tasks/inprocess + Phase 54 ControlSurface + bus + transport + identity scope
web/console/src/routes/tasks/+page.svelte
web/console/src/lib/components/tasks/KanbanBoard.svelte
web/console/src/lib/components/tasks/KanbanColumn.svelte
web/console/src/lib/components/tasks/TaskCard.svelte
web/console/src/lib/components/tasks/TasksTable.svelte
web/console/src/lib/components/tasks/SubHeaderStrip.svelte
web/console/src/lib/components/tasks/SelectedTaskActionBar.svelte
web/console/src/lib/components/tasks/BulkActionToolbar.svelte
web/console/src/lib/components/tasks/TaskDetailBottomDock.svelte
web/console/src/lib/components/tasks/RightRailSummary.svelte
web/console/src/lib/components/tasks/RightRailParentSession.svelte
web/console/src/lib/components/tasks/RightRailCostBreakdown.svelte
web/console/src/lib/db/saved_filters_tasks.ts      # Console DB schema for tasks-page saved filters (on top of Phase 72h base)
web/console/tests/tasks-page.spec.ts
web/console/src/lib/protocol.ts                            # REGENERATED ONLY by `make protocol-ts-gen` — never hand-edited
scripts/smoke/phase-73d.sh
docs/glossary.md                                            # +tasks.list, +tasks.get, +TaskRow, +TaskCostRollup, +TaskPlannerSnapshotRef, +kanban board view (Tasks page)
docs/plans/README.md                                        # +Phase 73d Pending row (under existing Phase 73 detail block)
README.md                                                   # +Phase 73d status row
```

No new top-level directory — `internal/protocol/`, `internal/tasks/`, `web/console/`, `test/integration/` are all already in CLAUDE.md §3.

## Public API surface

```go
// internal/protocol/methods/methods.go (additions only — Phase 54 methods unchanged)
const (
    MethodTasksList Method = "tasks.list"
    MethodTasksGet  Method = "tasks.get"  // declared here if Phase 73 has not yet shipped it
)

// internal/protocol/types/tasks.go
package types

import (
    "time"

    "github.com/hurtener/Harbor/internal/identity"
    "github.com/hurtener/Harbor/internal/tasks"
)

// TaskRow is the projection returned by tasks.list. Wire-only — never the
// internal tasks.Task struct (CLAUDE.md §8 — Console never reads internal
// runtime objects). Heavy fields (Result, Error) stay on TaskDetail; the
// row is compact for kanban + table density.
type TaskRow struct {
    ID                     tasks.TaskID
    Kind                   tasks.TaskKind
    Status                 tasks.TaskStatus
    Priority               int                  // task-level priority (D-072); NEVER session-level (D-065)
    Identity               identity.Quadruple   // truncated for display by the client
    ParentSessionID        identity.SessionID
    ParentTaskID           *tasks.TaskID
    Description            string
    Query                  string
    StartedAt              time.Time
    UpdatedAt              time.Time
    DurationMS             int64
    ErrorClass             *string              // set on Status == Failed
    ToolCount              int
    BackgroundAcknowledged bool                 // task.background_acknowledged latched
    GroupID                *tasks.TaskGroupID
}

// TaskFilter is the read-side filter for tasks.list.
type TaskFilter struct {
    Statuses      []tasks.TaskStatus    // empty = all
    Kinds         []tasks.TaskKind      // empty = both foreground + background
    ParentTaskID  *tasks.TaskID         // drill into a SpawnTask group
    Identities    []identity.Identity   // cross-tenant requires admin claim (D-079)
    Since         time.Time             // optional lower bound on StartedAt
    Until         time.Time             // optional upper bound on StartedAt
    ErrorClasses  []string              // facet on TaskRow.ErrorClass for Failed tasks
    LatencyAbove  *time.Duration        // facet on DurationMS for Running/Complete tasks
    Search        string                // free-text routed to runtime search.tasks index (Phase 72c)
}

// TaskListAggregates is the per-column counter the kanban renders at column header.
type TaskListAggregates struct {
    Pending int64
    Running int64
    Paused  int64
    Failed  int64
    // Complete / Cancelled are aggregated for the list-mode counter strip
    // but the kanban renders only the four primary columns per the mockup.
    Complete  int64
    Cancelled int64
}

type TaskListCursor struct {
    NextPageToken string // opaque; empty when no more pages
}

type TaskListRequest struct {
    Filter   TaskFilter
    PageSize int            // capped server-side; 0 → default
    Cursor   TaskListCursor // empty → first page
}

type TaskListResponse struct {
    Rows       []TaskRow
    Cursor     TaskListCursor
    Aggregates TaskListAggregates
}

// TaskDetail is tasks.get's enriched payload. Heavy fields stay refs.
type TaskDetail struct {
    Task            tasks.Task                  // full projection (post-redaction; heavy values via ArtifactRef per D-026)
    ParentSession   TaskParentSessionRef
    ParentTask      *TaskParentTaskRef          // set when Task.ParentTaskID != nil
    Cost            TaskCostRollup
    PlannerSnapshot *TaskPlannerSnapshotRef     // set when a planner checkpoint exists at spawn time
}

type TaskParentSessionRef struct {
    SessionID     identity.SessionID
    AgentName     string
    Status        string
    StartedAt     time.Time
    LatestEventAt time.Time
}

type TaskParentTaskRef struct {
    TaskID tasks.TaskID
    Kind   tasks.TaskKind
    Status tasks.TaskStatus
}

type TaskCostRollup struct {
    TotalTokens   int64
    PromptTokens  int64
    OutputTokens  int64
    USD           float64
    PerStep       []TaskCostStep // aggregated from llm.cost.recorded events scoped to the task
}

type TaskCostStep struct {
    StepIndex int
    Tokens    int64
    USD       float64
}

type TaskPlannerSnapshotRef struct {
    CheckpointID string  // resolved via the existing Phase 73 state.load_planner_checkpoint
    Summary      string  // pre-truncated; heavy content is on the checkpoint, fetched on demand
}
```

The package `internal/tasks/protocol` exports nothing public beyond a `RegisterProtocol(...)` wiring function the binary entry point calls — every method body is a pure mapping of `TaskFilter` → `tasks.TaskRegistry.List` / `tasks.TaskRegistry.Get` + the cross-tenant / scope-claim gate.

## Test plan

- **Unit:**
  - `internal/tasks/protocol/list_test.go` — table-driven matrix of `TaskFilter` combinations (status singletons, multi-status, kind facet, parent-task drill-down, error-class facet, latency-above facet, time-window, free-text search routing) vs a seeded TaskRegistry; assert paginated rows + cursor honored across pages; assert aggregates match the seed fixture.
  - `internal/tasks/protocol/list_test.go::TestList_IdentityMandatory` — missing each component of `(tenant, user, session)` → `CodeIdentityRequired`; no method body reached.
  - `internal/tasks/protocol/list_test.go::TestList_CrossTenant_RequiresAdmin` — request with multiple tenant IDs but no `admin` scope → `CodeScopeMismatch`; with admin → succeeds + `audit.admin_scope_used` emitted.
  - `internal/tasks/protocol/get_test.go` — fixture with parent session + parent task + 3 cost events spanning 2 planner steps → `TaskDetail` carries all four enriched fields; heavy result (>=heavy-content threshold) is rendered as `ArtifactRef`, never inlined; cross-tenant TaskID → `CodeNotFound` (never reveal existence).
- **Integration:**
  - `test/integration/tasks_page_test.go` (§17) — real `tasks/inprocess` TaskRegistry + real Phase 54 `ControlSurface` + real `events/drivers/inmem` bus + real Protocol transport (Phase 60 / 62). Seeds N=20 foreground + M=10 background tasks across tenants `t1` + `t2`. Asserts: (a) `tasks.list` for tenant `t1` returns only tenant-`t1` rows; (b) bulk `pause` on three `t1` rows → 3× `task.paused` events on the bus, run-loop transitions confirmed via TaskRegistry `Get`; (c) bulk `pause` targeting a `t2` task id with non-admin scope → per-row `CodeNotFound`, no event emitted, no transition on `t2`; (d) Prioritize with out-of-range priority → `CodePayloadInvalid` (`steering.ErrPayloadInvalid` mapping per Phase 54); (e) identity propagation: every emitted event carries the dispatcher's quadruple; (f) N=10 concurrent submitters each running a `tasks.list` + bulk `pause` round with disjoint task ids → no goroutine leak, no data race under `-race`.
- **Conformance:**
  - `tasks.list` and `tasks.get` run against the Protocol conformance suite (Phase 62 shipped) — every transport (HTTP+SSE / WebSocket / stdio) emits identical wire shapes for the same request.
- **Concurrency / leak:**
  - `internal/tasks/protocol/list_concurrent_test.go::TestList_ConcurrentReuse_D025` — N=128 concurrent goroutines, each with a goroutine-unique `TaskFilter`, against a single shared registry under `-race`. Per the D-025 contract: no data races (goroutine-local filter never leaks across calls; verified by per-row identity assertions), no context bleed (per-goroutine ctx-derived identity matches all returned rows), no cancellation cross-talk (cancelling goroutine A's ctx must not abort goroutine B), no goroutine leaks (baseline `runtime.NumGoroutine` restored after join).
- **UI (Playwright):**
  - `web/console/tests/tasks-page.spec.ts` — kanban 4-column board renders against a fixture; drag a Running card into the Paused column → assert the typed Protocol client invoked `pause` with the row's TaskID + the operator's identity quadruple; multi-select via row checkboxes → assert bulk-action toolbar shows; click Prioritize without the `tasks.control` scope claim → button is disabled and shows the scope-claim tooltip.

## Smoke script additions

`scripts/smoke/phase-73d.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'tasks/list' '{}'` → assert 200; `assert_json_path '.rows | type' 'array'`.
- `protocol_call 'tasks/list' '{"filter": {"statuses": ["running"]}}'` → assert filter honored.
- `protocol_call 'tasks/list' '{"filter": {"kinds": ["background"]}}'` → assert kind facet honored.
- `protocol_call 'tasks/list' '{"filter": {"parent_task_id": "<seed-spawn-task-id>"}}'` → assert SpawnTask drill-down.
- `protocol_call 'tasks/list' '{"filter": {"identities": [{"tenant_id":"t1"}, {"tenant_id":"t2"}]}}'` (without `admin` scope claim) → `assert_status 403` (CodeScopeMismatch).
- `protocol_call 'tasks/list' '{}'` with no identity context → `assert_status 401` (CodeIdentityRequired).
- `protocol_call 'tasks/get' '{"id": "<seed-task-id>"}'` → assert 200 + `assert_json_path '.parent_session.session_id' '<seed-session-id>'`.
- `protocol_call 'tasks/get' '{"id": "<cross-tenant-task-id>"}'` → `assert_status 404` (CodeNotFound — cross-tenant existence not revealed).
- `protocol_call 'cancel' '{"id": "<seed-task-id>"}'` (without `tasks.control` claim) → `assert_status 403` (CodeScopeMismatch — Phase 54 control-scope gate).
- `protocol_call 'pause' '{"id": "<seed-task-id>"}'` (without `tasks.control` claim) → `assert_status 403`.
- `protocol_call 'prioritize' '{"id": "<seed-task-id>", "payload": {"priority": 9999}}'` → `assert_status 400` (CodePayloadInvalid).
- `skip 'phase 73d: /console/tasks route lands with 73m harbor console subcommand'`.
- Surface-existence probe — `skip_if_404 "$(api_url /protocol/tasks/list)" 'phase 73d: tasks.list route absent until Protocol layer ships' || true`.

## Coverage target

- `internal/tasks/protocol`: 85%.
- `internal/protocol/transports/stream` (tasks handler additions): 80%.
- `web/console/src/routes/tasks/`: 70% (Svelte component coverage via `svelte-check` + Playwright).

## Dependencies

**Same-wave (Wave 13):**

- Phase 72 (events.subscribe scope foundation — Stage 1)
- Phase 72a (events filter + aggregate — Stage 1; the task-event subscription used for kanban live deltas relies on the filter extension)
- Phase 72c (search.* cluster — Stage 1; supplies `search.tasks` for the page's free-text search box)
- Phase 72h (Console DB schema — Stage 1; saved-filter chips and board/list-mode preference)
- Phase 73 (Console state inspection surface — Pending; supplies `tasks.get` parent + `state.load_planner_checkpoint` reference. If Phase 73 ships `tasks.get` first, 73d EXTENDS the response shape rather than redeclaring the method)
- Phase 73f (Console Tools page — Stage 2.1; the per-task detail surfaces a Tools-filter facet for the Task → Tool drill-down)
- Phase 75 (Playwright harness baseline — Stage 1)

**Already shipped (pre-Wave 13):**

- Phase 20 (TaskRegistry — `Shipped`, supplies `tasks.TaskRegistry.List` / `Get` / `Spawn` + the `Task` / `TaskRow` projection source-of-truth + `task.*` events)
- Phase 21 (TaskGroup + retain-turn + patches + ack-background — `Shipped`, supplies group / parent-task references + `task.group_*` + `task.background_acknowledged`)
- Phase 50 (pauseresume Coordinator — `Shipped`, supplies the pause primitive the Phase 54 verbs flow through)
- Phase 52 (steering registry + scope — `Shipped`, supplies `CheckScope` for the bulk-control gating)
- Phase 53 (steering RunLoop — `Shipped`, drains the steering inbox driven by the bulk verbs)
- Phase 54 (Protocol task control surface — `Shipped`, supplies `cancel` / `pause` / `resume` / `prioritize` / `approve` / `reject` — the SIX shipped methods this page consumes for bulk + per-task control; D-072)
- Phase 60 (Protocol wire transport — `Shipped`)
- Phase 61 (Protocol auth — `Shipped`, supplies the `admin` and `tasks.control` scope claims per D-066 / D-079)
- Phase 62 (Protocol conformance suite — `Shipped`, the new methods pass through)

## Risks / open questions

- **`tasks.list` aggregate scan cost.** `TaskListResponse.Aggregates` requires counting filtered tasks per status; for sessions with thousands of tasks this is wasteful. V1 accepts O(N) per call (matches brief 11 §CC-4 high-cardinality posture and existing `TaskRegistry.List` shape). Post-V1 may add a `tasks.aggregate` echo of `events.aggregate` (Phase 72a) for the kanban column counters; this phase deliberately keeps the surface minimal and reuses `tasks.list` aggregates so the §13 "no parallel implementation" rule binds.
- **`tasks.get` is also claimed by the parent Phase 73 plan.** If Phase 73 ships `tasks.get` first under its `state inspection surface` umbrella, 73d EXTENDS the response shape rather than redeclaring the method. The plan above carries the additive enrichments (ParentSession / ParentTask / Cost / PlannerSnapshot) as a backward-compatible struct expansion. The drift risk: Phase 73 lands a thinner `tasks.get` that omits enrichment fields — 73d's PR carries the expansion in the same commit. The wave-13 decomposition §5 row "73d" explicitly lists `tasks.get` enrichments under this phase.
- **Kanban Pending → Running drag is a no-op.** Pending is the state where the registry has spawned the task but the engine has not yet driven `MarkRunning`; this transition is server-initiated. The UI rejects the drag with a toast — but a misbehaving client could repeatedly send a verb that doesn't exist. Defense: the typed Protocol client surfaces no `start_existing` method; the only `start` method (Phase 54) creates a NEW task, not a transition. The UI's drag handler enforces the lookup against allowed-source-to-destination edges before issuing any verb.
- **Bulk control partial completion.** A bulk `pause` over 50 tasks may have 47 succeed + 3 fail (cross-tenant / cross-scope / payload-invalid). The UI MUST render per-row pass/fail outcomes — never a silent batch failure (§13 silent-degradation forbidden). The integration test asserts the partial-completion shape.
- **Phase 73 dependency slip.** 73d depends on Phase 73's `tasks.get` / `state.load_planner_checkpoint`. If Phase 73 slips past Stage 2.2, 73d's Cost / PlannerSnapshot fields ship as nullable — the kanban + bulk control still work without them. Mitigation: Phase 73 is `Pending` in the same wave; the decomposition doc §5 explicitly lists Phase 73 as a 73d dep.
- **Card-drag transactional semantics.** Drag-across-columns invokes a control verb; if the verb fails, the card MUST revert visually. The Svelte component holds a per-card optimistic-state struct that reverts on error rather than acknowledging the drag on click. The Playwright spec asserts the revert.
- **Per-page Playwright spec coverage.** The wave-end 75a aggregator suite enumerates every page-spec and asserts a matching `*.spec.ts` exists; 73d's spec MUST be merged before 75a's enumeration runs — i.e. before the final Stage-2.3 PR.

## Glossary additions

- **`tasks.list`** — Protocol method returning the paginated list of tasks visible to the caller's identity scope, with optional facet filters (status / kind / parent-task / identity / time-window / error-class / latency-above / free-text) and per-status aggregate counters (Pending / Running / Paused / Failed / Complete / Cancelled). Wave 13 Phase 73d. Cross-tenant queries require the `admin` scope claim (D-079).
- **`tasks.get`** — Protocol method returning the enriched detail of a single task: the full `Task` projection (heavy values via `ArtifactRef` per D-026), parent-session reference, parent-task reference (when child), per-step cost rollup aggregated from `llm.cost.recorded` events, and the planner-checkpoint reference at spawn time. Cross-tenant TaskID lookups return `CodeNotFound` (existence is never revealed across tenants). Wave 13 Phase 73d (over the Phase 73 base method).
- **`TaskRow`** — wire-only projection of `internal/tasks.Task` returned by `tasks.list`. Compact for kanban + table density; carries `Priority` (task-level, D-072) but explicitly NEVER a session-level priority field (D-065 carve-out). The Console reads `TaskRow`, never the internal Go type (CLAUDE.md §8).
- **`TaskCostRollup`** — `tasks.get` enrichment field aggregating `llm.cost.recorded` events scoped to the task: total tokens (prompt + output), USD cost, and a per-planner-step breakdown. Heavy per-event payloads stay on the event bus; the rollup carries only sums + the step index — never inlined event payloads.
- **`TaskPlannerSnapshotRef`** — `tasks.get` enrichment field pointing at the planner checkpoint that existed at task spawn time. Carries the checkpoint id (resolvable via the existing Phase 73 `state.load_planner_checkpoint`) and a pre-truncated summary. Heavy checkpoint content is fetched on demand, never inlined in the `tasks.get` response.
- **Kanban board view (Tasks page)** — the Tasks page default rendering mode: a 4-column board (Pending / Running / Paused / Failed) with virtualised columns. Cards drag across columns to invoke the matching shipped Phase 54 control verb (`pause` / `resume` / `cancel`) — NOT priority drag-reordering (priority surfaces only via the explicit `prioritize` method per D-072). List mode is the alternative virtualised-table rendering of the same `tasks.list` data.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` passes (`web/console/src/lib/protocol.ts` regenerated from `CanonicalWireTypes` per D-093)
- [ ] `svelte-check --fail-on-warnings` passes (no Svelte 4 reactivity syntax per D-092)
- [ ] `npm run lint` passes in `web/console/` (no raw color / spacing literals per §13)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (`tasks.list` + `tasks.get` + bulk control all touch identity; the integration test asserts cross-tenant rejection per-method)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `tasks.list` calls against a single shared TaskRegistry under `-race` (D-025)
- [ ] **Integration test exists** — `test/integration/tasks_page_test.go` wires real TaskRegistry + Phase 54 ControlSurface + bus + transport + identity propagation under `-race` (§17)
- [ ] **Per-page Playwright spec lands in this phase's PR** — `web/console/tests/tasks-page.spec.ts` exists and passes (binding for every 73x phase per the decomposition doc §12 lock-in)
- [ ] **`grep -nE 'session.priority|SessionPriority|session_priority' web/console/ internal/protocol/types/tasks.go internal/tasks/protocol/` returns NOTHING** — D-065 carve-out verification (a reviewer who finds such a field MUST reject the PR on sight)
- [ ] **`grep -nE 'methods\.(MethodCancel|MethodPause|MethodResume|MethodPrioritize|MethodApprove|MethodReject)' web/console/src/lib/components/tasks/` returns matches** — bulk toolbar consumes the EXISTING Phase 54 verbs (§13 no-parallel-implementations verification)
- [ ] Glossary updated with the 6 new entries
- [ ] If a brief finding was departed from: justified + decisions.md entry filed (None for this phase)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review (decomposition doc §12 lock-in item 3 + the binding coordinator-verify protocol)
