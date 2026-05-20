<script lang="ts">
  // Artifacts catalog table — Phase 73l (D-120). Renders the
  // `artifacts.list` rows per page-artifacts.md §12: Name / MIME /
  // Created / Owner / Size / Source / Tags / Driver + a row-action menu.
  // Clicking a row selects it for the right-rail preview. The row-action
  // Delete renders disabled-with-tooltip per the spec §10 deferred list.
  import type { ArtifactRow } from '$lib/protocol';

  let {
    rows,
    selectedId,
    onSelect,
    selection = $bindable<Set<string>>(new Set())
  }: {
    rows: ArtifactRow[];
    selectedId: string | null;
    onSelect: (row: ArtifactRow) => void;
    selection: Set<string>;
  } = $props();

  function toggleSelect(id: string): void {
    const next = new Set(selection);
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    selection = next;
  }

  function fmtSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }
</script>

<table class="artifacts-table" data-testid="artifacts-table">
  <thead>
    <tr>
      <th class="checkbox-col"></th>
      <th>Name</th>
      <th>MIME type</th>
      <th>Created</th>
      <th>Owner</th>
      <th>Size</th>
      <th>Source</th>
      <th>Tags</th>
      <th>Driver</th>
      <th>Actions</th>
    </tr>
  </thead>
  <tbody>
    {#if rows.length === 0}
      <tr>
        <td colspan="10" class="empty-row" data-testid="artifacts-empty">
          No artifacts match these filters.
        </td>
      </tr>
    {:else}
      {#each rows as row (row.ref.id)}
        <tr
          class:selected={row.ref.id === selectedId}
          data-testid="artifact-row"
          data-artifact-id={row.ref.id}
        >
          <td class="checkbox-col">
            <input
              type="checkbox"
              checked={selection.has(row.ref.id)}
              onchange={() => toggleSelect(row.ref.id)}
              aria-label="select artifact {row.ref.id}"
            />
          </td>
          <td>
            <button class="name-link" type="button" onclick={() => onSelect(row)}>
              {row.ref.filename || row.ref.id}
            </button>
          </td>
          <td><span class="chip">{row.ref.mime_type || '—'}</span></td>
          <td>{row.created_at ?? '—'}</td>
          <td class="owner">{row.ref.scope.tenant}/{row.ref.scope.user}</td>
          <td>{fmtSize(row.ref.size_bytes)}</td>
          <td><span class="chip">{row.source ?? '—'}</span></td>
          <td>
            {#each row.tags ?? [] as tag (tag)}
              <span class="tag-chip">{tag}</span>
            {/each}
          </td>
          <td><span class="chip">{row.driver ?? '—'}</span></td>
          <td>
            <button
              class="row-action-deferred"
              type="button"
              aria-disabled="true"
              title="Deferred — Phase 73"
            >
              Delete
            </button>
          </td>
        </tr>
      {/each}
    {/if}
  </tbody>
</table>

<style>
  .artifacts-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  th,
  td {
    text-align: left;
    padding: var(--space-2) var(--space-3);
    border-bottom: var(--border-thin) solid var(--color-border);
  }

  th {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
  }

  tr.selected {
    background: var(--color-surface-raised);
  }

  .checkbox-col {
    width: var(--space-8);
  }

  .empty-row {
    color: var(--color-text-muted);
    text-align: center;
    padding: var(--space-8);
  }

  .name-link {
    background: none;
    border: none;
    color: var(--color-accent);
    cursor: pointer;
    font-size: var(--text-sm);
    padding: var(--space-0);
  }

  .owner {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .chip {
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .tag-chip {
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border-radius: var(--radius-lg);
    padding: var(--space-1) var(--space-2);
    margin-right: var(--space-1);
  }

  .row-action-deferred {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: not-allowed;
  }
</style>
