<script lang="ts">
  // Harbor Console — Tasks page (`/tasks`) — Phase 108i rebuild (D-181;
  // supersedes the Phase 73d / D-123 pre-chrome layout + the placeholder
  // detail-tabs component).
  //
  // (the placeholder detail-tabs component is deleted; the bottom dock now
  // renders live run-scoped event data). The task-granularity counterpart
  // to Sessions, composed as a single viewport-locked MODE-SWITCH
  // (PAGE-POLISH §6 — the mock crams board +
  // detail-bar + dock + rail simultaneously, which cannot fit one viewport
  // without a page scroll):
  //   - BOARD/LIST mode (default): a carded filter strip + the kanban board
  //     (or the list `DataTable`) filling the viewport + a right-rail live
  //     board summary. Cursor pagination is real.
  //   - DETAIL mode (a card/row selected): the SAME page swaps its main
  //     region to a compact task header + the real control action bar + the
  //     bottom-dock tab strip (ONE internally-scrolling card), and the rail
  //     to Summary / Parent Session / Cost. `← Board` returns.
  //
  // Every datum + action is real-wired (PAGE-POLISH §3 — live-verified):
  //   - board / list / cards ← `tasks.list` (rows + per-status aggregates)
  //   - detail header / Details / Input / Output ← `tasks.get`
  //   - Events / Control / Interventions / Group / Cost ← a RUN-scoped
  //     `events.subscribe` projection owned by `TaskRunStream` (ONE
  //     subscription feeds the dock AND the rail cost/event figures)
  //   - cost / tokens ← `llm.cost.recorded` aggregated client-side (the
  //     all-zero `tasks.get.cost` is NOT used)
  //   - bulk + per-task Cancel/Pause/Resume/Prioritize/Approve/Reject ←
  //     the shipped Phase 54 control verbs via `client.control.*`,
  //     control-scope gated (D-066) — never a stubbed placeholder.
  //
  // Honest states for the unshipped surfaces (PAGE-POLISH §1): the Logs tab
  // needs the Phase 73 `state.history` surface; search is Console-local;
  // the card shows the session id (no `agent_name` on the row); the parent-
  // session card shows `—` for the sparse registry fields.
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient +
  // connection.ts only (CONVENTIONS.md §6).
  import { onMount } from 'svelte';
  import {
    DataTable,
    StatusChip,
    Pagination,
    PageState,
    SavedViewChips,
    DetailRail,
    RailCard,
    type DataTableColumn,
    type PageStatus,
    type StatusKind,
    type SavedView
  } from '$lib/components/ui/index.js';
  import KanbanBoard from '$lib/components/tasks/KanbanBoard.svelte';
  import TaskDetailHeader from '$lib/components/tasks/TaskDetailHeader.svelte';
  import TaskBottomDock from '$lib/components/tasks/TaskBottomDock.svelte';
  import SelectedTaskActionBar from '$lib/components/tasks/SelectedTaskActionBar.svelte';
  import TaskSummaryCard from '$lib/components/tasks/TaskSummaryCard.svelte';
  import RightRailSummary from '$lib/components/tasks/RightRailSummary.svelte';
  import RightRailParentSession from '$lib/components/tasks/RightRailParentSession.svelte';
  import RightRailCostBreakdown from '$lib/components/tasks/RightRailCostBreakdown.svelte';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import { TaskRunStream } from '$lib/tasks/run-stream.svelte.js';
  import { formatRelative } from '$lib/sessions/format.js';
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
  // Task control is an elevated tier (D-066/D-079); without the claim the
  // bulk + per-task controls render disabled-with-tooltip (CONVENTIONS.md §5).
  let canControl = $state(false);
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
  // Cursor stack — index 0 is "first page". next pushes, prev pops.
  let cursorStack = $state<TaskListCursor[]>([{}]);
  let pageIndex = $state(1);

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

  /* ---- detail (mode-switch) --------------------------------------- */
  let selectedId = $state<string | null>(null);
  let detailStatus = $state<PageStatus>('empty');
  let detailError = $state<ProtocolError | { code: string; message: string } | null>(null);
  let detail = $state<TaskDetail | null>(null);
  // The run-scoped event stream for the open task — owns ONE subscription
  // the dock + the rail Summary / Cost all read. `$state` so the async
  // re-scope triggers the reactive re-render.
  let stream = $state<TaskRunStream | null>(null);

  let selectedRow = $derived<TaskRow | null>(rows.find((r) => r.id === selectedId) ?? null);
  let inDetail = $derived(selectedId !== null);

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

  /* ================================================================ */
  /* Loading                                                           */
  /* ================================================================ */

  function toError(err: unknown): { code: string; message: string } {
    if (err instanceof ProtocolError) {
      return { code: err.code, message: err.message };
    }
    return { code: 'runtime_error', message: err instanceof Error ? err.message : 'unknown error' };
  }

  // Console-local substring filter over the loaded page — there is no
  // shipped runtime-side `search.tasks` (brief 11 §CC-4 / wave-13-extends);
  // honest fallback (CLAUDE.md §13), never a fabricated server search.
  let displayRows = $derived.by<TaskRow[]>(() => {
    const q = searchText.trim().toLowerCase();
    if (q === '') return rows;
    return rows.filter(
      (r) =>
        r.id.toLowerCase().includes(q) ||
        r.query.toLowerCase().includes(q) ||
        r.description.toLowerCase().includes(q) ||
        r.parent_session_id.toLowerCase().includes(q) ||
        (r.error_class ?? '').toLowerCase().includes(q)
    );
  });

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
      listResp = null;
      pageError = toError(err);
      status = 'error';
    }
  }

  async function loadDetail(id: string): Promise<void> {
    if (client === null) return;
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

  function onPageRequest(requested: number): void {
    if (requested > pageIndex) nextPage();
    else if (requested < pageIndex) prevPage();
  }

  function changePageSize(size: number): void {
    pageSize = size;
    cursorStack = [{}];
    pageIndex = 1;
    void loadTasks(0);
  }

  /* ================================================================ */
  /* Mode-switch: select / back                                        */
  /* ================================================================ */

  async function selectTask(id: string): Promise<void> {
    const row = rows.find((r) => r.id === id);
    if (row === undefined || connection === null) return;
    // Re-scope the run stream to the newly-opened task (close the prior).
    stream?.close();
    const s = new TaskRunStream(connection, { id, sessionID: row.parent_session_id });
    s.open();
    stream = s;
    selectedId = id;
    actionResult = null;
    await loadDetail(id);
  }

  function backToBoard(): void {
    stream?.close();
    stream = null;
    selectedId = null;
    detail = null;
    detailStatus = 'empty';
    actionResult = null;
  }

  function toggleSelection(id: string, isSelected: boolean): void {
    const next = new Set(selected);
    if (isSelected) next.add(id);
    else next.delete(id);
    selected = next;
  }

  /* ================================================================ */
  /* Control verbs — the SHIPPED Phase 54 surface (§13)                */
  /* ================================================================ */

  async function runVerb(
    verb: 'cancel' | 'pause' | 'resume' | 'approve' | 'reject',
    id: string
  ): Promise<void> {
    if (client === null || !canControl) return;
    actionPending = true;
    actionResult = null;
    try {
      await client.control.dispatch(verb, id);
      actionResult = { ok: true, message: `${verb} dispatched.` };
      await loadTasks(cursorStack.length - 1);
      if (selectedId === id) await loadDetail(id);
    } catch (err) {
      const e = toError(err);
      actionResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      actionPending = false;
    }
  }

  async function runPrioritize(id: string, priority: number): Promise<void> {
    if (client === null || !canControl) return;
    actionPending = true;
    actionResult = null;
    try {
      await client.control.prioritize(id, priority);
      actionResult = { ok: true, message: `Priority ${priority} set.` };
      await loadTasks(cursorStack.length - 1);
    } catch (err) {
      const e = toError(err);
      actionResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      actionPending = false;
    }
  }

  // Bulk control over the selected rows — iterates the shipped per-run
  // verbs; partial completion is rendered inline (never a silent abort, §13).
  async function bulkVerb(verb: 'pause' | 'cancel'): Promise<void> {
    if (client === null || !canControl || selected.size === 0) return;
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

  // Card drag-across-columns invokes the matching shipped control verb;
  // pending→running is a server-initiated transition (no client verb).
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
    if (savedFilters === null) return;
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
    if (spec === undefined) return;
    filter = { ...spec };
    searchText = spec.search ?? '';
    cursorStack = [{}];
    pageIndex = 1;
    activeSavedId = id;
    void loadTasks(0);
  }

  async function deleteSavedView(id: string): Promise<void> {
    if (savedFilters === null) return;
    await savedFilters.delete(id);
    if (activeSavedId === id) activeSavedId = null;
    await refreshSavedViews();
  }

  async function saveCurrentView(): Promise<void> {
    if (savedFilters === null) return;
    const name = (searchText.trim() || 'Tasks view').slice(0, 60);
    const created = await savedFilters.create(name, { ...filter });
    await refreshSavedViews();
    activeSavedId = created.id;
  }

  /* ================================================================ */
  /* Export                                                            */
  /* ================================================================ */

  function exportRows(): void {
    const blob = new Blob([JSON.stringify(displayRows, null, 2)], { type: 'application/json' });
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
        const operator = await operatorIdOf(connection!.identity.tenant, connection!.identity.user);
        savedFilters = new TasksSavedFilters(db, operator);
        await refreshSavedViews();
      } catch {
        savedFilters = null;
      }
    })();

    void loadTasks(0);
    return () => stream?.close();
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
    if (ms <= 0) return '—';
    if (ms < 1000) return `${ms}ms`;
    const s = Math.round(ms / 1000);
    if (s < 60) return `${s}s`;
    return `${Math.round(s / 60)}m`;
  }
