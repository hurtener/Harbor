<script lang="ts">
  // Harbor Console — Tasks page (`/tasks`), built on the D-121 design-
  // system foundation (CONVENTIONS.md; Phase 73d / D-123).
  //
  // The task-granularity counterpart to Sessions: a kanban board (the
  // §5 depth-bar primary view, in place of a flat DataTable) + a
  // list-mode DataTable toggle + a per-task detail with a bottom-dock
  // tab strip + the bulk-action toolbar. The page is built entirely
  // against the shared foundation:
  //   - the four-state `<PageState>` async contract (CONVENTIONS.md §4):
  //     Disconnected / Loading / Error / Empty, mutually exclusive; the
  //     Error state has a working Retry and suppresses any stale view.
  //   - the shared `ui/` inventory (CONVENTIONS.md §3): `PageHeader`,
  //     `FilterBar`, `SavedViewChips`, `DataTable`, `BulkActionBar`,
  //     `DetailRail`/`RailCard`, `Pagination`, `PageState`. Tasks-
  //     specific pieces (the kanban board, the per-task action bar, the
  //     detail tabs, the rail-card bodies) live in `components/tasks/`
  //     and compose `ui/` primitives underneath.
  //   - the unified `HarborClient` + `connection.ts` (CONVENTIONS.md §6):
  //     every Runtime read goes through `client.tasks.*`; every control
  //     verb through `client.control.*` (the SHIPPED Phase 54 verbs —
  //     no new control method, §13). No hand-rolled `fetch`, no page-
  //     local client, no direct `localStorage`.
  //   - Console-DB-backed `SavedViewChips` (D-061): the saved filters
  //     persist in the Console's IndexedDB store via `TasksSavedFilters`.
  //
  // The bulk-action toolbar + per-task action bar invoke the REAL
  // Phase 54 control methods; when the connection lacks the control
  // scope claim they render disabled-with-tooltip (CONVENTIONS.md §5 —
  // no stubbed action presented as done).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import { onMount } from 'svelte';
  import PageHeader from '$lib/components/ui/PageHeader.svelte';
  import FilterBar from '$lib/components/ui/FilterBar.svelte';
  import SavedViewChips, { type SavedView } from '$lib/components/ui/SavedViewChips.svelte';
  import DataTable, { type DataTableColumn } from '$lib/components/ui/DataTable.svelte';
  import BulkActionBar from '$lib/components/ui/BulkActionBar.svelte';
  import DetailRail from '$lib/components/ui/DetailRail.svelte';
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import StatusChip, { type StatusKind } from '$lib/components/ui/StatusChip.svelte';
  import Pagination from '$lib/components/ui/Pagination.svelte';
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  import KanbanBoard from '$lib/components/tasks/KanbanBoard.svelte';
  import SelectedTaskActionBar from '$lib/components/tasks/SelectedTaskActionBar.svelte';
  import TaskDetailTabs from '$lib/components/tasks/TaskDetailTabs.svelte';
  import RightRailSummary from '$lib/components/tasks/RightRailSummary.svelte';
  import RightRailParentSession from '$lib/components/tasks/RightRailParentSession.svelte';
  import RightRailCostBreakdown from '$lib/components/tasks/RightRailCostBreakdown.svelte';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import {
    resolveConnection,
    hasScope,
    DISCONNECTED_TOOLTIP,
    type RuntimeConnection
  } from '$lib/connection.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { TasksSavedFilters } from '$lib/db/saved_filters_tasks.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import type {
    TaskRow,
    TaskFilter,
    TaskStatus,
    TaskKind,
    TaskListResponse,
    TaskListAggregates,
    TaskListCursor,
    TaskDetail
  } from '$lib/protocol/tasks.js';

  /* ---- connection + client (CONVENTIONS.md §6) -------------------- */
  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  let connection = $state<RuntimeConnection | null>(null);
  let client = $state<ProtocolClient | null>(null);
  // The control verbs are an elevated tier (D-079) — bulk + per-task
  // control gate on the admin scope claim; without it the controls
  // render disabled-with-tooltip (CONVENTIONS.md §5).
  let canControl = $state(false);
  // Phase 83r disconnected predicate.
  let disconnected = $derived(connection === null);

  /* ---- page-level async state (the four-state contract) ----------- */
  let status = $state<PageStatus>('loading');
  let pageError = $state<ProtocolError | { code: string; message: string } | null>(null);

  /* ---- list state ------------------------------------------------- */
  let filter = $state<TaskFilter>({});
  let pageSize = $state(50);
  let listResp = $state<TaskListResponse | null>(null);
  let searchText = $state('');
  let viewMode = $state<'board' | 'list'>('board');
  // Cursor stack — index 0 is "first page". The page maintains a stack
  // of visited-page cursors so Pagination's prev/next is real (not a
  // fake "load more"): next pushes the response cursor, prev pops.
  let cursorStack = $state<TaskListCursor[]>([{}]);
  let pageIndex = $state(1); // 1-based, for the Pagination display

  let rows = $derived<TaskRow[]>(listResp?.rows ?? []);
  let aggregates = $derived<TaskListAggregates>(
    listResp?.aggregates ?? {
      pending: 0,
      running: 0,
      paused: 0,
      failed: 0,
      complete: 0,
      cancelled: 0
    }
  );
  // The filtered-view total = the sum of every per-status aggregate.
  let totalRows = $derived(
    aggregates.pending +
      aggregates.running +
      aggregates.paused +
      aggregates.failed +
      aggregates.complete +
      aggregates.cancelled
  );

  /* ---- selection + bulk actions ----------------------------------- */
  let selected = $state<Set<string>>(new Set<string>());
  let bulkPending = $state(false);
  let bulkResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- detail (nested PageState) ---------------------------------- */
  let selectedId = $state<string | null>(null);
  let detailStatus = $state<PageStatus>('empty');
  let detailError = $state<ProtocolError | { code: string; message: string } | null>(null);
  let detail = $state<TaskDetail | null>(null);

  let selectedRow = $derived<TaskRow | null>(
    rows.find((r) => r.id === selectedId) ?? null
  );

  /* ---- per-task action bar ---------------------------------------- */
  let actionPending = $state(false);
  let actionResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- drag-to-column toast --------------------------------------- */
  let dragToast = $state<string | null>(null);

  /* ---- saved views (Console-DB-backed, D-061) --------------------- */
  let savedFilters = $state<TasksSavedFilters | null>(null);
  let savedViews = $state<SavedView[]>([]);
  let savedFilterSpecs = $state<Map<string, TaskFilter>>(new Map());
  let activeSavedId = $state<string | null>(null);
  let saveName = $state('');

  /* ================================================================ */
  /* Loading                                                           */
  /* ================================================================ */

  function toError(err: unknown): { code: string; message: string } {
    if (err instanceof ProtocolError) {
      return { code: err.code, message: err.message };
    }
    return {
      code: 'runtime_error',
      message: err instanceof Error ? err.message : 'unknown error'
    };
  }

  async function loadTasks(cursorIdx = 0): Promise<void> {
    if (client === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    try {
      const resp = await client.tasks.list<TaskListResponse>({
        filter,
        page_size: pageSize,
        cursor: cursorStack[cursorIdx] ?? {}
      });
      listResp = resp;
      status = resp.rows.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      // The Error state suppresses any stale view — drop last-good data.
      listResp = null;
      pageError = toError(err);
      status = 'error';
    }
  }

  async function loadDetail(id: string): Promise<void> {
    if (client === null) {
      return;
    }
    detailStatus = 'loading';
    detailError = null;
    try {
      detail = await client.tasks.get<TaskDetail>(id);
      detailStatus = 'ready';
    } catch (err) {
      detail = null;
      detailError = toError(err);
      detailStatus = 'error';
    }
  }

  /* ================================================================ */
  /* Filtering + pagination                                            */
  /* ================================================================ */

  function applyFilter(next: TaskFilter): void {
    filter = next;
    cursorStack = [{}];
    pageIndex = 1;
    activeSavedId = null;
    void loadTasks(0);
  }

  function submitSearch(): void {
    applyFilter({ ...filter, search: searchText.trim() });
  }

  function clearFilters(): void {
    searchText = '';
    applyFilter({});
  }

  function toggleStatusFacet(s: TaskStatus): void {
    const cur = filter.statuses ?? [];
    const next = cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s];
    applyFilter({ ...filter, statuses: next.length > 0 ? next : undefined });
  }

  function toggleKindFacet(k: TaskKind): void {
    const cur = filter.kinds ?? [];
    const next = cur.includes(k) ? cur.filter((x) => x !== k) : [...cur, k];
    applyFilter({ ...filter, kinds: next.length > 0 ? next : undefined });
  }

  function nextPage(): void {
    const cur = listResp?.cursor;
    if (cur === undefined || !cur.next_page_token) return;
    cursorStack = [...cursorStack, cur];
    pageIndex += 1;
    void loadTasks(cursorStack.length - 1);
  }

  function prevPage(): void {
    if (pageIndex <= 1) return;
    cursorStack = cursorStack.slice(0, -1);
    pageIndex -= 1;
    void loadTasks(cursorStack.length - 1);
  }

  function changePageSize(size: number): void {
    pageSize = size;
    cursorStack = [{}];
    pageIndex = 1;
    void loadTasks(0);
  }

  function onPageRequest(requested: number): void {
    if (requested > pageIndex) {
      nextPage();
    } else if (requested < pageIndex) {
      prevPage();
    }
  }

  /* ================================================================ */
  /* Detail + selection                                                */
  /* ================================================================ */

  async function selectTask(id: string): Promise<void> {
    selectedId = id;
    actionResult = null;
    await loadDetail(id);
  }

  function toggleSelection(id: string, isSelected: boolean): void {
    const next = new Set(selected);
    if (isSelected) {
      next.add(id);
    } else {
      next.delete(id);
    }
    selected = next;
  }

  /* ================================================================ */
  /* Control verbs — the SHIPPED Phase 54 surface (§13)                */
  /* ================================================================ */

  async function runVerb(
    verb: 'cancel' | 'pause' | 'resume' | 'approve' | 'reject',
    id: string
  ): Promise<void> {
    if (client === null || !canControl) {
      return;
    }
    actionPending = true;
    actionResult = null;
    try {
      await client.control.dispatch(verb, id);
      actionResult = { ok: true, message: `${verb} dispatched for ${id}.` };
      await loadTasks(cursorStack.length - 1);
      if (selectedId === id) {
        await loadDetail(id);
      }
    } catch (err) {
      const e = toError(err);
      actionResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      actionPending = false;
    }
  }

  async function runPrioritize(id: string, priority: number): Promise<void> {
    if (client === null || !canControl) {
      return;
    }
    actionPending = true;
    actionResult = null;
    try {
      await client.control.prioritize(id, priority);
      actionResult = { ok: true, message: `Priority ${priority} set for ${id}.` };
      await loadTasks(cursorStack.length - 1);
    } catch (err) {
      const e = toError(err);
      actionResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      actionPending = false;
    }
  }

  // Bulk control over the selected rows. Each per-row call goes through
  // the shipped Phase 54 ControlSurface; partial completion is rendered
  // inline (per-row pass/fail), never a silent batch abort (§13).
  async function bulkVerb(verb: 'pause' | 'cancel'): Promise<void> {
    if (client === null || !canControl || selected.size === 0) {
      return;
    }
    bulkPending = true;
    bulkResult = null;
    const ids = [...selected];
    let ok = 0;
    let failed = 0;
    for (const id of ids) {
      try {
        await client.control.dispatch(verb, id);
        ok += 1;
      } catch {
        failed += 1;
      }
    }
    bulkResult = {
      ok: failed === 0,
      message: `Bulk ${verb}: ${ok} succeeded, ${failed} failed (of ${ids.length}).`
    };
    selected = new Set<string>();
    bulkPending = false;
    await loadTasks(cursorStack.length - 1);
  }

  // Card drag-across-columns invokes the matching shipped control verb.
  // Running → Paused = pause; Paused → Running = resume; Running →
  // Failed = cancel. Pending → Running is a server-initiated transition
  // (no client verb) — the drag is a no-op with an inline toast.
  function onDropCard(taskID: string, from: TaskStatus, to: TaskStatus): void {
    if (from === 'running' && to === 'paused') {
      void runVerb('pause', taskID);
    } else if (from === 'paused' && to === 'running') {
      void runVerb('resume', taskID);
    } else if (from === 'running' && to === 'failed') {
      void runVerb('cancel', taskID);
    } else if (to === 'running' && from === 'pending') {
      dragToast = 'Tasks transition to Running automatically — drag is a no-op.';
      setTimeout(() => (dragToast = null), 4000);
    } else {
      dragToast = `No control verb maps ${from} → ${to}.`;
      setTimeout(() => (dragToast = null), 4000);
    }
  }

  /* ================================================================ */
  /* Saved views (Console-DB-backed, D-061)                            */
  /* ================================================================ */

  async function refreshSavedViews(): Promise<void> {
    if (savedFilters === null) {
      return;
    }
    try {
      const records = await savedFilters.list();
      savedViews = records.map((r) => ({ id: r.id, name: r.name }));
      savedFilterSpecs = new Map(records.map((r) => [r.id, r.filterSpec]));
    } catch {
      savedViews = [];
      savedFilterSpecs = new Map();
    }
  }

  function applySavedView(id: string): void {
    const spec = savedFilterSpecs.get(id);
    if (spec === undefined) {
      return;
    }
    filter = { ...spec };
    searchText = spec.search ?? '';
    cursorStack = [{}];
    pageIndex = 1;
    activeSavedId = id;
    void loadTasks(0);
  }

  async function deleteSavedView(id: string): Promise<void> {
    if (savedFilters === null) {
      return;
    }
    await savedFilters.delete(id);
    if (activeSavedId === id) {
      activeSavedId = null;
    }
    await refreshSavedViews();
  }

  async function saveCurrentFilter(): Promise<void> {
    const name = saveName.trim();
    if (name.length === 0 || savedFilters === null) {
      return;
    }
    const created = await savedFilters.create(name, { ...filter });
    saveName = '';
    await refreshSavedViews();
    activeSavedId = created.id;
  }

  /* ================================================================ */
  /* Export                                                            */
  /* ================================================================ */

  function exportRows(): void {
    const blob = new Blob([JSON.stringify(rows, null, 2)], {
      type: 'application/json'
    });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'tasks.json';
    a.click();
    URL.revokeObjectURL(url);
  }

  /* ================================================================ */
  /* Boot                                                              */
  /* ================================================================ */

  onMount(() => {
    connection = resolveConnection();
    if (connection === null) {
      client = null;
      status = 'disconnected';
      return;
    }
    client = injectedClient ?? new HarborClient({ connection });
    canControl = hasScope(connection, 'admin');

    void (async () => {
      try {
        const db = await openListPageDB(connection!);
        const operator = await operatorIdOf(
          connection!.identity.tenant,
          connection!.identity.user
        );
        savedFilters = new TasksSavedFilters(db, operator);
        await refreshSavedViews();
      } catch {
        savedFilters = null;
      }
    })();

    void loadTasks(0);
  });

  /* ---- list-mode table column config ------------------------------ */
  const COLUMNS: DataTableColumn[] = [
    { key: 'id', label: 'Task ID' },
    { key: 'status', label: 'Status' },
    { key: 'kind', label: 'Kind' },
    { key: 'priority', label: 'Priority', numeric: true },
    { key: 'parent_session', label: 'Parent session' },
    { key: 'started', label: 'Started' },
    { key: 'duration', label: 'Duration', numeric: true },
    { key: 'tools', label: 'Tools', numeric: true }
  ];

  function rowKey(r: unknown): string {
    return (r as TaskRow).id;
  }

  const statusKinds: Record<string, StatusKind> = {
    pending: 'neutral',
    running: 'accent',
    paused: 'warning',
    complete: 'success',
    failed: 'danger',
    cancelled: 'neutral'
  };

  function durationLabel(ms: number): string {
    if (ms < 1000) return `${ms}ms`;
    const s = Math.round(ms / 1000);
    if (s < 60) return `${s}s`;
    return `${Math.round(s / 60)}m`;
  }
