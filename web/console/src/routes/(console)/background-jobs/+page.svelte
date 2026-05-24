<script lang="ts">
  // Harbor Console — Background Jobs page (`/background-jobs`), built on
  // the D-121 design-system foundation (CONVENTIONS.md; Phase 73h /
  // D-128).
  //
  // The queue view for planner-spawned background tasks — the focused
  // `tasks.list` projection with `kinds: ['background']` (the plural
  // []TaskKind slice — the A2 audit fix; never a `type=background`
  // scalar). The page is built entirely against the shared foundation:
  //   - the four-state `<PageState>` async contract (CONVENTIONS.md §4):
  //     Disconnected / Loading / Error / Empty, mutually exclusive; the
  //     Error state has a working Retry and suppresses any stale view.
  //   - the shared `ui/` inventory (CONVENTIONS.md §3): `PageHeader`,
  //     `FilterBar`, `SavedViewChips`, `DataTable`, `BulkActionBar`,
  //     `DetailRail`/`RailCard`, `Pagination`, `PageState`. The
  //     Background-Jobs-specific pieces (the queue table, the bulk
  //     toolbar, the orphan badge, the right-rail tabs, the saved-filter
  //     chips) live in `components/background-jobs/` and compose `ui/`
  //     primitives underneath — they never fork a primitive.
  //   - the unified `HarborClient` + `connection.ts` (CONVENTIONS.md §6):
  //     every Runtime read goes through `client.tasks.*`; every control
  //     verb through `client.control.*` (the SHIPPED Phase 54 verbs —
  //     no new control method, §13). No hand-rolled `fetch`, no page-
  //     local client, no direct `localStorage`.
  //   - Console-DB-backed saved filters (D-061) via the
  //     `BackgroundJobsSavedFilters` typed wrapper over the shipped
  //     `saved_filters` table (no new table).
  //
  // The bulk-action toolbar invokes the REAL Phase 54 control verbs once
  // per selected row (no bulk endpoint — D-128 reaffirms §13); when the
  // connection lacks the control scope claim the toolbar renders
  // disabled-with-tooltip (CONVENTIONS.md §5 — no stubbed action).
  //
  // The `AwaitTask` orphan detector is a pure Console-side cross-check
  // (D-128) — it adds no Protocol field and issues no Protocol call.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import { onMount } from 'svelte';
  import PageHeader from '$lib/components/ui/PageHeader.svelte';
  import FilterBar from '$lib/components/ui/FilterBar.svelte';
  import { type SavedView } from '$lib/components/ui/SavedViewChips.svelte';
  import BulkActionBar from '$lib/components/ui/BulkActionBar.svelte';
  import DetailRail from '$lib/components/ui/DetailRail.svelte';
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import Pagination from '$lib/components/ui/Pagination.svelte';
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  import QueueTable from '$lib/components/background-jobs/QueueTable.svelte';
  import BulkToolbar from '$lib/components/background-jobs/BulkToolbar.svelte';
  import RightRail from '$lib/components/background-jobs/RightRail.svelte';
  import SavedFilterChips, {
    type DerivedChip,
    STUCK_CHIP_ID
  } from '$lib/components/background-jobs/SavedFilterChips.svelte';
  import { detectOrphans } from '$lib/background-jobs/orphan-detector.js';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import {
    resolveConnection,
    hasScope,
    DISCONNECTED_TOOLTIP,
    type RuntimeConnection
  } from '$lib/connection.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { BackgroundJobsSavedFilters } from '$lib/db/saved_filters_background_jobs.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import type {
    TaskRow,
    TaskFilter,
    TaskStatus,
    TaskListResponse,
    TaskListCursor
  } from '$lib/protocol/tasks.js';

  /* ---- connection + client (CONVENTIONS.md §6) -------------------- */
  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  let connection = $state<RuntimeConnection | null>(null);
  let client = $state<ProtocolClient | null>(null);
  // Task control is an elevated tier (D-066 / D-079) — the bulk toolbar
  // gates on the admin scope claim; without it the controls render
  // disabled-with-tooltip (CONVENTIONS.md §5).
  let canControl = $state(false);
  // Phase 83r disconnected predicate.
  let disconnected = $derived(connection === null);

  /* ---- page-level async state (the four-state contract) ----------- */
  let status = $state<PageStatus>('loading');
  let pageError = $state<ProtocolError | { code: string; message: string } | null>(null);

  /* ---- list state ------------------------------------------------- */
  // The queue is ALWAYS `kinds: ['background']` — that is the page's
  // identity. A facet chip layers additional facets on top; the
  // background-kind filter is never removed.
  let facetFilter = $state<TaskFilter>({});
  let searchText = $state('');
  let pageSize = $state(50);
  let listResp = $state<TaskListResponse | null>(null);
  // Cursor stack — index 0 is the first page; next pushes, prev pops.
  let cursorStack = $state<TaskListCursor[]>([{}]);
  let pageIndex = $state(1);
  // The active built-in derived chip (Active only / High-priority /
  // Stuck > 1h / Recently failed) — null when none is active.
  let activeDerived = $state<DerivedChip | null>(null);
  // The status-facet multi-select chips.
  const STATUS_FACETS: TaskStatus[] = [
    'pending',
    'running',
    'paused',
    'complete',
    'failed',
    'cancelled'
  ];
  // The has-pending-approval facet: undefined = off, true = only.
  let approvalFacet = $state<boolean | undefined>(undefined);

  // The effective wire filter — always background-kind, plus the active
  // facets / search / derived chip.
  let effectiveFilter = $derived<TaskFilter>({
    ...facetFilter,
    kinds: ['background'],
    ...(searchText.trim() !== '' ? { search: searchText.trim() } : {}),
    ...(approvalFacet !== undefined ? { has_pending_approval: approvalFacet } : {})
  });

  // The raw rows from the last `tasks.list` page.
  let rawRows = $derived<TaskRow[]>(listResp?.rows ?? []);

  // The `Stuck > 1h` derived chip is a Console-local row-level
  // post-filter (D-061) — no activity for over an hour. Every other
  // chip maps onto a server facet, so only this one post-filters.
  let rows = $derived<TaskRow[]>(
    activeDerived?.id === STUCK_CHIP_ID
      ? rawRows.filter((r) => {
          const last = Date.parse(r.last_activity_at);
          return !Number.isNaN(last) && Date.now() - last > 3_600_000;
        })
      : rawRows
  );

  // The orphan set — pure Console-side cross-check over the snapshot
  // (D-128). Recomputed each time the rows change; O(N).
  let orphans = $derived<Set<string>>(detectOrphans(rawRows));

  // The filtered-view total — the sum of the per-status aggregates.
  let totalRows = $derived(() => {
    const a = listResp?.aggregates;
    if (a === undefined) return 0;
    return a.pending + a.running + a.paused + a.failed + a.complete + a.cancelled;
  });

  /* ---- selection + bulk actions ----------------------------------- */
  let selected = $state<Set<string>>(new Set<string>());
  let bulkPending = $state(false);
  let bulkResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- detail (nested PageState in the rail) ---------------------- */
  let selectedId = $state<string | null>(null);
  let detailLoading = $state(false);
  let detail = $state<import('$lib/protocol/tasks.js').TaskDetail | null>(null);
  let siblings = $state<TaskRow[]>([]);
  let siblingsLoading = $state(false);

  /* ---- saved views (Console-DB-backed, D-061) --------------------- */
  let savedFilters = $state<BackgroundJobsSavedFilters | null>(null);
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

  async function loadJobs(cursorIdx = 0): Promise<void> {
    if (client === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    try {
      const resp = await client.tasks.list<TaskListResponse>({
        filter: effectiveFilter,
        page_size: pageSize,
        cursor: cursorStack[cursorIdx] ?? {}
      });
      listResp = resp;
      // Empty is computed AFTER the Stuck-chip post-filter — a queue
      // whose rows all dropped under the derived chip is still "empty".
      const visible =
        activeDerived?.id === STUCK_CHIP_ID
          ? resp.rows.filter((r) => {
              const last = Date.parse(r.last_activity_at);
              return !Number.isNaN(last) && Date.now() - last > 3_600_000;
            })
          : resp.rows;
      status = visible.length === 0 ? 'empty' : 'ready';
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
    detailLoading = true;
    try {
      const d = await client.tasks.get<import('$lib/protocol/tasks.js').TaskDetail>(id);
      detail = d;
      // The "Related Sessions" tab needs the sibling tasks under the
      // same TaskGroup — `tasks.list?group_id=…`. Only fetched when the
      // job is a group member.
      const gid = d.task.group_id;
      if (gid !== undefined && gid !== '') {
        siblingsLoading = true;
        try {
          const sibResp = await client.tasks.list<TaskListResponse>({
            filter: { group_id: gid }
          });
          siblings = sibResp.rows.filter((r) => r.id !== id);
        } catch {
          siblings = [];
        } finally {
          siblingsLoading = false;
        }
      } else {
        siblings = [];
      }
    } catch (err) {
      detail = null;
      bulkResult = { ok: false, message: `Detail load failed: ${toError(err).message}` };
    } finally {
      detailLoading = false;
    }
  }

  /* ================================================================ */
  /* Filtering + pagination                                            */
  /* ================================================================ */

  function reloadFromFirstPage(): void {
    cursorStack = [{}];
    pageIndex = 1;
    void loadJobs(0);
  }

  function submitSearch(): void {
    reloadFromFirstPage();
  }

  function clearFilters(): void {
    searchText = '';
    facetFilter = {};
    activeDerived = null;
    activeSavedId = null;
    approvalFacet = undefined;
    reloadFromFirstPage();
  }

  function toggleStatusFacet(s: TaskStatus): void {
    const cur = facetFilter.statuses ?? [];
    const next = cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s];
    facetFilter = { ...facetFilter, statuses: next.length > 0 ? next : undefined };
    activeSavedId = null;
    reloadFromFirstPage();
  }

  function toggleApprovalFacet(): void {
    approvalFacet = approvalFacet === true ? undefined : true;
    activeSavedId = null;
    reloadFromFirstPage();
  }

  function applyDerivedChip(chip: DerivedChip | null): void {
    activeDerived = chip;
    activeSavedId = null;
    if (chip !== null) {
      facetFilter = { ...chip.filter };
    } else {
      facetFilter = {};
    }
    reloadFromFirstPage();
  }

  function nextPage(): void {
    const cur = listResp?.cursor;
    if (cur === undefined || !cur.next_page_token) return;
    cursorStack = [...cursorStack, cur];
    pageIndex += 1;
    void loadJobs(cursorStack.length - 1);
  }

  function prevPage(): void {
    if (pageIndex <= 1) return;
    cursorStack = cursorStack.slice(0, -1);
    pageIndex -= 1;
    void loadJobs(cursorStack.length - 1);
  }

  function onPageRequest(requested: number): void {
    if (requested > pageIndex) {
      nextPage();
    } else if (requested < pageIndex) {
      prevPage();
    }
  }

  function changePageSize(size: number): void {
    pageSize = size;
    reloadFromFirstPage();
  }

  /* ================================================================ */
  /* Selection + detail                                                */
  /* ================================================================ */

  async function selectJob(id: string): Promise<void> {
    selectedId = id;
    await loadDetail(id);
  }

  /* ================================================================ */
  /* Control verbs — the SHIPPED Phase 54 surface (§13 / D-128)        */
  /* ================================================================ */

  // Bulk control over the selected rows — one Phase 54 verb invocation
  // PER selected row. There is NO bulk endpoint (a single-call bulk
  // method would be a §13 parallel implementation; D-128 reaffirms
  // this). Partial completion is rendered inline (per-row pass/fail),
  // never a silent batch abort (§13).
  async function bulkVerb(verb: 'cancel' | 'pause' | 'resume'): Promise<void> {
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
    await loadJobs(cursorStack.length - 1);
  }

  async function bulkPrioritize(priority: number): Promise<void> {
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
        // Task-level priority only (D-072) — never session-level (D-065).
        await client.control.prioritize(id, priority);
        ok += 1;
      } catch {
        failed += 1;
      }
    }
    bulkResult = {
      ok: failed === 0,
      message: `Bulk prioritize ${priority}: ${ok} succeeded, ${failed} failed (of ${ids.length}).`
    };
    selected = new Set<string>();
    bulkPending = false;
    await loadJobs(cursorStack.length - 1);
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
    facetFilter = { ...spec };
    searchText = spec.search ?? '';
    activeDerived = null;
    activeSavedId = id;
    reloadFromFirstPage();
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
    const created = await savedFilters.create(name, { ...facetFilter });
    saveName = '';
    await refreshSavedViews();
    activeSavedId = created.id;
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
        savedFilters = new BackgroundJobsSavedFilters(db, operator);
        await refreshSavedViews();
      } catch {
        savedFilters = null;
      }
    })();

    void loadJobs(0);
  });
