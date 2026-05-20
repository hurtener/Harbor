<script lang="ts">
  // Harbor Console — shared Pagination (D-121, CONVENTIONS.md §3/§5).
  //
  // Real pagination: page / page-size / total display with prev / next
  // controls. Every Console page carries this — never a fake "load more".
  // The component is controlled: it emits `onpage` / `onpagesize`; the
  // page owns the state and re-invokes its loader. Svelte 5 runes (D-092).

  let {
    page,
    pageSize,
    total,
    pageSizeOptions = [25, 50, 100],
    onpage,
    onpagesize
  }: {
    /** The current 1-based page number. */
    page: number;
    /** The current page size. */
    pageSize: number;
    /** The total matched-row count across all pages. */
    total: number;
    /** Selectable page sizes. */
    pageSizeOptions?: number[];
    /** Emitted with the requested new page number. */
    onpage?: (page: number) => void;
    /** Emitted with the requested new page size. */
    onpagesize?: (size: number) => void;
  } = $props();

  const pageCount = $derived(Math.max(1, Math.ceil(total / Math.max(1, pageSize))));
  const firstRow = $derived(total === 0 ? 0 : (page - 1) * pageSize + 1);
  const lastRow = $derived(Math.min(total, page * pageSize));
  const canPrev = $derived(page > 1);
  const canNext = $derived(page < pageCount);

  function prev() {
    if (canPrev) onpage?.(page - 1);
  }
  function next() {
    if (canNext) onpage?.(page + 1);
  }
  function changeSize(event: Event) {
    const value = Number((event.currentTarget as HTMLSelectElement).value);
    onpagesize?.(value);
  }
</script>

<nav class="pagination" aria-label="Pagination">
  <span class="range" data-testid="pagination-range">
    {firstRow}–{lastRow} of {total}
  </span>
  <label class="size">
    Rows
    <select value={pageSize} onchange={changeSize} aria-label="Rows per page">
      {#each pageSizeOptions as size (size)}
        <option value={size}>{size}</option>
      {/each}
    </select>
  </label>
  <div class="controls">
    <button type="button" onclick={prev} disabled={!canPrev} aria-label="Previous page">
      ← Prev
    </button>
    <span class="page-of">Page {page} / {pageCount}</span>
    <button type="button" onclick={next} disabled={!canNext} aria-label="Next page">
      Next →
    </button>
  </div>
</nav>

<style>
  .pagination {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .size {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  select {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
  }

  .controls {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    margin-left: auto;
  }

  button {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  button:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
