<script lang="ts">
  // CatalogTable — the Tools-page primary surface (page-tools.md §12).
  // Renders the catalog rows in mockup column order: Name / Version /
  // Scope / Transport / OAuth status / Approval policy / Reliability
  // tier / Last used / Owner. Row click selects a tool; the parent
  // opens the detail panel. Svelte 5 runes mode (D-092).
  import type { Tool } from '$lib/protocol/tools.js';

  let {
    tools,
    selectedId = null,
    loading = false,
    onselect
  }: {
    tools: Tool[];
    selectedId?: string | null;
    loading?: boolean;
    onselect: (id: string) => void;
  } = $props();

  // Fixed skeleton-row indices rendered while a catalog load is in
  // flight (loading state).
  const SKELETON_ROWS = [0, 1, 2, 3, 4];

  // Relative-time formatting for the Last-used column. A zero-value
  // timestamp (the Go time.Time zero) means "never used".
  function lastUsed(iso: string): string {
    if (!iso || iso.startsWith('0001-01-01')) {
      return 'never';
    }
    const then = new Date(iso).getTime();
    if (Number.isNaN(then)) {
      return 'never';
    }
    const deltaMin = Math.round((Date.now() - then) / 60000);
    if (deltaMin < 1) return 'just now';
    if (deltaMin < 60) return `${deltaMin}m ago`;
    const deltaHr = Math.round(deltaMin / 60);
    if (deltaHr < 24) return `${deltaHr}h ago`;
    return `${Math.round(deltaHr / 24)}d ago`;
  }
</script>

<div class="catalog" data-testid="tools-catalog-table">
  <table>
    <thead>
      <tr>
        <th>Name</th>
        <th>Version</th>
        <th>Scope</th>
        <th>Transport</th>
        <th>OAuth</th>
        <th>Approval</th>
        <th>Reliability</th>
        <th>Last used</th>
        <th>Owner</th>
      </tr>
    </thead>
    <tbody>
      {#if loading}
        {#each SKELETON_ROWS as i (i)}
          <tr class="skeleton-row" aria-hidden="true">
            <td colspan="9"><span class="skeleton"></span></td>
          </tr>
        {/each}
      {:else if tools.length === 0}
        <tr>
          <td colspan="9" class="empty" data-testid="tools-catalog-empty">
            No tools match these filters.
          </td>
        </tr>
      {:else}
        {#each tools as tool (tool.id)}
          <tr
            class:selected={tool.id === selectedId}
            data-testid="tools-catalog-row"
            data-tool-id={tool.id}
            tabindex="0"
            onclick={() => onselect(tool.id)}
            onkeydown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                onselect(tool.id);
              }
            }}
          >
            <td class="name">{tool.name}</td>
            <td class="muted">{tool.version || '—'}</td>
            <td><span class="chip">{tool.scope}</span></td>
            <td><span class="chip chip-transport">{tool.transport}</span></td>
            <td>
              <span
                class="chip"
                class:chip-warn={tool.oauth_status === 'Required' ||
                  tool.oauth_status === 'Expired'}
                class:chip-ok={tool.oauth_status === 'Bound'}
              >
                {tool.oauth_status}
              </span>
            </td>
            <td>
              <span
                class="chip"
                class:chip-warn={tool.approval_policy === 'gated'}
                class:chip-danger={tool.approval_policy === 'denied'}
              >
                {tool.approval_policy}
              </span>
            </td>
            <td><span class="chip">{tool.reliability_tier}</span></td>
            <td class="muted">{lastUsed(tool.last_used_at)}</td>
            <td class="muted">{tool.owner || '—'}</td>
          </tr>
        {/each}
      {/if}
    </tbody>
  </table>
</div>

<style>
  .catalog {
    overflow: auto;
    border: var(--border-width-thin) solid var(--color-border);
    border-radius: var(--radius-md);
    background: var(--color-surface);
  }

  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  th {
    text-align: left;
    padding: var(--space-2) var(--space-3);
    color: var(--color-text-muted);
    font-weight: 600;
    border-bottom: var(--border-width-thin) solid var(--color-border);
    position: sticky;
    top: var(--space-0);
    background: var(--color-surface-raised);
  }

  td {
    padding: var(--space-2) var(--space-3);
    border-bottom: var(--border-width-thin) solid var(--color-border);
    height: var(--layout-table-row-height);
  }

  tr.selected {
    background: var(--color-accent-soft);
  }

  tbody tr:not(.skeleton-row):hover {
    background: var(--color-surface-raised);
    cursor: pointer;
  }

  .name {
    color: var(--color-text);
    font-weight: 600;
  }

  .muted {
    color: var(--color-text-muted);
  }

  .empty {
    text-align: center;
    color: var(--color-text-muted);
    padding: var(--space-8);
  }

  .chip {
    display: inline-block;
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
    color: var(--color-text);
    font-size: var(--text-xs);
  }

  .chip-transport {
    color: var(--color-accent);
  }

  .chip-ok {
    background: var(--color-success-soft);
    color: var(--color-success);
  }

  .chip-warn {
    background: var(--color-warning-soft);
    color: var(--color-warning);
  }

  .chip-danger {
    background: var(--color-danger-soft);
    color: var(--color-danger);
  }

  .skeleton {
    display: block;
    height: var(--text-base);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
  }
</style>
