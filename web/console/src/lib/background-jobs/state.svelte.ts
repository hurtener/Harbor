// Harbor Console — Background Jobs page reactive state controller
// (Phase 108j / D-182). Svelte 5 runes mode (D-092).
//
// This module owns the Background Jobs page's reactive state; the
// `.svelte` view reads it and calls its actions, never touching the
// Protocol client directly (CONVENTIONS.md §6). It is the king-file
// refactor (the page was an ~801-line monolith) — the controller +
// the pure `derive.ts` / `orphan-detector.ts` projections + the focused
// rail components replace it.
//
// The page is a filtered projection of `tasks.list` with the binding
// `kinds: ['background']` (the plural []TaskKind slice — never a
// `type=background` scalar). It composes ONLY already-shipped surface:
//   - `tasks.list` (rows + per-status aggregates) — the queue feed.
//   - `tasks.get` — the per-job rail detail.
//   - `artifacts.list` (scope.task = job id) — the Artifacts-so-far card.
//   - a RUN-scoped `events.subscribe` projection owned by `TaskRunStream`
//     (reused from the Tasks page, NOT forked) — the rail Events /
//     Control History tabs + the Progress state-transition timeline.
//   - the shipped Phase 54 control verbs via `client.control.*` — the
//     bulk Cancel / Pause / Resume / Prioritize (control-scope gated,
//     D-066). No new control method (§13 / D-128).
//   - the Console-DB-backed saved filters (D-061).
//
// The `AwaitTask` orphan detector is a pure Console-side cross-check
// (D-128) — it adds no Protocol field and issues no Protocol call.
//
// Fields that an async load assigns AFTER first render (the list
// response, the detail, the run stream) are `$state` so the reactive
// re-read fires — the Events-page D-180 lesson: a plain field leaves the
// UI bound to the initial null and never re-renders when data streams in.

import {
  resolveConnection,
  hasScope,
  type RuntimeConnection
} from '$lib/connection.js';
import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import { TaskRunStream } from '$lib/tasks/run-stream.svelte.js';
import { detectOrphans } from './orphan-detector.js';
import { openListPageDB } from '$lib/db/console_db.js';
import { BackgroundJobsSavedFilters } from '$lib/db/saved_filters_background_jobs.js';
import { operatorIdOf } from '$lib/db/schema.js';
import { STUCK_CHIP_ID, type DerivedChip } from '$lib/components/background-jobs/SavedFilterChips.svelte';
import type { SavedView } from '$lib/components/ui/SavedViewChips.svelte';
import type { ArtifactRow, ArtifactsListResponse } from '$lib/protocol/artifacts.js';
import type {
  TaskRow,
  TaskFilter,
  TaskStatus,
  TaskListResponse,
  TaskListCursor,
  TaskDetail
} from '$lib/protocol/tasks.js';

/** The status-facet chips, in mockup order. */
export const STATUS_FACETS: TaskStatus[] = [
  'pending',
  'running',
  'paused',
  'complete',
  'failed',
  'cancelled'
];

/** A page-friendly `{code, message}` projection of a thrown error. */
export type PageError = { code: string; message: string };

/** One hour in ms — the `Stuck > 1h` derived-chip threshold. */
const ONE_HOUR_MS = 3_600_000;

export class BackgroundJobsPageState {
  /* ---- connection + client (CONVENTIONS.md §6) ------------------- */
  connection = $state<RuntimeConnection | null>(null);
  /** True when the connection carries the elevated control claim (D-066/D-079). */
  canControl = $state(false);
  /** Phase 83r disconnected predicate. */
  disconnected = $derived(this.connection === null);

  /* ---- page-level async state (the four-state contract) ---------- */
  status = $state<PageStatus>('loading');
  pageError = $state<PageError | null>(null);

  /* ---- list state ------------------------------------------------ */
  // The queue is ALWAYS `kinds: ['background']` — that is the page's
  // identity. A facet chip layers facets on top; the background-kind
  // filter is never removed.
  facetFilter = $state<TaskFilter>({});
  searchText = $state('');
  pageSize = $state(50);
  listResp = $state<TaskListResponse | null>(null);
  // Cursor stack — index 0 is the first page; next pushes, prev pops.
  cursorStack = $state<TaskListCursor[]>([{}]);
  pageIndex = $state(1);
  /** The active built-in derived chip, or null. */
  activeDerived = $state<DerivedChip | null>(null);
  /** The has-pending-approval facet: undefined = off, true = only. */
  approvalFacet = $state<boolean | undefined>(undefined);