</script>

<svelte:head>
  <title>Tasks · Harbor Console</title>
</svelte:head>

<section class="tasks-page" data-testid="tasks-page">
  {#if inDetail && selectedRow !== null && stream !== null}
    <!-- ============================ DETAIL MODE ========================= -->
    <div class="detail-grid">
      <div class="main">
        <section class="panel card detail-card">
          <PageState status={detailStatus} error={detailError} onretry={() => void loadDetail(selectedId ?? '')}>
            {#snippet empty()}
              <p class="empty-headline">Task not found</p>
              <p class="empty-detail">No task with id <code>{selectedId}</code> is visible in your scope.</p>
              <button type="button" class="control" onclick={backToBoard}>← Back to board</button>
            {/snippet}

            <TaskDetailHeader task={selectedRow} onback={backToBoard}>
              {#snippet actions()}
                <SelectedTaskActionBar
                  task={selectedRow}
                  {canControl}
                  pending={actionPending}
                  result={actionResult}
                  onverb={(verb, id) => void runVerb(verb, id)}
                  onprioritize={(id, p) => void runPrioritize(id, p)}
                />
              {/snippet}
            </TaskDetailHeader>

            <TaskBottomDock
              {stream}
              {detail}
              detailLoading={detailStatus === 'loading'}
              {canControl}
            />
          </PageState>
        </section>
      </div>

      <DetailRail>
        <RailCard title="Summary">
          <TaskSummaryCard task={selectedRow} cost={stream.cost} eventCount={stream.eventCount} />
        </RailCard>
        <RailCard title="Parent session">
          <RightRailParentSession {detail} />
        </RailCard>
        <RailCard title="Cost breakdown (USD)">
          <RightRailCostBreakdown cost={stream.cost} />
        </RailCard>
      </DetailRail>
    </div>
  {:else}
    <!-- ========================== BOARD / LIST MODE ===================== -->
    <section class="panel card filter-card">
      <header class="filter-head">
        <input
          type="search"
          class="search"
          placeholder="Search tasks by id, agent, session, error…"
          data-testid="tasks-search"
          bind:value={searchText}
          disabled={disconnected}
          title={disconnected
            ? DISCONNECTED_TOOLTIP
            : 'Searching the loaded page only — runtime-side search.tasks is unshipped (brief 11 §CC-4)'}
        />
        <div class="toolbar">
          <div class="seg" role="group" aria-label="View mode">
            <button
              type="button"
              class="seg-btn"
              class:on={viewMode === 'board'}
              data-testid="tasks-mode-board"
              onclick={() => (viewMode = 'board')}
            >
              Board
            </button>
            <button
              type="button"
              class="seg-btn"
              class:on={viewMode === 'list'}
              data-testid="tasks-mode-list"
              onclick={() => (viewMode = 'list')}
            >
              List
            </button>
          </div>
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
            disabled={displayRows.length === 0 || disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onclick={exportRows}
          >
            Export
          </button>
        </div>
      </header>

      <div class="facets">
        {#each ['pending', 'running', 'paused', 'complete', 'failed'] as const as s (s)}
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
        <span class="facet-sep" aria-hidden="true"></span>
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

      <div class="saved-row">
        <SavedViewChips
          views={savedViews}
          activeId={activeSavedId}
          onselect={applySavedView}
          ondelete={(id) => void deleteSavedView(id)}
        />
        <button
          type="button"
          class="control small"
          data-testid="tasks-save-view"
          disabled={savedFilters === null || disconnected}
          title={disconnected
            ? DISCONNECTED_TOOLTIP
            : savedFilters === null
              ? 'Console-local saved-view store unavailable'
              : 'Save the current filter as a view'}
          onclick={() => void saveCurrentView()}
        >
          Save view
        </button>
        {#if (filter.statuses?.length ?? 0) > 0 || (filter.kinds?.length ?? 0) > 0 || searchText.trim() !== ''}
          <button type="button" class="control small ghost" data-testid="tasks-filter-clear" onclick={clearFilters}>
            Clear
          </button>
        {/if}
      </div>

      {#if selected.size > 0}
        <div class="bulk-bar" data-testid="tasks-bulk-bar">
          <span class="bulk-count">{selected.size} selected</span>
          <button
            type="button"
            class="control small"
            data-testid="tasks-bulk-pause"
            disabled={!canControl || bulkPending}
            title={canControl ? 'Pause the selected runs' : 'Requires the control-plane scope claim (D-066)'}
            onclick={() => void bulkVerb('pause')}
          >
            Pause selected
          </button>
          <button
            type="button"
            class="control small"
            data-testid="tasks-bulk-cancel"
            disabled={!canControl || bulkPending}
            title={canControl ? 'Cancel the selected runs' : 'Requires the control-plane scope claim (D-066)'}
            onclick={() => void bulkVerb('cancel')}
          >
            Cancel selected
          </button>
          <button type="button" class="control small ghost" onclick={() => (selected = new Set<string>())}>Clear</button>
          {#if bulkResult !== null}
            <span class="bulk-result" class:err={!bulkResult.ok} data-testid="tasks-bulk-result">{bulkResult.message}</span>
          {/if}
        </div>
      {/if}
      {#if dragToast !== null}
        <p class="drag-toast" data-testid="tasks-drag-toast">{dragToast}</p>
      {/if}
    </section>

    <div class="layout">
      <section class="panel card board-card">
        <PageState {status} error={pageError} onretry={() => void loadTasks(cursorStack.length - 1)}>
          {#snippet skeleton()}
            <div class="board-skeleton" aria-hidden="true">
              {#each [0, 1, 2, 3, 4] as i (i)}
                <span class="skeleton-col"></span>
              {/each}
            </div>
          {/snippet}
          {#snippet empty()}
            <div class="empty-block" data-testid="tasks-empty">
              <p class="empty-headline">No tasks match the current view</p>
              <p class="empty-detail">No tasks running, or the active filters yield zero rows. Start a run in Live Runtime.</p>
              <button type="button" class="control" onclick={clearFilters}>Clear filters</button>
            </div>
          {/snippet}

          {#if viewMode === 'board'}
            <KanbanBoard
              rows={displayRows}
              {aggregates}
              {selected}
              activeId={selectedId}
              onselect={(id) => void selectTask(id)}
              ontoggle={toggleSelection}
              ondropcard={onDropCard}
            />
          {:else}
            <div class="table-scroll">
              <DataTable
                columns={COLUMNS}
                rows={displayRows}
                {rowKey}
                selectable
                {selected}
                onselectionchange={(s) => (selected = s)}
                onrowclick={(r) => void selectTask((r as TaskRow).id)}
              >
                {#snippet row(r)}
                  {@const t = r as TaskRow}
                  <td><span class="task-id" data-testid="tasks-table-row">{t.id}</span></td>
                  <td><StatusChip kind={statusKinds[t.status] ?? 'neutral'} label={t.status} /></td>
                  <td>{t.kind}</td>
                  <td class="numeric">{t.priority}</td>
                  <td class="mono">{t.parent_session_id}</td>
                  <td>{formatRelative(t.started_at)}</td>
                  <td class="numeric">{durationLabel(t.duration_ms)}</td>
                  <td class="numeric">{t.tool_count}</td>
                {/snippet}
              </DataTable>
            </div>
          {/if}
        </PageState>

        {#if status === 'ready' || status === 'empty'}
          <Pagination
            page={pageIndex}
            {pageSize}
            total={totalRows}
            pageSizeOptions={[50, 100, 250]}
            onpage={onPageRequest}
            onpagesize={changePageSize}
          />
        {/if}
      </section>

      <section class="panel card rail-card">
        <h2 class="panel-title">Board summary</h2>
        <RightRailSummary {aggregates} />
        <p class="rail-hint">Click a card to open its detail — events, control history, interventions and cost.</p>
      </section>
    </div>
  {/if}
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls; only the board columns / table + the detail dock +
     the rail scroll internally (PAGE-POLISH §6). */
  .tasks-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow: hidden;
  }

  /* The carded surface — same vocabulary as Overview / Sessions / Events. */
  .card {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-width: 0;
  }

  .panel-title {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  /* ---- filter card (fixed height) ---- */
  .filter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  .filter-head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .search {
    flex: 1 1 var(--size-search-min);
    min-width: var(--size-search-min);
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
  }

  .toolbar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .seg {
    display: inline-flex;
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .seg-btn {
    background: var(--color-bg);
    color: var(--color-text-muted);
    border: none;
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .seg-btn.on {
    background: var(--color-accent-soft);
    color: var(--color-accent);
    font-weight: 600;
  }

  .facets {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-1);
  }

  .facet-sep {
    align-self: stretch;
    border-left: var(--border-hairline);
    margin: var(--space-0) var(--space-1);
  }

  .chip {
    background: var(--color-bg);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
    text-transform: capitalize;
  }

  .chip.on {
    color: var(--color-accent);
    border-color: var(--color-accent);
    font-weight: 600;
  }

  .saved-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .control {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .control.small {
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
  }

  .control.ghost {
    background: none;
  }

  .control:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .bulk-bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .bulk-count {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .bulk-result {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .bulk-result.err {
    color: var(--color-danger);
  }

  .drag-toast {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  /* ---- board / list layout (fills remaining height) ---- */
  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  .board-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 0;
  }

  /* PageState wraps the board/table; let its content fill + scroll. */
  .board-card :global(.page-state) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }

  .table-scroll {
    flex: 1;
    min-height: 0;
    overflow: auto;
  }

  .rail-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-height: 0;
    overflow-y: auto;
  }

  .rail-hint {
    margin: var(--space-2) var(--space-0) var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .board-skeleton {
    display: grid;
    grid-template-columns: repeat(5, 1fr);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
  }

  .skeleton-col {
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

  /* ---- detail mode ---- */
  .detail-grid {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  .main {
    display: flex;
    flex-direction: column;
    min-width: 0;
    min-height: 0;
  }

  .detail-card {
    display: flex;
    flex-direction: column;
    flex: 1;
    gap: var(--space-3);
    min-height: 0;
  }

  .detail-card :global(.page-state) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .task-id,
  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .task-id {
    color: var(--color-accent);
    font-weight: 600;
  }

  .numeric {
    text-align: right;
  }

  td {
    padding: var(--space-2) var(--space-3);
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
