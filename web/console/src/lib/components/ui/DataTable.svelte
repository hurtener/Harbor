<script lang="ts" module>
  // Harbor Console — shared DataTable (D-121, CONVENTIONS.md §3/§5).
  //
  // The primary tabular view every Console list page renders. The audit
  // found two forked `CatalogTable.svelte` plus several bespoke tables;
  // this is the one. It is columns-config driven, generic over the row
  // type, with an optional selection model and a built-in empty slot.
  // Svelte 5 runes mode (D-092); design tokens only.

  /** One column definition. */
  export interface DataTableColumn {
    /** Stable column key — also the header data attribute. */
    key: string;
    /** The header label. */
    label: string;
    /** Optional fixed/relative width (a token-derived CSS length). */
    width?: string;
    /** Right-align numeric columns. */
    numeric?: boolean;
  }
</script>

<script lang="ts">
  import type { Snippet } from 'svelte';

  // `DataTable` is row-type agnostic: rows flow as `unknown` and the
  // calling page narrows them inside its `row` snippet (and its `rowKey`
  // / `onrowclick` callbacks). A page that wants compile-time row typing
  // wraps `DataTable` in a thin page-specific component. This keeps the
  // shared primitive lint-clean without a `generics=` attribute the
  // `no-undef` rule cannot see.
  type DataRow = unknown;

  let {
    columns,
    rows,
    rowKey,
    selectable = false,
    selected = new Set<string>(),
    onselectionchange,
    onrowclick,
    row,
    empty
  }: {
    /** The column definitions, left-to-right. */
    columns: DataTableColumn[];
    /** The row data. */
    rows: DataRow[];
    /** Extracts a stable string key from a row. */
    rowKey: (row: DataRow) => string;
    /** When true, a leading checkbox column drives the selection model. */
    selectable?: boolean;
    /** The set of currently-selected row keys. */
    selected?: Set<string>;
    /** Emitted with the new selection set when a checkbox toggles. */
    onselectionchange?: (selected: Set<string>) => void;
    /** Emitted with the row when a row body is clicked. */
    onrowclick?: (row: DataRow) => void;
    /** Renders one row's cells — receives the row; emits `<td>`s. */
    row: Snippet<[DataRow]>;
    /** The built-in empty-state content, shown when `rows` is empty. */
    empty?: Snippet;
  } = $props();

  const allSelected = $derived(
    rows.length > 0 && rows.every((r) => selected.has(rowKey(r)))
  );

  function toggleRow(key: string) {
    const next = new Set(selected);
    if (next.has(key)) {
      next.delete(key);
    } else {
      next.add(key);
    }
    onselectionchange?.(next);
  }

  function toggleAll() {
    const next = new Set<string>();
    if (!allSelected) {
      for (const r of rows) {
        next.add(rowKey(r));
      }
    }
    onselectionchange?.(next);
  }
</script>

<table class="data-table">
  <thead>
    <tr>
      {#if selectable}
        <th class="select-col">
          <input
            type="checkbox"
            checked={allSelected}
            onchange={toggleAll}
            aria-label="Select all rows"
          />
        </th>
      {/if}
      {#each columns as col (col.key)}
        <th
          data-col={col.key}
          class:numeric={col.numeric}
          style={col.width ? `width: ${col.width}` : undefined}
        >
          {col.label}
        </th>
      {/each}
    </tr>
  </thead>
  <tbody>
    {#if rows.length === 0}
      <tr class="empty-row">
        <td colspan={columns.length + (selectable ? 1 : 0)}>
          {#if empty}
            {@render empty()}
          {:else}
            <span class="empty-text">No rows.</span>
          {/if}
        </td>
      </tr>
    {:else}
      {#each rows as r (rowKey(r))}
        <tr class="data-row" class:selected={selected.has(rowKey(r))}>
          {#if selectable}
            <td class="select-col">
              <input
                type="checkbox"
                checked={selected.has(rowKey(r))}
                onchange={() => toggleRow(rowKey(r))}
                aria-label="Select row"
              />
            </td>
          {/if}
          <!-- The row snippet emits the data `<td>`s; wrapping them in a
               clickable region keeps row-click separate from checkboxes. -->
          {#if onrowclick}
            <td
              class="row-body-cell"
              colspan={columns.length}
              onclick={() => onrowclick?.(r)}
            >
              <table class="row-inner"><tbody><tr>{@render row(r)}</tr></tbody></table>
            </td>
          {:else}
            {@render row(r)}
          {/if}
        </tr>
      {/each}
    {/if}
  </tbody>
</table>

<style>
  .data-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  thead th {
    text-align: left;
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
    border-bottom: var(--border-hairline);
  }

  thead th.numeric {
    text-align: right;
  }

  .select-col {
    width: var(--space-8);
  }

  tbody tr.data-row {
    border-bottom: var(--border-hairline);
  }

  tbody tr.data-row.selected {
    background: var(--color-accent-soft);
  }

  .row-body-cell {
    padding: var(--space-0);
    cursor: pointer;
  }

  .row-inner {
    width: 100%;
    border-collapse: collapse;
  }

  .empty-row td {
    padding: var(--space-8) var(--space-3);
    text-align: center;
  }

  .empty-text {
    color: var(--color-text-muted);
  }
</style>
