<script lang="ts">
  // Harbor Console — Sessions catalog page (`/sessions`), Phase 73c /
  // D-122, built on the design-system foundation (D-121).
  //
  // The list-mode view: a filtered, cursor-paginated catalog of the
  // past-and-active sessions the operator can access (`sessions.list`).
  // Selecting a row navigates to the detail route `/sessions/<id>`.
  //
  // # Console consistency (CONVENTIONS.md)
  //
  // - Routes under `(console)/` with no `/console/` URL prefix (§1).
  //   Detail views at `(console)/sessions/[id]/`.
  // - Renders inside the app shell — sidebar, breadcrumb, footer (§2).
  // - Composes the `components/ui/` inventory: `PageHeader`, `FilterBar`,
  //   `SavedViewChips`, `DataTable`, `BulkActionBar`, `Pagination`,
  //   `StatusChip`, `PageState` (§3). Sessions-specific components
  //   (`SessionFacetChips`, `IdentityCell`) stay in `components/sessions/`.
  // - Routes all async state through the four-state `<PageState>` (§4).
  // - Talks to the Runtime only through `HarborClient` + `connection.ts`
  //   (§6) — no hand-rolled `fetch`.
  // - Design tokens only (§7).
  import { goto } from '$app/navigation';
  import { HarborClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import { SessionsProtocol } from '$lib/protocol/sessions.js';
  import { resolveConnection, hasScope } from '$lib/connection.js';
  import {
    PageHeader,
    FilterBar,
    SavedViewChips,
    DataTable,
    BulkActionBar,
    StatusChip,
    Pagination,
    PageState,
    type DataTableColumn,
    type PageStatus,
    type SavedView
  } from '$lib/components/ui/index.js';
  import SessionFacetChips from '$lib/components/sessions/SessionFacetChips.svelte';
  import IdentityCell from '$lib/components/sessions/IdentityCell.svelte';
  import {
    formatCostCents,
    formatDurationNS,
    formatRelative,
    shortSessionID,
    statusKind
  } from '$lib/sessions/format.js';
  import type {
    SessionFilter,
    SessionRow,
    SessionSort
  } from '$lib/sessions/types.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import {
    SessionsSavedFilters,
    type SessionsSavedFilter
  } from '$lib/db/saved_filters_sessions.js';

  // ---- Connection + typed client (CONVENTIONS.md §6) ----
  const connection = resolveConnection();
  const sessionsClient =
    connection !== null ? new SessionsProtocol(new HarborClient({ connection })) : null;
  // The tenant facet renders only with the admin scope claim (D-079).
  const adminScoped = hasScope(connection, 'admin');

  // ---- Async-state model (CONVENTIONS.md §4) ----
  let status = $state<PageStatus>(connection === null ? 'disconnected' : 'loading');
  let loadError = $state<ProtocolError | null>(null);

  // ---- Catalog data + cursor-stack pagination ----
  let rows = $state<SessionRow[]>([]);
  let truncated = $state(false);
  let nextCursor = $state('');
  // cursorStack[i] is the cursor that loads page i+2 (page 1 = empty).
  let cursorStack = $state<string[]>([]);
  let pageNum = $state(1);
  let pageSize = $state(50);

  // ---- Filter + sort state ----
  let filter = $state<SessionFilter>({});
  let query = $state('');
  let sort = $state<SessionSort>('started_desc');

  // ---- Bulk selection ----
  let selected = $state<Set<string>>(new Set());

  // ---- Saved views (Console-DB-backed — D-061) ----
  let savedFilters = $state<SessionsSavedFilter[]>([]);
  let activeViewID = $state<string | null>(null);
  let savedStore = $state<SessionsSavedFilters | null>(null);

  /** The `SessionFilter` assembled from the live filter controls. */
  function currentFilter(): SessionFilter {
    const f: SessionFilter = { ...filter };
    const q = query.trim();
    if (q.length > 0) {
      f.query = q;
    } else {
      delete f.query;
    }
    return f;
  }

  /** Loads `sessions.list` for the current filter / sort / page. */
  async function loadCatalog(cursor = ''): Promise<void> {
    if (!sessionsClient) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    loadError = null;
    try {
      const resp = await sessionsClient.list({
        filter: currentFilter(),
        sort,
        cursor,
        limit: pageSize
      });
      rows = resp.rows;
      truncated = resp.truncated;
      nextCursor = resp.next_cursor;
      status = resp.rows.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      rows = [];
      truncated = false;
      nextCursor = '';
      loadError =
        err instanceof ProtocolError
          ? err
          : new ProtocolError('runtime_error', String(err), 0);
      status = 'error';
    }
  }

  /** Re-runs the listing from page 1 (filter / sort / search changed). */
  function reload(): void {
    pageNum = 1;
    cursorStack = [];
    selected = new Set();
    void loadCatalog('');
  }

  /** Advances to the next cursor page. */
  function nextPage(): void {
    if (!truncated || nextCursor === '') {
      return;
    }
    cursorStack = [...cursorStack, nextCursor];
    pageNum += 1;
    void loadCatalog(nextCursor);
  }

  /** Returns to the previous cursor page. */
  function prevPage(): void {
    if (pageNum <= 1) {
      return;
    }
    const stack = cursorStack.slice(0, -1);
    cursorStack = stack;
    pageNum -= 1;
    void loadCatalog(stack.length > 0 ? stack[stack.length - 1] : '');
  }

  /** Loads the saved-filter chips from the Console DB. */
  async function loadSavedFilters(): Promise<void> {
    try {
      if (connection === null) {
        return;
      }
      const db = await openListPageDB(connection);
      const operatorID = await operatorIdOf(
        connection.identity.tenant,
        connection.identity.user
      );
      savedStore = new SessionsSavedFilters(db, operatorID);
      savedFilters = await savedStore.list();
    } catch {
      // A Console-DB open failure is non-fatal for the catalog view —
      // the saved-view chips render empty. The catalog itself (the
      // Protocol surface) is unaffected; Console-local state (D-061).
      savedFilters = [];
    }
  }

  /** Persists the current filter as a named saved view (Console DB). */
  async function saveCurrentView(): Promise<void> {
    if (!savedStore) {
      return;
    }
    const name = (query.trim() || 'Sessions view').slice(0, 60);
    await savedStore.create(name, currentFilter());
    savedFilters = await savedStore.list();
  }

  /** Applies a saved view's filter spec and reloads. */
  function applySavedView(id: string): void {
    const view = savedFilters.find((v) => v.id === id);
    if (!view) {
      return;
    }
    activeViewID = id;
    filter = { ...view.filterSpec };
    query = view.filterSpec.query ?? '';
    reload();
  }

  /** Deletes a saved view from the Console DB. */
  async function deleteSavedView(id: string): Promise<void> {
    if (!savedStore) {
      return;
    }
    await savedStore.delete(id);
    if (activeViewID === id) {
      activeViewID = null;
    }
    savedFilters = await savedStore.list();
  }

  function onFacetChange(next: SessionFilter): void {
    filter = next;
    activeViewID = null;
    reload();
  }

  function applySearch(): void {
    activeViewID = null;
    reload();
  }

  function changeSort(value: SessionSort): void {
    sort = value;
    reload();
  }

  // ---- Bulk actions ----
  // D-066: bulk Cancel / Pause require the control-plane scope claim.
  // The bulk wrapping is local UI iterating the per-row cancel / pause
  // methods (D-072) — no bulk Protocol method. V1 ships the toolbar +
  // the scope-gated affordances; the per-row control invocation lands
  // with the Live Runtime control surface (Phase 73b). Until then the
  // bulk actions render disabled-with-tooltip — never a faked success
  // string (CONVENTIONS.md §5 / §13).
  const canControl = hasScope(connection, 'admin');

  function onSelectionChange(next: Set<string>): void {
    selected = next;
  }

  // ---- DataTable column config (CONVENTIONS.md §3) ----
  const columns: DataTableColumn[] = [
    { key: 'session', label: 'Session' },
    { key: 'status', label: 'Status' },
    { key: 'agent', label: 'Agent' },
    { key: 'identity', label: 'Identity' },
    { key: 'started', label: 'Started' },
    { key: 'last_activity', label: 'Last activity' },
    { key: 'events', label: 'Events', numeric: true },
    { key: 'cost', label: 'Cost' }
  ];

  function rowKey(row: unknown): string {
    return (row as SessionRow).session_id;
  }

  // Pagination total estimate: rows loaded so far + a "(more)" hint
  // when truncated. sessions.list is cursor-paginated — D-026 says no
  // silent exact total — so the footer micro-counter is per-page +
  // a more-pages signal, never a global aggregate.
  const totalEstimate = $derived(
    (pageNum - 1) * pageSize + rows.length + (truncated ? 1 : 0)
  );

  $effect(() => {
    void loadCatalog('');
    void loadSavedFilters();
  });