</script>

<svelte:head>
  <title>Tasks · Harbor Console</title>
</svelte:head>

<div class="page" data-testid="tasks-page">
  <PageHeader title="Tasks" subtitle="Task-granularity execution · kanban + bulk control" />

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews}
        activeId={activeSavedId}
        onselect={applySavedView}
        ondelete={(id) => void deleteSavedView(id)}
      />
      <input
        class="control save-input"
        type="text"
        placeholder="Save current as…"
        bind:value={saveName}
        data-testid="tasks-save-filter-name"
        disabled={savedFilters === null || disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onkeydown={(e) => e.key === 'Enter' && void saveCurrentFilter()}
      />
      <button
        type="button"
        class="control"
        data-testid="tasks-save-filter"
        disabled={savedFilters === null || saveName.trim().length === 0 || disconnected}
        title={disconnected
          ? DISCONNECTED_TOOLTIP
          : savedFilters === null
            ? 'Console-local saved-view store unavailable'
            : undefined}
        onclick={() => void saveCurrentFilter()}
      >
        Save view
      </button>
    {/snippet}

    {#snippet facets()}
      <div class="facet-chips" data-testid="tasks-facets">
        {#each ['pending', 'running', 'paused', 'failed', 'complete', 'cancelled'] as const as s (s)}
          <button
            type="button"
            class="chip"
            class:on={(filter.statuses ?? []).includes(s)}
            data-testid={`tasks-facet-${s}`}
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onclick={() => toggleStatusFacet(s)}
          >
            {s}
          </button>
        {/each}
        {#each ['foreground', 'background'] as const as k (k)}
          <button
            type="button"
            class="chip"
            class:on={(filter.kinds ?? []).includes(k)}
            data-testid={`tasks-facet-kind-${k}`}
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onclick={() => toggleKindFacet(k)}
          >
            {k}
          </button>
        {/each}
      </div>
    {/snippet}

    {#snippet search()}
      <input
        class="control search-input"
        type="search"
        placeholder="Search tasks…"
        bind:value={searchText}
        data-testid="tasks-search"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onkeydown={(e) => e.key === 'Enter' && submitSearch()}
      />
      <button
        type="button"
        class="control"
        data-testid="tasks-search-apply"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={submitSearch}
      >
        Apply
      </button>
      <button
        type="button"
        class="control"
        data-testid="tasks-filter-clear"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={clearFilters}
      >
        Clear
      </button>
    {/snippet}

    {#snippet actions()}
      <button
        type="button"
        class="control"
        data-testid="tasks-mode-toggle"
        onclick={() => (viewMode = viewMode === 'board' ? 'list' : 'board')}
      >
        {viewMode === 'board' ? 'List view' : 'Board view'}
      </button>
      <button
        type="button"
        class="control"
        data-testid="tasks-refresh"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={() => void loadTasks(cursorStack.length - 1)}
      >
        Refresh
      </button>
      <button
        type="button"
        class="control"
        data-testid="tasks-export"
        disabled={rows.length === 0 || disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={exportRows}
      >
        Export
      </button>
    {/snippet}
  </FilterBar>

  <BulkActionBar count={selected.size} onclear={() => (selected = new Set<string>())}>
    {#snippet actions()}
      <button
        type="button"
        class="control"
        data-testid="tasks-bulk-pause"
        disabled={!canControl || bulkPending}
        title={canControl
          ? undefined
          : 'Requires the control scope claim — task control is an elevated tier (D-079).'}
        onclick={() => void bulkVerb('pause')}
      >
        Pause selected
      </button>
      <button
        type="button"
        class="control"
        data-testid="tasks-bulk-cancel"
        disabled={!canControl || bulkPending}
        title={canControl
          ? undefined
          : 'Requires the control scope claim — task control is an elevated tier (D-079).'}
        onclick={() => void bulkVerb('cancel')}
      >
        Cancel selected
      </button>
    {/snippet}
  </BulkActionBar>
  {#if bulkResult !== null}
    <p
      class="inline-result"
      class:ok={bulkResult.ok}
      class:err={!bulkResult.ok}
      data-testid="tasks-bulk-result"
    >
      {bulkResult.message}
    </p>
  {/if}
  {#if dragToast !== null}
    <p class="inline-result" data-testid="tasks-drag-toast">{dragToast}</p>
  {/if}

  <div class="layout">
    <div class="main-col">
      <PageState status={status} error={pageError} onretry={() => void loadTasks(cursorStack.length - 1)}>
        {#snippet skeleton()}
          <div class="board-skeleton" aria-hidden="true">
            {#each [0, 1, 2, 3] as i (i)}
              <span class="skeleton-col"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="tasks-empty">
            <p class="headline">No tasks match the current view</p>
            <p class="detail">No tasks running, or the active filters yield zero rows.</p>
            <button type="button" class="control" onclick={clearFilters}>Clear filters</button>
          </div>
        {/snippet}

        {#if viewMode === 'board'}
          <KanbanBoard
            {rows}
            {aggregates}
            {selected}
            activeId={selectedId}
            onselect={(id) => void selectTask(id)}
            ontoggle={toggleSelection}
            ondropcard={onDropCard}
          />
        {:else}
          <DataTable
            columns={COLUMNS}
            rows={rows}
            {rowKey}
            selectable
            {selected}
            onselectionchange={(s) => (selected = s)}
            onrowclick={(r) => void selectTask((r as TaskRow).id)}
          >
            {#snippet row(r)}
              {@const t = r as TaskRow}
              <td data-testid="tasks-table-row" class:row-active={t.id === selectedId}>{t.id}</td>
              <td><StatusChip kind={statusKinds[t.status] ?? 'neutral'} label={t.status} /></td>
              <td>{t.kind}</td>
              <td>{t.priority}</td>
              <td>{t.parent_session_id}</td>
              <td>{t.started_at}</td>
              <td>{durationLabel(t.duration_ms)}</td>
              <td>{t.tool_count}</td>
            {/snippet}
          </DataTable>
        {/if}
      </PageState>

      {#if status === 'ready' || status === 'empty'}
        <Pagination
          page={pageIndex}
          pageSize={pageSize}
          total={totalRows}
          onpage={onPageRequest}
          onpagesize={changePageSize}
        />
      {/if}

      <SelectedTaskActionBar
        task={selectedRow}
        {canControl}
        pending={actionPending}
        result={actionResult}
        onverb={(verb, id) => void runVerb(verb, id)}
        onprioritize={(id, p) => void runPrioritize(id, p)}
      />

      <TaskDetailTabs detail={detail} loading={detailStatus === 'loading'} />
      {#if detailStatus === 'error' && detailError !== null}
        <p class="inline-result err" data-testid="tasks-detail-error">
          {detailError.code}: {detailError.message}
        </p>
      {/if}
    </div>

    <DetailRail>
      <RailCard title="Summary">
        <RightRailSummary {aggregates} />
      </RailCard>
      <RailCard title="Parent session">
        <RightRailParentSession {detail} />
      </RailCard>
      <RailCard title="Cost breakdown">
        <RightRailCostBreakdown {detail} />
      </RailCard>
    </DetailRail>
  </div>
</div>

<style>
  .page {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-6);
  }

  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    align-items: start;
  }

  .main-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: var(--space-0);
  }

  .control {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .control:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .search-input {
    flex: 1;
    min-width: var(--size-search-min);
  }

  .save-input {
    width: var(--size-search-min);
  }

  .facet-chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }

  .chip {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .chip.on {
    color: var(--color-accent);
    border-color: var(--color-accent);
    font-weight: 600;
  }

  .board-skeleton {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: var(--space-3);
  }

  .skeleton-col {
    height: var(--space-12);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .empty-block {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-12) var(--space-4);
    text-align: center;
  }

  .empty-block .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-block .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .row-active {
    color: var(--color-accent);
    font-weight: 600;
  }

  .inline-result {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .inline-result.ok {
    color: var(--color-success);
  }

  .inline-result.err {
    color: var(--color-danger);
  }
</style>
