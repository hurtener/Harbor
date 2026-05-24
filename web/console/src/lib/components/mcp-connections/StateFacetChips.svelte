<script lang="ts" module>
  // MCP Connections — state facet chips (D-121, MCP refactor).
  //
  // The state-facet filter row for the MCP Connections list page. It
  // slots into the shared `<FilterBar>`'s `facets` slot. Each chip is a
  // shared `<StatusChip>` wrapped in a button; clicking applies the
  // single-state facet to the list filter. Page-specific component —
  // lives in `components/mcp-connections/` per CONVENTIONS.md §3.
  import type { MCPServerState } from '$lib/protocol/mcp.js';

  /** The state facets, in mockup order (page-mcp §12). */
  export const STATE_FACETS: { label: string; value: MCPServerState | null }[] = [
    { label: 'All servers', value: null },
    { label: 'Online', value: 'online' },
    { label: 'Reconnecting', value: 'reconnecting' },
    { label: 'Offline', value: 'offline' },
    { label: 'Auth pending', value: 'auth_pending' },
    { label: 'Errored', value: 'error' }
  ];
</script>

<script lang="ts">
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import { mcpStatusKind } from '$lib/mcp-connections/status.js';

  let {
    active,
    disconnected = false,
    onselect
  }: {
    /** The currently-applied single-state facet, or null for "all". */
    active: MCPServerState | null;
    /** When true, the chips desaturate + render non-clickable
        (Phase 83r / N8). Hover-affordances carry the disconnected
        tooltip via the `title` attribute. */
    disconnected?: boolean;
    /** Emitted with the selected state (or null to clear). */
    onselect: (state: MCPServerState | null) => void;
  } = $props();
</script>

{#each STATE_FACETS as facet (facet.label)}
  <button
    type="button"
    class="facet-chip"
    class:active={facet.value === active}
    data-testid={`filter-${facet.value ?? 'all'}`}
    aria-pressed={facet.value === active}
    disabled={disconnected}
    title={disconnected ? 'Attach a Runtime to enable' : undefined}
    onclick={() => onselect(facet.value)}
  >
    {#if facet.value}
      <StatusChip
        kind={mcpStatusKind(facet.value)}
        label={facet.label}
        desaturated={disconnected}
      />
    {:else}
      <span class="all-label">{facet.label}</span>
    {/if}
  </button>
{/each}

<style>
  .facet-chip {
    background: transparent;
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1);
    cursor: pointer;
    line-height: 1;
  }

  .facet-chip.active {
    border-color: var(--color-accent);
  }

  .facet-chip:hover {
    border-color: var(--color-accent);
  }

  .all-label {
    display: inline-flex;
    align-items: center;
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-text-muted);
  }

  .facet-chip.active .all-label {
    color: var(--color-accent);
  }
</style>