  /* ---- selection + bulk actions ---------------------------------- */
  selected = $state<Set<string>>(new Set<string>());
  bulkPending = $state(false);
  bulkResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- detail (the right-rail) ----------------------------------- */
  selectedId = $state<string | null>(null);
  detailLoading = $state(false);
  detail = $state<TaskDetail | null>(null);
  /** Sibling tasks under the same TaskGroup (`tasks.list?group_id`). */
  siblings = $state<TaskRow[]>([]);
  siblingsLoading = $state(false);
  /** Artifacts produced by the selected job (`artifacts.list` scope.task). */
  artifacts = $state<ArtifactRow[]>([]);
  artifactsLoading = $state(false);
  /**
   * The run-scoped event stream for the open job — owns ONE
   * `events.subscribe` subscription the rail Events / Control / Progress-
   * timeline all read. `$state` so the async re-scope triggers re-render.
   */
  stream = $state<TaskRunStream | null>(null);

  /* ---- saved views (Console-DB-backed, D-061) -------------------- */
  savedFilters = $state<BackgroundJobsSavedFilters | null>(null);
  savedViews = $state<SavedView[]>([]);
  savedFilterSpecs = $state<Map<string, TaskFilter>>(new Map());
  activeSavedId = $state<string | null>(null);
  saveName = $state('');

  #client: ProtocolClient | null = null;

  /* ================================================================ */
  /* Derived projections                                               */
  /* ================================================================ */

  /** The effective wire filter — always background-kind, plus the facets. */
  get effectiveFilter(): TaskFilter {
    const search = this.searchText.trim();
    return {
      ...this.facetFilter,
      kinds: ['background'],
      ...(search !== '' ? { search } : {}),
      ...(this.approvalFacet !== undefined ? { has_pending_approval: this.approvalFacet } : {})
    };
  }

  /** The raw rows from the last `tasks.list` page. */
  get rawRows(): TaskRow[] {
    return this.listResp?.rows ?? [];
  }

  /**
   * The displayed rows. The `Stuck > 1h` derived chip is a Console-local
   * row-level post-filter (D-061) — no activity for over an hour. Every
   * other chip maps onto a server facet, so only this one post-filters.
   */
  get rows(): TaskRow[] {
    if (this.activeDerived?.id !== STUCK_CHIP_ID) {
      return this.rawRows;
    }
    const now = Date.now();
    return this.rawRows.filter((r) => {
      const last = Date.parse(r.last_activity_at);
      return !Number.isNaN(last) && now - last > ONE_HOUR_MS;
    });
  }

  /** The orphan set — pure Console-side cross-check over the snapshot (D-128). */
  get orphans(): Set<string> {
    return detectOrphans(this.rawRows);
  }

  /** The filtered-view total — the sum of the per-status aggregates. */
  get totalRows(): number {
    const a = this.listResp?.aggregates;
    if (a === undefined) return 0;
    return a.pending + a.running + a.paused + a.failed + a.complete + a.cancelled;
  }

  /**
   * The status the `<PageState>` boundary renders. `loading` / `error` /
   * `disconnected` pass through; the ready/empty split is derived LIVE
   * from the displayed-row count so a queue whose rows all dropped under
   * the Stuck-chip post-filter still reads "empty" (Events-page D-180
   * pattern — derive display state, never a status set once at load).
   */
  get displayStatus(): PageStatus {
    if (this.status === 'loading' || this.status === 'error' || this.status === 'disconnected') {
      return this.status;
    }
    return this.rows.length > 0 ? 'ready' : 'empty';
  }

  /** The currently-open job row (from the loaded page), or null. */
  get selectedRow(): TaskRow | null {
    return this.rawRows.find((r) => r.id === this.selectedId) ?? null;
  }

  /* ================================================================ */
  /* Boot + loading                                                    */
  /* ================================================================ */

  /**
   * Resolves the connection + Console-DB saved filters, then loads page 1.
   * `injected` is an optional in-page client the harness supplies (read
   * inside the page's onMount closure — never captured at construction).
   */
  load(injected?: ProtocolClient): void {
    const connection = resolveConnection();
    this.connection = connection;
    if (connection === null) {
      this.#client = null;
      this.status = 'disconnected';
      return;
    }
    this.#client = injected ?? new HarborClient({ connection });
    this.canControl = hasScope(connection, 'admin');

    void (async () => {
      try {
        const db = await openListPageDB(connection);
        const operator = await operatorIdOf(connection.identity.tenant, connection.identity.user);
        this.savedFilters = new BackgroundJobsSavedFilters(db, operator);
        await this.refreshSavedViews();
      } catch {
        this.savedFilters = null;
      }
    })();

    void this.loadJobs(0);
  }

  /** The page calls this on unmount — tears down the run subscription. */
  close(): void {
    this.stream?.close();
    this.stream = null;
  }

