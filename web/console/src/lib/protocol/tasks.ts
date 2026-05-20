/**
 * Tasks-page Protocol wire types (Phase 73d / D-123).
 *
 * # Wire types only — the client is the unified `HarborClient`
 *
 * This module is the typed wire-shape surface ONLY: the request /
 * response shapes the Tasks page narrows `client.tasks.*` and
 * `client.control.*` results into. There is no `fetch` here and no
 * page-local client class (CONVENTIONS.md §6).
 *
 * The wire shapes mirror `internal/protocol/types/tasks.go` field-for-
 * field (the Go-side single source per D-002). When the
 * `cmd/harbor-gen-protocol-ts` generator (D-093) ships, these types fold
 * into the generated `protocol.ts` and this module re-exports from there
 * — a mechanical migration.
 *
 * The Console Tasks page consumes the EXISTING Phase 54 control verbs
 * (`cancel` / `pause` / `resume` / `prioritize` / `approve` / `reject`)
 * for mutation via `client.control.*` — there is NO `tasks.*` mutating
 * method (CLAUDE.md §13 "no parallel implementations").
 */

/** Wire enum — a task's lifecycle status. */
export type TaskStatus =
  | 'pending'
  | 'running'
  | 'paused'
  | 'complete'
  | 'failed'
  | 'cancelled';

/** Wire enum — a task's kind (foreground run vs background task). */
export type TaskKind = 'foreground' | 'background';

/** The identity triple a task runs within. */
export interface TaskIdentity {
  tenant: string;
  user: string;
  session: string;
}

/** The compact projection returned by `tasks.list`. */
export interface TaskRow {
  id: string;
  kind: TaskKind;
  status: TaskStatus;
  /** TASK-level priority (D-072). NEVER session-level (D-065 carve-out). */
  priority: number;
  identity: TaskIdentity;
  parent_session_id: string;
  parent_task_id?: string;
  description: string;
  query: string;
  started_at: string;
  updated_at: string;
  duration_ms: number;
  error_class?: string;
  tool_count: number;
  background_acknowledged: boolean;
  group_id?: string;
}

/** The server-enforced facet filter on `tasks.list`. */
export interface TaskFilter {
  statuses?: TaskStatus[];
  kinds?: TaskKind[];
  parent_task_id?: string;
  identities?: TaskIdentity[];
  since?: string;
  until?: string;
  error_classes?: string[];
  latency_above_ms?: number;
  search?: string;
}

/** The per-status counters the kanban renders at each column header. */
export interface TaskListAggregates {
  pending: number;
  running: number;
  paused: number;
  failed: number;
  complete: number;
  cancelled: number;
}

/** The opaque pagination cursor for `tasks.list`. */
export interface TaskListCursor {
  next_page_token?: string;
}

/**
 * The Phase 73b (D-126) status-counter-strip aggregate — the Console
 * Live Runtime page's header five-chip strip (`pending / running /
 * completed / paused / failed`). Mirrors the Go
 * `types.TasksListStatusCounterStrip`.
 *
 * Distinct from {@link TaskListAggregates}: the strip is computed over
 * the FULL identity-scoped task set (not the filtered view) and is the
 * session-wide present-tense posture. It keys on `completed` (not
 * `complete`) and omits `cancelled` — a five-chip strip, not the
 * six-status kanban tally. Identity-scoped + computed server-side: the
 * counter never crosses the isolation boundary.
 */
export interface TaskListStatusCounterStrip {
  pending: number;
  running: number;
  completed: number;
  paused: number;
  failed: number;
}

/** The `tasks.list` request body. */
export interface TaskListRequest {
  filter?: TaskFilter;
  page_size?: number;
  cursor?: TaskListCursor;
  /**
   * Phase 73b (D-126) — opt the response into carrying the
   * {@link TaskListStatusCounterStrip} aggregate. The Live Runtime page
   * sets it on its initial-load call; the Tasks page never does.
   */
  include_status_counter_strip?: boolean;
}

/** The `tasks.list` reply. */
export interface TaskListResponse {
  rows: TaskRow[];
  cursor: TaskListCursor;
  aggregates: TaskListAggregates;
  /**
   * Phase 73b (D-126) header-strip aggregate — present ONLY when the
   * request set `include_status_counter_strip`. Computed over the full
   * identity-scoped task set (not the filtered view).
   */
  status_counter_strip?: TaskListStatusCounterStrip;
}

/** The parent-session reference card `tasks.get` returns. */
export interface TaskParentSessionRef {
  session_id: string;
  agent_name: string;
  status: string;
  started_at: string;
  latest_event_at: string;
}

/** The parent-task reference `tasks.get` returns for a child task. */
export interface TaskParentTaskRef {
  task_id: string;
  kind: TaskKind;
  status: TaskStatus;
}

/** One planner-step cost entry in a TaskCostRollup. */
export interface TaskCostStep {
  step_index: number;
  tokens: number;
  usd: number;
}

/** The per-task cost aggregation `tasks.get` returns. */
export interface TaskCostRollup {
  total_tokens: number;
  prompt_tokens: number;
  output_tokens: number;
  usd: number;
  per_step: TaskCostStep[];
}

/** The planner-checkpoint reference `tasks.get` returns. */
export interface TaskPlannerSnapshotRef {
  checkpoint_id: string;
  summary: string;
}

/** A by-reference artifact stub for heavy result content (D-026). */
export interface TaskArtifactRef {
  id: string;
  mime_type?: string;
  size_bytes: number;
}

/** The enriched payload `tasks.get` returns. */
export interface TaskDetail {
  task: TaskRow;
  parent_session: TaskParentSessionRef;
  parent_task?: TaskParentTaskRef;
  cost: TaskCostRollup;
  planner_snapshot?: TaskPlannerSnapshotRef;
  result_ref?: TaskArtifactRef;
  result_inline?: string;
}

/** The four primary kanban columns, in mockup order. */
export const KANBAN_COLUMNS: { status: TaskStatus; label: string }[] = [
  { status: 'pending', label: 'Pending' },
  { status: 'running', label: 'Running' },
  { status: 'paused', label: 'Paused' },
  { status: 'failed', label: 'Failed' }
];

/** The shipped Phase 54 control verbs the bulk toolbar consumes. */
export type ControlVerb = 'cancel' | 'pause' | 'resume' | 'approve' | 'reject';