</script>

<svelte:head>
  <title>Background Jobs · Harbor Console</title>
</svelte:head>

<div class="page" data-testid="background-jobs-page">
  <PageHeader
    title="Background Jobs"
    subtitle="Planner-spawned background tasks · queue + bulk control"
  />

  <FilterBar>
    {#snippet saved()}
      <SavedFilterChips
        activeDerivedId={activeDerived?.id ?? null}
        {savedViews}
        {activeSavedId}
        onderived={applyDerivedChip}
        onsaved={applySavedView}
        ondeletesaved={(id) => void deleteSavedView(id)}
      />
      <input
        class="control save-input"
        type="text"
        placeholder="Save current as…"
        bind:value={saveName}
        data-testid="bg-save-filter-name"
        disabled={savedFilters === null || disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onkeydown={(e) => e.key === 'Enter' && void saveCurrentFilter()}
      />
      <button
        type="button"
        class="control"
        data-testid="bg-save-filter"
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
      <div class="facet-chips" data-testid="bg-facets">
        {#each STATUS_FACETS as s (s)}
          <button
            type="button"
            class="chip"
            class:on={(facetFilter.statuses ?? []).includes(s)}
            data-testid={`bg-facet-status-${s}`}
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onclick={() => toggleStatusFacet(s)}
          >
            {s}
          </button>
        {/each}
        <button
          type="button"
          class="chip"
          class:on={approvalFacet === true}
          data-testid="bg-facet-approval"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={toggleApprovalFacet}
        >
          Has pending approval
        </button>
      </div>
    {/snippet}

    {#snippet search()}
      <input
        class="control search-input"
        type="search"
        placeholder="Search this queue…"
        bind:value={searchText}
        data-testid="bg-search"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onkeydown={(e) => e.key === 'Enter' && submitSearch()}
      />
      <button
        type="button"
        class="control"
        data-testid="bg-search-apply"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={submitSearch}
      >
        Apply
      </button>
      <button
        type="button"
        class="control"
        data-testid="bg-filter-clear"
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
        data-testid="bg-refresh"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={() => void loadJobs(cursorStack.length - 1)}
      >
        Refresh
      </button>
    {/snippet}
  </FilterBar>

  <BulkActionBar count={selected.size} onclear={() => (selected = new Set<string>())}>
    {#snippet actions()}
      <BulkToolbar
        {canControl}
        pending={bulkPending}
        onverb={(verb) => void bulkVerb(verb)}
        onprioritize={(p) => void bulkPrioritize(p)}
      />
    {/snippet}
  </BulkActionBar>
  {#if bulkResult !== null}
    <p
      class="inline-result"
      class:ok={bulkResult.ok}
      class:err={!bulkResult.ok}
      data-testid="bg-bulk-result"
    >
      {bulkResult.message}
    </p>
  {/if}

  <div class="layout">
    <div class="main-col">
      <PageState
        status={status}
        error={pageError}
        onretry={() => void loadJobs(cursorStack.length - 1)}
      >
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each [0, 1, 2, 3, 4] as i (i)}
              <span class="skeleton-row"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="bg-empty">
            <p class="headline">No background jobs running</p>
            <p class="detail">
              No background jobs match the current view. Background jobs are
              in-process at V1 (D-006) — a runtime restart clears the queue.
            </p>
            <a class="control" href="/tasks">View all tasks</a>
          </div>
        {/snippet}

        <QueueTable
          {rows}
          {selected}
          activeId={selectedId}
          {orphans}
          onselect={(id) => void selectJob(id)}
          onselectionchange={(s) => (selected = s)}
        />
      </PageState>

      {#if status === 'ready' || status === 'empty'}
        <Pagination
          page={pageIndex}
          pageSize={pageSize}
          total={totalRows()}
          onpage={onPageRequest}
          onpagesize={changePageSize}
        />
      {/if}
    </div>

    <DetailRail>
      <RailCard title="Selected job">
        <RightRail
          {detail}
          loading={detailLoading}
          {siblings}
          {siblingsLoading}
        />
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
    text-decoration: none;
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

  .table-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--space-8);
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
    max-width: var(--size-modal-width);
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
