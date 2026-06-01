<script lang="ts">
  // Harbor Console — Sessions list page (`/sessions`) — Phase 108g rebuild
  // (D-179; supersedes the Phase 73c / D-122 pre-chrome composition).
  //
  // The investigative catalog of every session the operator can access
  // (`sessions.list`, cursor-paged). Rebuilt to the carded `.panel.card`
  // vocabulary the four done pages set (Overview 108c, Settings 108f),
  // with a calm toolbar (search + Status / Tenant facets + Sort +
  // Refresh) over the table. Selecting a row navigates to the detail
  // route `/sessions/<id>`.
  //
  // Every datum + action is real-wired (PAGE-POLISH §3):
  //   - rows           ← `sessions.list` (cursor-paged, registry projection)
  //   - Events column  ← `events.aggregate` enriched per visible row (D-122:
  //                       the registry doesn't model counts; the Console folds)
  //   - bulk Cancel /  ← the shipped `cancel` / `pause` control verbs (D-047),
  //     Pause            iterated per selected session's active run resolved
  //                       via `tasks.list`. Control-scope gated (D-066) — never
  //                       a disabled placeholder (the D-122 cruft this removes).
  //   - saved filters  ← Console DB (local, D-061)
  //   - search         ← the `sessions.list` query field (→ `search.sessions`)
  //
  // NO Cost column — per-session cost has no shipped aggregate wire
  // (`events.aggregate` counts by type only); cost is surfaced in the
  // detail's Cost History tab where it can be summed honestly (D-179).
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient +
  // connection.ts only (CONVENTIONS.md §6).
  import { goto } from '$app/navigation';
  import { HarborClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import { SessionsProtocol } from '$lib/protocol/sessions.js';
  import { resolveConnection, hasScope, DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import { DataTable, StatusChip, Pagination, PageState, SavedViewChips, type DataTableColumn, type PageStatus, type SavedView } from '$lib/components/ui/index.js';
  import SessionFacetChips from '$lib/components/sessions/SessionFacetChips.svelte';
  import IdentityCell from '$lib/components/sessions/IdentityCell.svelte';
  import { formatDurationNS, formatRelative, shortSessionID, statusKind } from '$lib/sessions/format.js';
  import type { SessionFilter, SessionRow, SessionSort } from '$lib/sessions/types.js';
  import type { TaskRow } from '$lib/protocol/tasks.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import { SessionsSavedFilters, type SessionsSavedFilter } from '$lib/db/saved_filters_sessions.js';

  // ---- Connection + typed client (CONVENTIONS.md §6) ----
  const connection = resolveConnection();
  const harborClient = connection !== null ? new HarborClient({ connection }) : null;
  const sessionsClient = harborClient !== null ? new SessionsProtocol(harborClient) : null;
  // The tenant facet renders only with the admin scope claim (D-079).
  const adminScoped = hasScope(connection, 'admin');
  // D-066 — bulk Cancel / Pause are control-plane verbs gated on the
  // control scope. The runtime re-checks server-side regardless.
  const canControl = hasScope(connection, 'admin');
  const disconnected = connection === null;

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

  // ---- Per-row enrichment (D-122: the registry projects no aggregates) ----
  // sessionId → events count (from `events.aggregate`) and active
  // duration in ms (Σ of the session's run `duration_ms` from a
  // session-scoped `tasks.list`). Folded best-effort over the VISIBLE
  // page only (bounded; never a global scan — D-026). The duration is
  // ACTIVE processing time (sum of per-run durations), not wall-clock
  // from open to now — mirrors the Playground's `activeWorkMs`.
  let eventCounts = $state<Map<string, number>>(new Map());
  let durations = $state<Map<string, number>>(new Map());

  // ---- Filter + sort state ----
  let filter = $state<SessionFilter>({});
  let query = $state('');
  let sort = $state<SessionSort>('started_desc');

  // ---- Bulk selection + action feedback ----
  let selected = $state<Set<string>>(new Set());
  let bulkBusy = $state(false);
  let bulkResult = $state<string | null>(null);

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
      const resp = await sessionsClient.list({ filter: currentFilter(), sort, cursor, limit: pageSize });
      rows = resp.rows;
      truncated = resp.truncated;
      nextCursor = resp.next_cursor;
      status = resp.rows.length === 0 ? 'empty' : 'ready';
      void enrichVisibleRows(resp.rows);
    } catch (err) {
      rows = [];
      truncated = false;
      nextCursor = '';
      loadError = err instanceof ProtocolError ? err : new ProtocolError('runtime_error', String(err), 0);
      status = 'error';
    }
  }

  /**
   * Folds a per-session events count + active duration into the visible
   * page. Per row, in parallel and applied as each resolves (non-blocking
   * — the table renders before the cells fill):
   *   - events count   ← `events.aggregate` (session_ids filter)
   *   - active duration ← Σ of the session's run `duration_ms` from a
   *                       session-scoped `tasks.list` (X-Harbor-Session is
   *                       the conversation id — D-171). This is ACTIVE
   *                       processing time, not wall-clock from open to now.
   * A failed leg leaves the row's value absent (the cell renders "—"),
   * never a fabricated zero (CLAUDE.md §13). Bounded to the visible page
   * (≤ pageSize) — never a global scan (D-026).
   */
  async function enrichVisibleRows(pageRows: SessionRow[]): Promise<void> {
    if (harborClient === null || connection === null) return;
    const thirtyDaysNS = 30 * 24 * 60 * 60 * 1_000_000_000;
    await Promise.all(
      pageRows.map(async (r) => {
        try {
          const resp = await harborClient.events.aggregate({
            filter: { session_ids: [r.session_id] },
            window: thirtyDaysNS,
            bucket: thirtyDaysNS
          });
          let total = 0;
          for (const b of resp.buckets ?? []) {
            for (const v of Object.values(b.counts ?? {})) {
              total += v ?? 0;
            }
          }
          eventCounts = new Map(eventCounts).set(r.session_id, total);
        } catch {
          // best-effort — leave the row's count absent ("—")
        }
        try {
          // `tasks.list` is session-scoped via the X-Harbor-Session header
          // (D-171) — a session-scoped client surfaces that session's runs.
          const scoped = new HarborClient({
            connection: { ...connection, identity: { ...connection.identity, session: r.session_id } }
          });
          const tr = await scoped.tasks.list<{ rows: TaskRow[] }>({});
          const activeMs = (tr.rows ?? []).reduce((sum, t) => sum + (t.duration_ms ?? 0), 0);
          durations = new Map(durations).set(r.session_id, activeMs);
        } catch {
          // best-effort — leave the row's active duration absent ("—")
        }
      })
    );
  }

  /** Re-runs the listing from page 1 (filter / sort / search changed). */
  function reload(): void {
    pageNum = 1;
    cursorStack = [];
    selected = new Set();
    bulkResult = null;
    eventCounts = new Map();
    durations = new Map();
    void loadCatalog('');
  }

  /** Advances to the next cursor page. */
  function nextPage(): void {
    if (!truncated || nextCursor === '') return;
    cursorStack = [...cursorStack, nextCursor];
    pageNum += 1;
    void loadCatalog(nextCursor);
  }

  /** Returns to the previous cursor page. */
  function prevPage(): void {
    if (pageNum <= 1) return;
    const stack = cursorStack.slice(0, -1);
    cursorStack = stack;
    pageNum -= 1;
    void loadCatalog(stack.length > 0 ? stack[stack.length - 1] : '');
  }

  /** Loads the saved-filter chips from the Console DB. */
  async function loadSavedFilters(): Promise<void> {
    try {
      if (connection === null) return;
      const db = await openListPageDB(connection);
      const operatorID = await operatorIdOf(connection.identity.tenant, connection.identity.user);
      savedStore = new SessionsSavedFilters(db, operatorID);
      savedFilters = await savedStore.list();
    } catch {
      // A Console-DB open failure is non-fatal — the saved-view chips
      // render empty; the catalog (the Protocol surface) is unaffected.
      savedFilters = [];
    }
  }

  /** Persists the current filter as a named saved view (Console DB). */
  async function saveCurrentView(): Promise<void> {
    if (!savedStore) return;
    const name = (query.trim() || 'Sessions view').slice(0, 60);
    await savedStore.create(name, currentFilter());
    savedFilters = await savedStore.list();
  }

  /** Applies a saved view's filter spec and reloads. */
  function applySavedView(id: string): void {
    const view = savedFilters.find((v) => v.id === id);
    if (!view) return;
    activeViewID = id;
    filter = { ...view.filterSpec };
    query = view.filterSpec.query ?? '';
    reload();
  }

  /** Deletes a saved view from the Console DB. */
  async function deleteSavedView(id: string): Promise<void> {
    if (!savedStore) return;
    await savedStore.delete(id);
    if (activeViewID === id) activeViewID = null;
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

  // ---- Bulk control (D-066 / D-047) -----------------------------------
  // Bulk Cancel / Pause iterate the SHIPPED per-run `cancel` / `pause`
  // control verbs (no bulk Protocol method — the wrapping is local UI,
  // D-072). A control verb targets a RUN (`identity.run`), not a session,
  // so we resolve each selected session's active run via `tasks.list`
  // and invoke the verb per run. The resulting state change is observed
  // on the reload that follows.
  function onSelectionChange(next: Set<string>): void {
    selected = next;
  }

  async function runBulkControl(verb: 'cancel' | 'pause'): Promise<void> {
    if (harborClient === null || selected.size === 0 || bulkBusy) return;
    bulkBusy = true;
    bulkResult = null;
    try {
      // Resolve the active runs for the selected sessions. `tasks.list`
      // returns the operator's task rows; we keep the live (running /
      // paused) ones whose session is selected.
      const resp = await harborClient.tasks.list<{ rows: TaskRow[] }>({});
      const targets = (resp.rows ?? []).filter(
        (t) => selected.has(t.identity?.session ?? t.parent_session_id) && (t.status === 'running' || t.status === 'paused')
      );
      let ok = 0;
      let fail = 0;
      for (const t of targets) {
        try {
          if (verb === 'cancel') await harborClient.control.cancel(t.id);
          else await harborClient.control.pause(t.id);
          ok += 1;
        } catch {
          fail += 1;
        }
      }
      bulkResult =
        targets.length === 0
          ? 'No active runs in the selected sessions.'
          : `${verb === 'cancel' ? 'Cancelled' : 'Paused'} ${ok} run${ok === 1 ? '' : 's'}${fail > 0 ? `, ${fail} failed` : ''}.`;
      selected = new Set();
      void loadCatalog(cursorStack[cursorStack.length - 1] ?? '');
    } catch (err) {
      bulkResult = err instanceof ProtocolError ? `${err.code}: ${err.message}` : String(err);
    } finally {
      bulkBusy = false;
    }
  }

  // ---- DataTable column config (lean — registry-owned + Events) -------
  const columns: DataTableColumn[] = [
    { key: 'session', label: 'Session' },
    { key: 'status', label: 'Status' },
    { key: 'agent', label: 'Agent' },
    { key: 'identity', label: 'Identity' },
    { key: 'started', label: 'Started' },
    { key: 'last_activity', label: 'Last activity' },
    { key: 'events', label: 'Events', numeric: true },
    { key: 'duration', label: 'Duration' }
  ];

  function rowKey(row: unknown): string {
    return (row as SessionRow).session_id;
  }

  function eventsLabel(id: string): string {
    const n = eventCounts.get(id);
    return n === undefined ? '—' : String(n);
  }

  /** Active processing time (Σ run durations), formatted; "—" until enriched. */
  function durationLabel(id: string): string {
    const ms = durations.get(id);
    return ms === undefined ? '—' : formatDurationNS(ms * 1_000_000);
  }

  // Pagination total estimate: rows loaded so far + a "(more)" hint when
  // truncated. sessions.list is cursor-paginated (no silent exact total —
  // D-026); the footer is per-page + a more-pages signal.
  const totalEstimate = $derived((pageNum - 1) * pageSize + rows.length + (truncated ? 1 : 0));

  $effect(() => {
    void loadCatalog('');
    void loadSavedFilters();
  });
</script>

<svelte:head>
  <title>Sessions · Harbor Console</title>
</svelte:head>

<section class="sessions-page" data-testid="sessions-page">
  <section class="panel card">
    <header class="card-head">
      <h2 class="panel-title">Sessions</h2>
      <div class="toolbar">
        <input
          type="search"
          class="search"
          placeholder="Search session id, agent, user…"
          data-testid="sessions-search"
          bind:value={query}
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onkeydown={(e) => {
            if (e.key === 'Enter') applySearch();
          }}
        />
        <label class="sort-label">
          Sort
          <select
            class="control"
            data-testid="sessions-sort"
            value={sort}
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onchange={(e) => changeSort((e.currentTarget as HTMLSelectElement).value as SessionSort)}
          >
            <option value="started_desc">Newest first</option>
            <option value="started_asc">Oldest first</option>
            <option value="last_activity_desc">Most recently active</option>
            <option value="cost_desc">Most expensive</option>
          </select>
        </label>
        <button
          type="button"
          class="control"
          data-testid="sessions-refresh"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => void loadCatalog(cursorStack[cursorStack.length - 1] ?? '')}
        >
          Refresh
        </button>
      </div>
    </header>

    <div class="facets-row">
      <SessionFacetChips {filter} {adminScoped} onchange={onFacetChange} />
    </div>

    <div class="saved-row">
      <SavedViewChips
        views={savedFilters as SavedView[]}
        activeId={activeViewID}
        onselect={applySavedView}
        ondelete={(id) => void deleteSavedView(id)}
      />
      <button
        type="button"
        class="control small"
        data-testid="sessions-save-view"
        disabled={savedStore === null || disconnected}
        title={disconnected
          ? DISCONNECTED_TOOLTIP
          : savedStore === null
            ? 'Connect to a Runtime to save views'
            : 'Save the current filter as a view'}
        onclick={() => void saveCurrentView()}
      >
        Save view
      </button>
    </div>

    {#if selected.size > 0}
      <div class="bulk-bar" data-testid="bulk-bar">
        <span class="bulk-count">{selected.size} selected</span>
        <button
          type="button"
          class="control small"
          data-testid="bulk-cancel"
          disabled={!canControl || bulkBusy}
          title={canControl ? 'Cancel the active run of each selected session' : 'Requires the control-plane scope claim (D-066)'}
          onclick={() => void runBulkControl('cancel')}
        >
          Cancel selected
        </button>
        <button
          type="button"
          class="control small"
          data-testid="bulk-pause"
          disabled={!canControl || bulkBusy}
          title={canControl ? 'Pause the active run of each selected session' : 'Requires the control-plane scope claim (D-066)'}
          onclick={() => void runBulkControl('pause')}
        >
          Pause selected
        </button>
        <button type="button" class="control small ghost" onclick={() => (selected = new Set())}>Clear</button>
        {#if bulkResult}
          <span class="bulk-result" data-testid="bulk-result">{bulkResult}</span>
        {/if}
      </div>
    {/if}

    <PageState {status} error={loadError} onretry={() => void loadCatalog('')}>
      {#snippet empty()}
        <p class="empty-headline">No sessions match these filters</p>
        <p class="empty-detail">Adjust or clear the filters above, or start your first session in Live Runtime.</p>
        <button
          type="button"
          class="control"
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
        <div class="table-scroll">
        <DataTable
          {columns}
          {rows}
          {rowKey}
          selectable
          {selected}
          onselectionchange={onSelectionChange}
          onrowclick={(r) => void goto(`/sessions/${encodeURIComponent((r as SessionRow).session_id)}`)}
        >
          {#snippet row(r)}
            {@const s = r as SessionRow}
            <td>
              <span class="session-id" data-testid="catalog-row" data-session-id={s.session_id} title={s.session_id}>
                {shortSessionID(s.session_id)}
              </span>
            </td>
            <td><StatusChip kind={statusKind(s.status)} label={s.status} /></td>
            <td>{s.agent_name || '—'}</td>
            <td><IdentityCell identity={s.identity} /></td>
            <td>{formatRelative(s.started_at)}</td>
            <td>{formatRelative(s.last_activity_at)}</td>
            <td class="numeric">{eventsLabel(s.session_id)}</td>
            <td title="Active processing time — sum of run durations, not wall-clock">{durationLabel(s.session_id)}</td>
          {/snippet}
        </DataTable>
        </div>

        <Pagination
          page={pageNum}
          {pageSize}
          total={totalEstimate}
          onpage={(p) => {
            if (p > pageNum) nextPage();
            else if (p < pageNum) prevPage();
          }}
          onpagesize={(sz) => {
            pageSize = sz;
            reload();
          }}
        />
      </div>
    </PageState>
  </section>
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls; only the table region scrolls internally (the
     50-row case stays inside the card — PAGE-POLISH §6). */
  .sessions-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    padding: var(--space-4);
    overflow: hidden;
  }

  /* The carded surface — same vocabulary as the Overview / Settings pages. */
  .card {
    display: flex;
    flex-direction: column;
    flex: 1;
    gap: var(--space-3);
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    min-width: 0;
    min-height: 0;
  }

  .panel-title {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .card-head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .toolbar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .facets-row,
  .saved-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .search {
    flex: 1 1 var(--size-search-min);
    min-width: var(--size-search-min);
    max-width: 100%;
    background: var(--color-bg);
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

  .control {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
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

  .catalog {
    display: flex;
    flex-direction: column;
    flex: 1;
    gap: var(--space-3);
    min-width: 0;
    min-height: 0;
  }

  /* The table scrolls inside this region; the sticky `<thead>` keeps the
     column headers pinned while the rows scroll (DataTable). */
  .table-scroll {
    flex: 1;
    min-height: 0;
    overflow: auto;
  }

  .session-id {
    color: var(--color-accent);
    font-weight: 600;
    font-family: var(--font-mono);
    font-size: var(--text-xs);
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
</style>
