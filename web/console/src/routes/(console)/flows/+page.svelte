<script lang="ts">
  // Harbor Console — Flows catalog page (`/flows`), Phase 73i / D-117,
  // refactored onto the design-system foundation (D-121).
  //
  // The list-mode view: a searchable, sortable catalog of registered
  // graph-family flows (`flows.list`) + the Flow Metrics card for the
  // selected flow in a `DetailRail`. Selecting a flow row navigates to
  // the detail route `/flows/<flow_id>`. The page is VIEW-ONLY (D-063)
  // — the only mutating action is `Run flow`, gated on the `flows.run`
  // scope claim (D-066).
  //
  // # Console consistency (CONVENTIONS.md)
  //
  // - Routes under `(console)/` with no `/console/` URL prefix (§1).
  // - Renders inside the app shell — sidebar, breadcrumb, footer (§2).
  // - Composes the `components/ui/` inventory: `PageHeader`, `FilterBar`,
  //   `SavedViewChips`, `DataTable`, `DetailRail`/`RailCard`, `Pagination`,
  //   `StatusChip` (§3). The Flows-specific `FlowMetricsCard` /
  //   `RunFlowModal` stay in `components/flows/` (§3).
  // - Routes all async state through the four-state `<PageState>` (§4),
  //   including the new Empty state.
  // - Talks to the Runtime only through `HarborClient` + `connection.ts`
  //   (§6) — no hand-rolled `fetch`.
  // - Design tokens only (§7).
  import { goto } from '$app/navigation';
  import { HarborClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import { FlowsProtocol } from '$lib/protocol/flows.js';
  import { resolveConnection, hasScope } from '$lib/connection.js';
  import {
    PageHeader,
    FilterBar,
    SavedViewChips,
    DataTable,
    DetailRail,
    RailCard,
    StatusChip,
    Pagination,
    PageState,
    type DataTableColumn,
    type PageStatus,
    type SavedView
  } from '$lib/components/ui/index.js';
  import FlowMetricsCard from '$lib/components/flows/FlowMetricsCard.svelte';
  import RunFlowModal from '$lib/components/flows/RunFlowModal.svelte';
  import {
    formatCost,
    formatDurationMS,
    formatRate,
    formatRelative
  } from '$lib/flows/format.js';
  import type { Flow, FlowFilter, FlowMetrics } from '$lib/flows/types.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import {
    FlowsSavedFilters,
    type FlowsSavedFilter
  } from '$lib/db/saved_filters_flows.js';

  // ---- Connection + typed client (CONVENTIONS.md §6) ----
  const connection = resolveConnection();
  const flowsClient =
    connection !== null
      ? new FlowsProtocol(new HarborClient({ connection }))
      : null;
  const canRun = hasScope(connection, 'admin');

  // ---- Async-state model (CONVENTIONS.md §4) ----
  let status = $state<PageStatus>(connection === null ? 'disconnected' : 'loading');
  let loadError = $state<ProtocolError | null>(null);

  // ---- Catalog data ----
  let flows = $state<Flow[]>([]);
  let totalRows = $state(0);
  let pageNum = $state(1);
  let pageSize = $state(50);

  // ---- Filter state ----
  let query = $state('');

  // ---- Selection + metrics rail ----
  let selectedID = $state<string | null>(null);
  let metrics = $state<FlowMetrics | null>(null);
  let railStatus = $state<PageStatus>('empty');
  let railError = $state<ProtocolError | null>(null);

  // ---- Saved views (Console-DB-backed — D-061) ----
  let savedFilters = $state<FlowsSavedFilter[]>([]);
  let activeViewID = $state<string | null>(null);
  let savedStore = $state<FlowsSavedFilters | null>(null);

  // ---- Run-flow modal ----
  let runFlowID = $state<string | null>(null);
  let runPending = $state(false);
  let runError = $state<string | null>(null);

  /** The `FlowFilter` assembled from the live filter controls. */
  function currentFilter(): FlowFilter {
    const f: FlowFilter = {};
    const q = query.trim();
    if (q.length > 0) {
      f.query = q;
    }
    return f;
  }

  /** Loads the flow catalog for the current filter + page. */
  async function loadCatalog(): Promise<void> {
    if (!flowsClient) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    loadError = null;
    try {
      const resp = await flowsClient.list({
        filter: currentFilter(),
        page: pageNum,
        page_size: pageSize
      });
      flows = resp.flows;
      totalRows = resp.total_rows;
      status = resp.flows.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      flows = [];
      totalRows = 0;
      loadError =
        err instanceof ProtocolError
          ? err
          : new ProtocolError('runtime_error', String(err), 0);
      status = 'error';
    }
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
      savedStore = new FlowsSavedFilters(db, operatorID);
      savedFilters = await savedStore.list();
    } catch {
      // A Console-DB open failure is non-fatal for the catalog view —
      // the saved-view chips simply render empty. The catalog itself
      // (the Protocol surface) is unaffected; this is Console-local
      // state only (D-061).
      savedFilters = [];
    }
  }

  /** Loads `flows.metrics` for a selected flow into the detail rail. */
  async function selectFlow(id: string): Promise<void> {
    selectedID = id;
    if (!flowsClient) {
      return;
    }
    railStatus = 'loading';
    railError = null;
    try {
      metrics = await flowsClient.metrics({ flow_id: id });
      railStatus = 'ready';
    } catch (err) {
      metrics = null;
      railError =
        err instanceof ProtocolError
          ? err
          : new ProtocolError('runtime_error', String(err), 0);
      railStatus = 'error';
    }
  }

  /** Persists the current filter as a named saved view (Console DB). */
  async function saveCurrentView(): Promise<void> {
    if (!savedStore) {
      return;
    }
    const name = (query.trim() || 'All flows').slice(0, 60);
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
    query = view.filterSpec.query ?? '';
    pageNum = 1;
    void loadCatalog();
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

  /** Submits a `flows.run` invocation from the runner modal. */
  async function submitRun(inputs: Record<string, unknown>): Promise<void> {
    if (!flowsClient || !runFlowID) {
      return;
    }
    runPending = true;
    runError = null;
    try {
      await flowsClient.run({ flow_id: runFlowID, inputs });
      runFlowID = null;
    } catch (err) {
      runError =
        err instanceof ProtocolError
          ? `${err.code}: ${err.message}`
          : 'Failed to start the flow run.';
    } finally {
      runPending = false;
    }
  }

  function applySearch(): void {
    activeViewID = null;
    pageNum = 1;
    void loadCatalog();
  }

  // ---- DataTable column config (CONVENTIONS.md §3) ----
  const columns: DataTableColumn[] = [
    { key: 'name', label: 'Flow' },
    { key: 'owner', label: 'Owner' },
    { key: 'version', label: 'Version' },
    { key: 'runs_24h', label: 'Runs (24h)', numeric: true },
    { key: 'latency', label: 'p50 / p95' },
    { key: 'success', label: 'Success', numeric: true },
    { key: 'last_run', label: 'Last run' },
    { key: 'budget', label: 'Budget' },
    { key: 'actions', label: '' }
  ];

  function flowKey(row: unknown): string {
    return (row as Flow).id;
  }

  function successKind(rate: number, runs: number): 'success' | 'warning' | 'danger' | 'neutral' {
    if (runs === 0) {
      return 'neutral';
    }
    if (rate >= 0.95) {
      return 'success';
    }
    if (rate >= 0.7) {
      return 'warning';
    }
    return 'danger';
  }

  $effect(() => {
    void loadCatalog();
    void loadSavedFilters();
  });
</script>

<svelte:head>
  <title>Flows · Harbor Console</title>
</svelte:head>

<section class="flows-page" data-testid="flows-page">
  <PageHeader title="Flows" subtitle="Registered engine-graph flows — view-only (D-063).">
    {#snippet actions()}
      <button
        type="button"
        class="ghost"
        data-testid="flows-refresh"
        onclick={() => void loadCatalog()}
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
        data-testid="flows-save-view"
        disabled={savedStore === null}
        title={savedStore === null
          ? 'Connect to a Runtime to save views'
          : 'Save the current filter as a view'}
        onclick={() => void saveCurrentView()}
      >
        Save snapshot
      </button>
    {/snippet}
    {#snippet search()}
      <input
        type="search"
        placeholder="Search flows…"
        data-testid="flows-search"
        bind:value={query}
        onkeydown={(e) => {
          if (e.key === 'Enter') {
            applySearch();
          }
        }}
      />
      <button type="button" class="ghost small" data-testid="flows-search-apply" onclick={applySearch}>
        Apply
      </button>
    {/snippet}
  </FilterBar>

  <PageState {status} error={loadError} onretry={() => void loadCatalog()}>
    {#snippet empty()}
      <p class="headline">No flows registered</p>
      <p class="detail">
        Flows are defined in agents whose planner is Graph, Workflow, or
        Deterministic. Register a graph-family agent, then refresh.
      </p>
      <button
        type="button"
        class="ghost"
        data-testid="flows-empty-refresh"
        onclick={() => void loadCatalog()}
      >
        Refresh
      </button>
    {/snippet}

    <div class="catalog-grid">
      <div class="catalog">
        <DataTable {columns} rows={flows} rowKey={flowKey} onrowclick={(r) => void goto(`/flows/${encodeURIComponent((r as Flow).id)}`)}>
          {#snippet row(r)}
            {@const flow = r as Flow}
            <td>
              <span class="flow-name" data-testid="catalog-row" data-flow-id={flow.id}>
                {flow.name}
              </span>
            </td>
            <td>{flow.owner ?? '—'}</td>
            <td class="mono">{flow.version ?? '—'}</td>
            <td class="numeric">{flow.runs_24h}</td>
            <td class="mono">
              {formatDurationMS(flow.p50_latency_ms)} / {formatDurationMS(flow.p95_latency_ms)}
            </td>
            <td class="numeric">
              <StatusChip
                kind={successKind(flow.success_rate, flow.runs_24h)}
                label={formatRate(flow.success_rate)}
              />
            </td>
            <td>{formatRelative(flow.last_run)}</td>
            <td class="mono">{formatCost(flow.budget.cost_cap_usd)} cap</td>
            <td class="actions-cell">
              <button
                type="button"
                class="run-btn"
                data-testid="catalog-run"
                disabled={!canRun}
                title={canRun
                  ? `Run ${flow.name}`
                  : 'Running a flow requires the flows.run scope claim'}
                onclick={(e) => {
                  e.stopPropagation();
                  runFlowID = flow.id;
                  runError = null;
                }}
              >
                Run flow ▶
              </button>
              <button
                type="button"
                class="ghost small"
                data-testid="catalog-metrics"
                onclick={(e) => {
                  e.stopPropagation();
                  void selectFlow(flow.id);
                }}
              >
                Metrics
              </button>
            </td>
          {/snippet}
        </DataTable>

        <Pagination
          page={pageNum}
          {pageSize}
          total={totalRows}
          onpage={(p) => {
            pageNum = p;
            void loadCatalog();
          }}
          onpagesize={(s) => {
            pageSize = s;
            pageNum = 1;
            void loadCatalog();
          }}
        />
      </div>

      <DetailRail>
        <RailCard title="Flow metrics">
          <PageState status={railStatus} error={railError} onretry={() => selectedID && void selectFlow(selectedID)}>
            {#snippet empty()}
              <p class="detail" data-testid="rail-metrics-empty">
                Select a flow's <strong>Metrics</strong> to preview its
                sparkline aggregates here.
              </p>
            {/snippet}
            <FlowMetricsCard {metrics} />
          </PageState>
        </RailCard>
      </DetailRail>
    </div>
  </PageState>
</section>

<RunFlowModal
  flowID={runFlowID ?? ''}
  open={runFlowID !== null}
  pending={runPending}
  errorMessage={runError}
  onsubmit={(inputs) => void submitRun(inputs)}
  oncancel={() => {
    runFlowID = null;
    runError = null;
  }}
/>

<style>
  .flows-page {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .catalog-grid {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    align-items: start;
  }

  .catalog {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-width: 0;
  }

  .flow-name {
    color: var(--color-accent);
    font-weight: 600;
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

  .actions-cell {
    display: flex;
    gap: var(--space-2);
    justify-content: flex-end;
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

  .run-btn {
    background: var(--color-accent);
    color: var(--color-bg);
    border: none;
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .run-btn:disabled {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    cursor: not-allowed;
  }
</style>