</script>

<svelte:head>
  <title>Sessions · Harbor Console</title>
</svelte:head>

<section class="sessions-page" data-testid="sessions-page">
  <PageHeader
    title="Sessions"
    subtitle="The past-and-active record of every Harbor execution you can access."
  >
    {#snippet actions()}
      <label class="sort-label">
        Sort by
        <select
          class="sort-select"
          data-testid="sessions-sort"
          value={sort}
          onchange={(e) =>
            changeSort((e.currentTarget as HTMLSelectElement).value as SessionSort)}
        >
          <option value="started_desc">Newest first</option>
          <option value="started_asc">Oldest first</option>
          <option value="last_activity_desc">Most recently active</option>
          <option value="cost_desc">Most expensive</option>
        </select>
      </label>
      <button
        type="button"
        class="ghost"
        data-testid="sessions-refresh"
        onclick={() => void loadCatalog(cursorStack[cursorStack.length - 1] ?? '')}
      >
        Refresh
      </button>
    {/snippet}
  </PageHeader>

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedFilters as SavedView[]}
        activeId={activeViewID}
        onselect={applySavedView}
        ondelete={(id) => void deleteSavedView(id)}
      />
      <button
        type="button"
        class="ghost small"
        data-testid="sessions-save-view"
        disabled={savedStore === null}
        title={savedStore === null
          ? 'Connect to a Runtime to save filters'
          : 'Save the current filter as a view'}
        onclick={() => void saveCurrentView()}
      >
        Save filter
      </button>
    {/snippet}
    {#snippet facets()}
      <SessionFacetChips {filter} {adminScoped} onchange={onFacetChange} />
    {/snippet}
    {#snippet search()}
      <input
        type="search"
        placeholder="Search session id, agent, user…"
        data-testid="sessions-search"
        bind:value={query}
        onkeydown={(e) => {
          if (e.key === 'Enter') {
            applySearch();
          }
        }}
      />
      <button
        type="button"
        class="ghost small"
        data-testid="sessions-search-apply"
        onclick={applySearch}
      >
        Search
      </button>
    {/snippet}
  </FilterBar>

  <BulkActionBar count={selected.size} onclear={() => (selected = new Set())}>
    {#snippet actions()}
      <button
        type="button"
        class="ghost small"
        data-testid="bulk-cancel"
        disabled
        title={canControl
          ? 'Bulk cancel iterates the per-row cancel method — wired with the Live Runtime control surface (Phase 73b)'
          : 'Bulk cancel requires the control-plane scope claim (D-066)'}
      >
        Cancel selected
      </button>
      <button
        type="button"
        class="ghost small"
        data-testid="bulk-pause"
        disabled
        title={canControl
          ? 'Bulk pause iterates the per-row pause method — wired with the Live Runtime control surface (Phase 73b)'
          : 'Bulk pause requires the control-plane scope claim (D-066)'}
      >
        Pause selected
      </button>
    {/snippet}
  </BulkActionBar>

  <PageState {status} error={loadError} onretry={() => void loadCatalog('')}>
    {#snippet empty()}
      <p class="headline">No sessions match these filters</p>
      <p class="detail">
        Adjust or clear the filters above, or start your first session in Live
        Runtime.
      </p>
      <button
        type="button"
        class="ghost"
        data-testid="sessions-empty-clear"
        onclick={() => {
          filter = {};
          query = '';
          reload();
        }}
      >
        Clear filters
      </button>
    {/snippet}

    <div class="catalog">
      <DataTable
        {columns}
        {rows}
        {rowKey}
        selectable
        {selected}
        onselectionchange={onSelectionChange}
        onrowclick={(r) =>
          void goto(`/sessions/${encodeURIComponent((r as SessionRow).session_id)}`)}
      >
        {#snippet row(r)}
          {@const s = r as SessionRow}
          <td>
            <span
              class="session-id"
              data-testid="catalog-row"
              data-session-id={s.session_id}
              title={s.session_id}
            >
              {shortSessionID(s.session_id)}
            </span>
          </td>
          <td>
            <StatusChip kind={statusKind(s.status)} label={s.status} />
          </td>
          <td>{s.agent_name || '—'}</td>
          <td><IdentityCell identity={s.identity} /></td>
          <td>{formatRelative(s.started_at)}</td>
          <td>{formatRelative(s.last_activity_at)} · {formatDurationNS(s.duration)}</td>
          <td class="numeric">{s.events_count}</td>
          <td class="mono">{formatCostCents(s.total_cost_cents)}</td>
        {/snippet}
      </DataTable>

      <Pagination
        page={pageNum}
        {pageSize}
        total={totalEstimate}
        onpage={(p) => {
          if (p > pageNum) {
            nextPage();
          } else if (p < pageNum) {
            prevPage();
          }
        }}
        onpagesize={(sz) => {
          pageSize = sz;
          reload();
        }}
      />
    </div>
  </PageState>
</section>

<style>
  .sessions-page {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .catalog {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-width: 0;
  }

  .session-id {
    color: var(--color-accent);
    font-weight: 600;
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .numeric {
    text-align: right;
  }

  td {
    padding: var(--space-2) var(--space-3);
  }

  .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  input[type='search'] {
    width: 100%;
    background: var(--color-surface);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
  }

  .sort-label {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .sort-select {
    background: var(--color-surface);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
  }

  .ghost {
    background: none;
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .ghost.small {
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
  }

  .ghost:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
</style>
