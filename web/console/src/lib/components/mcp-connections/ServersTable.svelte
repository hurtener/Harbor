<script lang="ts">
  // Harbor Console — MCP Connections servers table (Phase 108m / D-185).
  //
  // A typed wrapper over the shared `ui/DataTable` primitive — it does NOT
  // fork a table (CONVENTIONS.md §3). It renders the registered MCP-server
  // catalog in the page-mock §12 column order: Server name / Status /
  // Endpoint / Transport / Tools / Resources / Last connect / OAuth. Clicking
  // a row selects it into the right-rail detail (the master-detail pattern —
  // there is no separate detail route). Page-specific — lives in
  // `components/mcp-connections/`. Svelte 5 runes (D-092); design tokens only.
  import DataTable, { type DataTableColumn } from '$lib/components/ui/DataTable.svelte';
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import { mcpStatusKind, mcpStateLabel, relativeTime } from '$lib/mcp-connections/derive.js';
  import type { MCPServerView } from '$lib/protocol/mcp.js';

  let {
    rows,
    activeName,
    disconnected = false,
    onselect
  }: {
    /** The visible server rows. */
    rows: MCPServerView[];
    /** The currently-open server name (drives the row-active highlight). */
    activeName: string | null;
    /** When true, status chips desaturate (no backing Runtime). */
    disconnected?: boolean;
    /** Emitted when a row is clicked (opens the right-rail detail). */
    onselect: (name: string) => void;
  } = $props();

  const COLUMNS: DataTableColumn[] = [
    { key: 'name', label: 'Server name' },
    { key: 'status', label: 'Status' },
    { key: 'endpoint', label: 'Endpoint' },
    { key: 'transport', label: 'Transport' },
    { key: 'tools', label: 'Tools', numeric: true },
    { key: 'resources', label: 'Resources', numeric: true },
    { key: 'last_connect', label: 'Last connect' },
    { key: 'oauth', label: 'OAuth', numeric: true }
  ];

  function rowKey(r: unknown): string {
    return (r as MCPServerView).name;
  }
</script>

<div class="servers-table">
  <DataTable columns={COLUMNS} {rows} {rowKey} onrowclick={(r) => onselect((r as MCPServerView).name)}>
    {#snippet row(r)}
      {@const srv = r as MCPServerView}
      <td
        class="cell-name"
        class:row-active={srv.name === activeName}
        data-testid={`server-row-${srv.name}`}
        title={srv.name}
      >
        {srv.name}
      </td>
      <td>
        <span data-testid={`status-${srv.name}`}>
          <StatusChip
            kind={mcpStatusKind(srv.state)}
            label={mcpStateLabel(srv.state)}
            desaturated={disconnected}
          />
        </span>
      </td>
      <td class="cell-endpoint" title={srv.url_or_command}>{srv.url_or_command}</td>
      <td class="cell-muted">{srv.transport}</td>
      <td class="cell-num">{srv.tool_count}</td>
      <td class="cell-num">{srv.resource_count}</td>
      <td class="cell-muted">{relativeTime(srv.last_discovery_at)}</td>
      <td class="cell-num">{srv.oauth_binding_count}</td>
    {/snippet}
  </DataTable>
</div>

<style>
  /* Compact the shared DataTable's header padding to match the body cells so
     the dense catalog packs into the card. Scoped to THIS table (the
     `.servers-table` wrapper) so it never reaches another page's DataTable
     (the unscoped-:global leak that bit Background Jobs). */
  .servers-table :global(.data-table thead th) {
    padding: var(--space-2) var(--space-1);
  }

  td {
    padding: var(--space-2) var(--space-1);
    vertical-align: middle;
  }

  .cell-name {
    font-size: var(--text-sm);
    color: var(--color-text);
    white-space: nowrap;
  }

  .row-active {
    color: var(--color-accent);
    font-weight: 600;
  }

  .cell-endpoint {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    max-width: var(--size-input-compact);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .cell-muted {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    white-space: nowrap;
  }

  .cell-num {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    text-align: right;
  }
</style>
