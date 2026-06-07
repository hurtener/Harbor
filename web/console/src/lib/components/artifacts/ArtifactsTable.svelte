<script lang="ts">
  // Harbor Console — Artifacts page catalog table (Phase 108o / D-187).
  //
  // A typed wrapper over the shared `ui/DataTable` primitive — it does NOT
  // fork a table (CONVENTIONS.md §3). It renders the artifact catalog in the
  // page-mock §12 column order: checkbox / Name / MIME / Created / Owner /
  // Size / Source / Tags / Driver / Actions. Clicking a row opens the
  // right-rail preview; the row Delete action is the REAL admin
  // `artifacts.delete` (disabled-with-tooltip without the admin claim, D-079).
  // Page-specific — lives in `components/artifacts/`. Svelte 5 runes (D-092).
  import DataTable, { type DataTableColumn } from '$lib/components/ui/DataTable.svelte';
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import { fmtSize, sourceKind, relativeTime } from '$lib/artifacts/derive.js';
  import type { ArtifactRow } from '$lib/protocol/artifacts.js';

  let {
    rows,
    selected,
    activeId,
    canAdmin,
    onselect,
    onselectionchange,
    ondelete
  }: {
    rows: ArtifactRow[];
    selected: Set<string>;
    /** The currently-previewed artifact id (drives the row-active highlight). */
    activeId: string | null;
    /** True when the connection carries the admin claim (D-079). */
    canAdmin: boolean;
    onselect: (row: ArtifactRow) => void;
    onselectionchange: (next: Set<string>) => void;
    /** Evicts one artifact via the real admin artifacts.delete. */
    ondelete: (id: string) => void;
  } = $props();

  const COLUMNS: DataTableColumn[] = [
    { key: 'name', label: 'Name' },
    { key: 'mime', label: 'MIME type' },
    { key: 'created', label: 'Created' },
    { key: 'owner', label: 'Owner' },
    { key: 'size', label: 'Size', numeric: true },
    { key: 'source', label: 'Source' },
    { key: 'tags', label: 'Tags' },
    { key: 'driver', label: 'Driver' },
    { key: 'actions', label: 'Actions' }
  ];

  const deleteGate = 'Requires the admin scope claim — artifacts.delete is an admin Protocol method (D-079).';

  function rowKey(r: unknown): string {
    return (r as ArtifactRow).ref.id;
  }
</script>

<div class="artifacts-table">
  <DataTable
    columns={COLUMNS}
    {rows}
    {rowKey}
    selectable
    {selected}
    onselectionchange={(s) => onselectionchange(s)}
    onrowclick={(r) => onselect(r as ArtifactRow)}
  >
    {#snippet row(r)}
      {@const a = r as ArtifactRow}
      <td class="cell-name" class:row-active={a.ref.id === activeId} title={a.ref.id}>
        {a.ref.filename || a.ref.id}
      </td>
      <td><StatusChip kind="neutral" label={a.ref.mime_type || '—'} /></td>
      <td class="cell-muted" title={a.created_at ?? ''}>{relativeTime(a.created_at)}</td>
      <td class="owner mono">{a.ref.scope.tenant}/{a.ref.scope.user}</td>
      <td class="cell-num">{fmtSize(a.ref.size_bytes)}</td>
      <td><StatusChip kind={sourceKind(a.source)} label={a.source ?? '—'} /></td>
      <td class="cell-tags">
        {#each a.tags ?? [] as tag (tag)}<span class="tag-chip">{tag}</span>{/each}
      </td>
      <td><StatusChip kind="neutral" label={a.driver ?? '—'} /></td>
      <td>
        <button
          type="button"
          class="row-delete"
          data-testid="artifact-row-delete"
          disabled={!canAdmin}
          title={canAdmin ? 'Evict this artifact (audited)' : deleteGate}
          onclick={(e) => {
            e.stopPropagation();
            ondelete(a.ref.id);
          }}
        >
          Delete
        </button>
      </td>
    {/snippet}
  </DataTable>
</div>

<style>
  .artifacts-table :global(.data-table thead th) {
    padding: var(--space-2) var(--space-2);
  }

  td {
    padding: var(--space-2) var(--space-2);
    vertical-align: middle;
  }

  .cell-name {
    font-size: var(--text-sm);
    color: var(--color-accent);
    max-width: var(--size-rail);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .row-active {
    font-weight: 600;
  }

  .cell-muted {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    white-space: nowrap;
  }

  .owner {
    color: var(--color-text-muted);
    white-space: nowrap;
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .cell-num {
    text-align: right;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .cell-tags {
    white-space: nowrap;
  }

  .tag-chip {
    display: inline-block;
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border-radius: var(--radius-lg);
    padding: var(--space-1) var(--space-2);
    margin-right: var(--space-1);
  }

  .row-delete {
    background: transparent;
    color: var(--color-danger);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .row-delete:disabled {
    opacity: 0.4;
    cursor: not-allowed;
    color: var(--color-text-muted);
  }
</style>
