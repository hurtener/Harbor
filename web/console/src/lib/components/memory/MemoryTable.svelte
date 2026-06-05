<script lang="ts">
  // Harbor Console — Memory page records table (Phase 108n / D-186).
  //
  // A typed wrapper over the shared `ui/DataTable` primitive — it does NOT
  // fork a table (CONVENTIONS.md §3). It renders the identity-scoped memory
  // catalog in the page-mock §12 column order: Memory key / Strategy / Scope /
  // Owner / Created / Last updated / TTL / Size / Driver. The checkbox column
  // is the DataTable's selection model (drives the V1 view-only bulk bar);
  // clicking a row opens the right-rail detail. Page-specific — lives in
  // `components/memory/`. Svelte 5 runes (D-092); design tokens only.
  import DataTable, { type DataTableColumn } from '$lib/components/ui/DataTable.svelte';
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import { strategyKind, relativeTime, shortTime } from '$lib/memory/derive.js';
  import type { MemoryItem } from '$lib/protocol/memory-types.js';

  let {
    rows,
    selected,
    activeKey,
    onselect,
    onselectionchange
  }: {
    rows: MemoryItem[];
    /** The set of checked row keys (drives the bulk bar). */
    selected: Set<string>;
    /** The currently-open record key (drives the row-active highlight). */
    activeKey: string | null;
    /** Emitted when a row is clicked (opens the right-rail detail). */
    onselect: (key: string) => void;
    /** Emitted when the checkbox selection changes. */
    onselectionchange: (next: Set<string>) => void;
  } = $props();

  const COLUMNS: DataTableColumn[] = [
    { key: 'key', label: 'Memory key' },
    { key: 'strategy', label: 'Strategy' },
    { key: 'scope', label: 'Scope' },
    { key: 'owner', label: 'Owner' },
    { key: 'created', label: 'Created' },
    { key: 'updated', label: 'Last updated' },
    { key: 'ttl', label: 'TTL / Expires' },
    { key: 'size', label: 'Size', numeric: true },
    { key: 'driver', label: 'Driver' }
  ];

  function rowKey(r: unknown): string {
    return (r as MemoryItem).key;
  }

  /** Relative expiry, with an honest `—` for the Go zero time (no TTL). */
  function expiry(iso: string | undefined): string {
    if (!iso || iso.startsWith('0001-01-01')) return '—';
    return relativeTime(iso);
  }
</script>

<div class="memory-table">
  <DataTable
    columns={COLUMNS}
    {rows}
    {rowKey}
    selectable
    {selected}
    onselectionchange={(s) => onselectionchange(s)}
    onrowclick={(r) => onselect((r as MemoryItem).key)}
  >
    {#snippet row(r)}
      {@const item = r as MemoryItem}
      <td class="mono key" class:row-active={item.key === activeKey} title={item.key}>{item.key}</td>
      <td><StatusChip kind={strategyKind(item.strategy)} label={item.strategy} /></td>
      <td><StatusChip kind="neutral" label={item.scope} /></td>
      <td class="owner mono">{item.identity.tenant} / {item.identity.user} / {item.identity.session}</td>
      <td class="cell-muted" title={shortTime(item.created_at)}>{relativeTime(item.created_at)}</td>
      <td class="cell-muted" title={shortTime(item.last_updated_at)}>{relativeTime(item.last_updated_at)}</td>
      <td class="cell-muted" title={shortTime(item.expires_at)}>{expiry(item.expires_at)}</td>
      <td class="numeric">{item.size_bytes}</td>
      <td><StatusChip kind="neutral" label={item.driver} /></td>
    {/snippet}
  </DataTable>
</div>

<style>
  /* Compact the shared DataTable's header padding so the dense 9-column
     catalog packs into the card. Scoped to THIS table (the `.memory-table`
     wrapper) so it never reaches another page's DataTable. */
  .memory-table :global(.data-table thead th) {
    padding: var(--space-2) var(--space-2);
  }

  td {
    padding: var(--space-2) var(--space-2);
    vertical-align: middle;
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .key {
    max-width: var(--size-rail);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .row-active {
    color: var(--color-accent);
    font-weight: 600;
  }

  .owner {
    color: var(--color-text-muted);
    white-space: nowrap;
  }

  .cell-muted {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    white-space: nowrap;
  }

  td.numeric {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }
</style>
