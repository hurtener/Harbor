<script lang="ts">
  // Harbor Console — Tools page catalog table (Phase 108k / D-183).
  //
  // A typed wrapper over the shared `ui/DataTable` primitive — it does
  // NOT fork a table (CONVENTIONS.md §3). It renders the registered-tool
  // catalog in the page-spec §12 column order: checkbox / Name / Version /
  // Scope / Transport / OAuth / Approval / Reliability / Last used / Owner.
  // Page-specific — lives in `components/tools/`. Svelte 5 runes (D-092);
  // design tokens only.
  import DataTable, { type DataTableColumn } from '$lib/components/ui/DataTable.svelte';
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import { lastUsed, oauthKind, approvalKind } from '$lib/tools/derive.js';
  import type { Tool } from '$lib/protocol/tools.js';

  let {
    rows,
    selected,
    activeId,
    onselect,
    onselectionchange
  }: {
    /** The catalog rows for the current page. */
    rows: Tool[];
    /** The set of selected tool IDs. */
    selected: Set<string>;
    /** The currently-open tool ID (drives the right-rail). */
    activeId: string | null;
    /** Emitted when a row is clicked (opens the right-rail detail). */
    onselect: (id: string) => void;
    /** Emitted when the row selection changes. */
    onselectionchange: (next: Set<string>) => void;
  } = $props();

  // Column config — the page-spec §12 mockup order. The checkbox column is
  // the DataTable's built-in selection model. N13 (Phase 83x): the
  // Reliability column carries an explicit min-width token so long tier
  // values like `production-tested` no longer truncate mid-glyph.
  const COLUMNS: DataTableColumn[] = [
    { key: 'name', label: 'Name' },
    { key: 'version', label: 'Version' },
    { key: 'scope', label: 'Scope' },
    { key: 'transport', label: 'Transport' },
    { key: 'oauth', label: 'OAuth' },
    { key: 'approval', label: 'Approval' },
    { key: 'reliability', label: 'Reliability', width: 'var(--size-col-reliability)' },
    { key: 'last_used', label: 'Last used' },
    { key: 'owner', label: 'Owner' }
  ];

  function rowKey(r: unknown): string {
    return (r as Tool).id;
  }
</script>

<div class="tools-table">
  <DataTable
    columns={COLUMNS}
    rows={rows}
    {rowKey}
    selectable
    {selected}
    onselectionchange={(s) => onselectionchange(s)}
    onrowclick={(r) => onselect((r as Tool).id)}
  >
    {#snippet row(r)}
      {@const t = r as Tool}
      <td
        class="cell-name"
        class:row-active={t.id === activeId}
        data-testid="tools-catalog-row"
        data-tool-id={t.id}
        title={t.id}
      >
        {t.name}
      </td>
      <td class="cell-muted">{t.version || '—'}</td>
      <td class="cell-muted">{t.scope}</td>
      <td><StatusChip kind="neutral" label={t.transport} /></td>
      <td><StatusChip kind={oauthKind(t.oauth_status)} label={t.oauth_status} /></td>
      <td><StatusChip kind={approvalKind(t.approval_policy)} label={t.approval_policy} /></td>
      <td class="cell-tier">{t.reliability_tier}</td>
      <td class="cell-muted">{lastUsed(t.last_used_at)}</td>
      <td class="cell-muted">{t.owner || '—'}</td>
    {/snippet}
  </DataTable>
</div>

<style>
  /* Compact the shared DataTable's header padding to match the body cells
     so the dense 9-column catalog packs into the card without a horizontal
     scroll. Scoped to THIS table (the `.tools-table` wrapper) so it never
     reaches another page's DataTable (the unscoped-:global leak that bit
     Background Jobs). */
  .tools-table :global(.data-table thead th) {
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

  .cell-muted {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    white-space: nowrap;
  }

  .cell-tier {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    white-space: nowrap;
  }
</style>
