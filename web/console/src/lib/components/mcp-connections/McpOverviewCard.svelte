<script lang="ts">
  // Harbor Console — MCP Connections idle catalog-overview card
  // (Phase 108m / D-185). The right-rail idle state shown when no server row
  // is selected: a per-state roll-up of the loaded catalog + a hint to drill
  // in. Mirrors the Tools-108k `ToolOverviewCard`. The counts are a pure
  // projection of the loaded `mcp.servers.list` rows — a disconnected page
  // passes the zeroed roll-up, never a fabricated total (CLAUDE.md §13).
  import type { ServerStateCounts } from '$lib/mcp-connections/derive.js';

  let {
    counts,
    disconnected = false
  }: {
    counts: ServerStateCounts;
    disconnected?: boolean;
  } = $props();

  const ROWS: { key: keyof ServerStateCounts; label: string }[] = [
    { key: 'online', label: 'Online' },
    { key: 'reconnecting', label: 'Reconnecting' },
    { key: 'auth_pending', label: 'Auth pending' },
    { key: 'offline', label: 'Offline' },
    { key: 'error', label: 'Errored' }
  ];

  /** A disconnected page shows `—` placeholders, not fabricated zeros. */
  function cell(n: number): string {
    return disconnected ? '—' : String(n);
  }
</script>

<div class="overview" data-testid="mcp-overview">
  <div class="total-row">
    <span class="total-num" data-testid="mcp-overview-total">{cell(counts.total)}</span>
    <span class="total-label">servers configured</span>
  </div>
  <dl class="state-grid">
    {#each ROWS as r (r.key)}
      <dt>{r.label}</dt>
      <dd data-testid={`mcp-overview-${r.key}`}>{cell(counts[r.key])}</dd>
    {/each}
  </dl>
</div>

<style>
  .overview {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .total-row {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .total-num {
    font-size: var(--text-2xl);
    font-weight: 600;
    color: var(--color-text);
  }

  .total-label {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .state-grid {
    display: grid;
    grid-template-columns: 1fr auto;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
  }

  .state-grid dt {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .state-grid dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
    text-align: right;
    font-variant-numeric: tabular-nums;
  }
</style>