  #toError(err: unknown): PageError {
    if (err instanceof ProtocolError) {
      return { code: err.code, message: err.message };
    }
    return { code: 'runtime_error', message: err instanceof Error ? err.message : 'unknown error' };
  }

  async loadJobs(cursorIdx = 0): Promise<void> {
    if (this.#client === null) {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.pageError = null;
    try {
      const resp = await this.#client.tasks.list<TaskListResponse>({
        filter: this.effectiveFilter,
        page_size: this.pageSize,
        cursor: this.cursorStack[cursorIdx] ?? {}
      });
      this.listResp = resp;
      // ready/empty is derived live via `displayStatus`; set a non-terminal
      // status here so it flips out of `loading`.
      this.status = 'ready';
    } catch (err) {
      // The Error state suppresses any stale view — drop last-good data.
      this.listResp = null;
      this.pageError = this.#toError(err);
      this.status = 'error';
    }
  }

  /** The Refresh button + the post-control reload re-fetch the current page. */
  refresh(): void {
    void this.loadJobs(this.cursorStack.length - 1);
  }

  /* ================================================================ */
  /* Selection + detail (the right-rail)                               */
  /* ================================================================ */

  /** Opens the per-job rail: `tasks.get` + the run stream + artifacts. */
  async selectJob(id: string): Promise<void> {
    const row = this.rawRows.find((r) => r.id === id);
    if (this.#client === null) return;
    this.selectedId = id;
    // Re-scope the run stream to the newly-opened job (close the prior).
    this.stream?.close();
    if (this.connection !== null && row !== undefined) {
      const s = new TaskRunStream(this.connection, { id, sessionID: row.parent_session_id });
      s.open();
      this.stream = s;
    } else {
      this.stream = null;
    }
    await this.#loadDetail(id);
    if (row !== undefined) {
      void this.#loadArtifacts(row);
    }
  }

  /** Closes the rail back to the idle hint. */
  clearSelection(): void {
    this.stream?.close();
    this.stream = null;
    this.selectedId = null;
    this.detail = null;
    this.siblings = [];
    this.artifacts = [];
  }

  async #loadDetail(id: string): Promise<void> {
    if (this.#client === null) return;
    this.detailLoading = true;
    try {
      const d = await this.#client.tasks.get<TaskDetail>(id);
      this.detail = d;
      // "Related Sessions" needs the sibling tasks under the same TaskGroup.
      const gid = d.task.group_id;
      if (gid !== undefined && gid !== '') {
        this.siblingsLoading = true;
        try {
          const sib = await this.#client.tasks.list<TaskListResponse>({ filter: { group_id: gid } });
          this.siblings = sib.rows.filter((r) => r.id !== id);
        } catch {
          this.siblings = [];
        } finally {
          this.siblingsLoading = false;
        }
      } else {
        this.siblings = [];
      }
    } catch (err) {
      this.detail = null;
      this.bulkResult = { ok: false, message: `Detail load failed: ${this.#toError(err).message}` };
    } finally {
      this.detailLoading = false;
    }
  }

  // Artifacts produced by the job — `artifacts.list` scoped to the job's
  // session + task (its run id). Honest-empty when the job produced none.
  async #loadArtifacts(row: TaskRow): Promise<void> {
    if (this.#client === null || this.connection === null) return;
    this.artifactsLoading = true;
    try {
      const resp = await this.#client.artifacts.list<ArtifactsListResponse>({
        scope: {
          tenant: this.connection.identity.tenant,
          user: this.connection.identity.user,
          session: row.parent_session_id,
          task: row.id
        }
      });
      this.artifacts = resp.rows ?? [];
    } catch {
      this.artifacts = [];
    } finally {
      this.artifactsLoading = false;
    }
  }

  /* ================================================================ */
  /* Filtering + pagination                                            */
  /* ================================================================ */

  #reloadFromFirstPage(): void {
    this.cursorStack = [{}];
    this.pageIndex = 1;
    void this.loadJobs(0);
  }

  submitSearch(): void {
    this.#reloadFromFirstPage();
  }

  setSearch(term: string): void {
    this.searchText = term;
  }

  clearFilters(): void {
    this.searchText = '';
    this.facetFilter = {};
    this.activeDerived = null;
    this.activeSavedId = null;
    this.approvalFacet = undefined;
    this.#reloadFromFirstPage();
  }

  toggleStatusFacet(s: TaskStatus): void {
    const cur = this.facetFilter.statuses ?? [];
    const next = cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s];
    this.facetFilter = { ...this.facetFilter, statuses: next.length > 0 ? next : undefined };
    this.activeSavedId = null;
    this.#reloadFromFirstPage();
  }

  toggleApprovalFacet(): void {
    this.approvalFacet = this.approvalFacet === true ? undefined : true;
    this.activeSavedId = null;
    this.#reloadFromFirstPage();
  }

  applyDerivedChip(chip: DerivedChip | null): void {
    this.activeDerived = chip;
    this.activeSavedId = null;
    this.facetFilter = chip !== null ? { ...chip.filter } : {};
    this.#reloadFromFirstPage();
  }

  nextPage(): void {
    const cur = this.listResp?.cursor;
    if (cur === undefined || !cur.next_page_token) return;
    this.cursorStack = [...this.cursorStack, cur];
    this.pageIndex += 1;
    void this.loadJobs(this.cursorStack.length - 1);
  }

  prevPage(): void {
    if (this.pageIndex <= 1) return;
    this.cursorStack = this.cursorStack.slice(0, -1);
    this.pageIndex -= 1;
    void this.loadJobs(this.cursorStack.length - 1);
  }

  onPageRequest(requested: number): void {
    if (requested > this.pageIndex) this.nextPage();
    else if (requested < this.pageIndex) this.prevPage();
  }

  changePageSize(size: number): void {
    this.pageSize = size;
    this.#reloadFromFirstPage();
  }

  setSelection(next: Set<string>): void {
    this.selected = next;
  }

  /* ================================================================ */
  /* Control verbs — the SHIPPED Phase 54 surface (§13 / D-128)        */
  /* ================================================================ */

  // Bulk control over the selected rows — one Phase 54 verb invocation
  // PER selected row. There is NO bulk endpoint (a single-call bulk method
  // would be a §13 parallel implementation; D-128 reaffirms this). Partial
  // completion is rendered inline (per-row pass/fail), never a silent abort.
  async bulkVerb(verb: 'cancel' | 'pause' | 'resume'): Promise<void> {
    if (this.#client === null || !this.canControl || this.selected.size === 0) return;
    this.bulkPending = true;
    this.bulkResult = null;
    const ids = [...this.selected];
    let ok = 0;
    let failed = 0;
    for (const id of ids) {
      try {
        await this.#client.control.dispatch(verb, id);
        ok += 1;
      } catch {
        failed += 1;
      }
    }
    this.bulkResult = {
      ok: failed === 0,
      message: `Bulk ${verb}: ${ok} succeeded, ${failed} failed (of ${ids.length}).`
    };
    this.selected = new Set<string>();
    this.bulkPending = false;
    await this.loadJobs(this.cursorStack.length - 1);
  }

  async bulkPrioritize(priority: number): Promise<void> {
    if (this.#client === null || !this.canControl || this.selected.size === 0) return;
    this.bulkPending = true;
    this.bulkResult = null;
    const ids = [...this.selected];
    let ok = 0;
    let failed = 0;
    for (const id of ids) {
      try {
        // Task-level priority only (D-072) — never session-level (D-065).
        await this.#client.control.prioritize(id, priority);
        ok += 1;
      } catch {
        failed += 1;
      }
    }
    this.bulkResult = {
      ok: failed === 0,
      message: `Bulk prioritize ${priority}: ${ok} succeeded, ${failed} failed (of ${ids.length}).`
    };
    this.selected = new Set<string>();
    this.bulkPending = false;
    await this.loadJobs(this.cursorStack.length - 1);
  }

  /* ================================================================ */
  /* Saved views (Console-DB-backed, D-061)                            */
  /* ================================================================ */

  async refreshSavedViews(): Promise<void> {
    if (this.savedFilters === null) return;
    try {
      const records = await this.savedFilters.list();
      this.savedViews = records.map((r) => ({ id: r.id, name: r.name }));
      this.savedFilterSpecs = new Map(records.map((r) => [r.id, r.filterSpec]));
    } catch {
      this.savedViews = [];
      this.savedFilterSpecs = new Map();
    }
  }

  applySavedView(id: string): void {
    const spec = this.savedFilterSpecs.get(id);
    if (spec === undefined) return;
    this.facetFilter = { ...spec };
    this.searchText = spec.search ?? '';
    this.activeDerived = null;
    this.activeSavedId = id;
    this.#reloadFromFirstPage();
  }

  async deleteSavedView(id: string): Promise<void> {
    if (this.savedFilters === null) return;
    await this.savedFilters.delete(id);
    if (this.activeSavedId === id) this.activeSavedId = null;
    await this.refreshSavedViews();
  }

  async saveCurrentFilter(): Promise<void> {
    const name = this.saveName.trim();
    if (name.length === 0 || this.savedFilters === null) return;
    const created = await this.savedFilters.create(name, { ...this.facetFilter });
    this.saveName = '';
    await this.refreshSavedViews();
    this.activeSavedId = created.id;
  }
}
