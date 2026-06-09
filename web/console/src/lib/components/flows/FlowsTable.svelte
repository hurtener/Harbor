<script lang="ts">
  // Harbor Console — Flows catalog table (Phase 108p / D-188).
  //
  // A typed wrapper over the shared `ui/DataTable` primitive — it does NOT fork
  // a table (CONVENTIONS.md §3). It renders the registered-flow catalog in the
  // page-mock §12 column order: Flow / Owner / Version / Runs (24h) / p50·p95 /
  // Success / Last run / Budget / Actions. A row-click opens the detail route
  // (mock-faithful — page-flows.md §6); the per-row Metrics action loads the
  // list rail's `flows.metrics`; `Run flow ▶` is the real admin `flows.run`
  // (disabled-with-tooltip without the admin claim, D-066 / D-079). Page-specific
  // — lives in `components/flows/`. Svelte 5 runes (D-092).
  import DataTable, { type DataTableColumn } from '$lib/components/ui/DataTable.svelte';
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import {
    formatCost,
    formatDurationMS,
    formatRate,
    formatRelative,
    successKind
  } from '$lib/flows/derive.js';
  import type { Flow } from '$lib/flows/types.js';

  let {
    flows,
    canRun,
    selectedId,
    onopen,
    onmetrics,
    onrun
  }: {
    flows: Flow[];
    /** True when the connection carries the admin claim (D-079). */
    canRun: boolean;
    /** The flow whose metrics fill the rail (drives the row-active highlight). */
    selectedId: string | null;
    /** Opens the detail route for a flow id. */
    onopen: (id: string) => void;
    /** Loads `flows.metrics` for a flow id into the list rail. */
    onmetrics: (id: string) => void;
    /** Opens the inline runner for a flow id (real `flows.run`). */
    onrun: (id: string) => void;
  } = $props();

  const COLUMNS: DataTableColumn[] = [
    { key: 'name', label: 'Flow' },
    { key: 'owner', label: 'Owner' },
    { key: 'version', label: 'Version' },
    { key: 'runs_24h', label: 'Runs (24h)', numeric: true },
    { key: 'latency', label: 'p50 · p95' },
    { key: 'success', label: 'Success', numeric: true },
    { key: 'last_run', label: 'Last run' },
    { key: 'budget', label: 'Budget' },
    { key: 'actions', label: 'Actions' }
  ];

  const runGate = 'Running a flow requires the admin scope claim (flows.run — D-079).';

  function rowKey(r: unknown): string {
    return (r as Flow).id;
  }
</script>

<div class="flows-table">
  <DataTable columns={COLUMNS} rows={flows} {rowKey} onrowclick={(r) => onopen((r as Flow).id)}>
    {#snippet row(r)}
      {@const flow = r as Flow}
      <td class="cell-name" class:row-active={flow.id === selectedId}>
        <span class="flow-name" data-testid="catalog-row" data-flow-id={flow.id} title={flow.id}>
          {flow.name}
        </span>
      </td>
      <td class="cell-muted">{flow.owner ?? '—'}</td>
      <td class="mono">{flow.version ?? '—'}</td>
      <td class="cell-num">{flow.runs_24h}</td>
      <td class="mono">
        {formatDurationMS(flow.p50_latency_ms)} · {formatDurationMS(flow.p95_latency_ms)}
      </td>
      <td class="cell-num">
        <StatusChip
          kind={successKind(flow.success_rate, flow.runs_24h)}
          label={formatRate(flow.success_rate)}
        />
      </td>
      <td class="cell-muted" title={flow.last_run ?? ''}>{formatRelative(flow.last_run)}</td>
      <td class="mono">{formatCost(flow.budget.cost_cap_usd)} cap</td>
      <td>
        <div class="actions-cell">
          <button
            type="button"
            class="run-btn"
            data-testid="catalog-run"
            disabled={!canRun}
            title={canRun ? `Run ${flow.name}` : runGate}
            onclick={(e) => {
              e.stopPropagation();
              onrun(flow.id);
            }}
          >
            Run flow ▶
          </button>
          <button
            type="button"
            class="metrics-btn"
            data-testid="catalog-metrics"
            title="Load this flow's metrics in the rail"
            onclick={(e) => {
              e.stopPropagation();
              onmetrics(flow.id);
            }}
          >
            Metrics
          </button>
        </div>
      </td>
    {/snippet}
  </DataTable>
</div>

<style>
  .flows-table :global(.data-table thead th) {
    padding: var(--space-2) var(--space-2);
  }

  td {
    padding: var(--space-2) var(--space-2);
    vertical-align: middle;
  }

  .cell-name {
    max-width: var(--size-rail);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .flow-name {
    color: var(--color-accent);
    font-weight: 600;
    font-size: var(--text-sm);
  }

  .row-active .flow-name {
    text-decoration: underline;
  }

  .cell-muted {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    white-space: nowrap;
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    white-space: nowrap;
  }

  .cell-num {
    text-align: right;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .actions-cell {
    display: flex;
    gap: var(--space-2);
    justify-content: flex-end;
  }

  .run-btn {
    background: var(--color-accent);
    color: var(--color-bg);
    border: none;
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
    white-space: nowrap;
  }

  .run-btn:disabled {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    cursor: not-allowed;
  }

  .metrics-btn {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }
</style>
