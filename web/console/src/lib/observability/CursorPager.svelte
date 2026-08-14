<script lang="ts">
  // Harbor Console — Observability cursor pager (HA-65, Phase 247).
  //
  // Honest pagination over the wire's deterministic full-query-bound
  // cursor. The shared `ui/Pagination` component requires a TOTAL row
  // count; `observability.query` returns NO total — only the opaque
  // `next_cursor` ("" = last page) — so fabricating a total to feed the
  // shared pager would be silent degradation (CONVENTIONS.md §5, §13).
  // This pager therefore shows only what the wire proves: the current
  // page number, a Previous control (when the cursor stack has depth),
  // a Next control (only while `next_cursor` is non-empty), and the
  // page-size selector. The cursor is opaque and full-query-bound — any
  // control change restarts from page 1 (the state owns the stack).
  let {
    page,
    pageSize,
    hasNext,
    pageSizeOptions,
    onpage,
    onpagesize
  }: {
    /** The current 1-based page number. */
    page: number;
    /** The current page size. */
    pageSize: number;
    /** True while the response carried a non-empty `next_cursor`. */
    hasNext: boolean;
    /** Selectable page sizes. */
    pageSizeOptions?: readonly number[];
    /** Emitted with the requested next (page+1) / previous (page-1). */
    onpage?: (page: number) => void;
    /** Emitted with the requested new page size. */
    onpagesize?: (size: number) => void;
  } = $props();

  const canPrev = $derived(page > 1);

  function changeSize(event: Event) {
    const value = Number((event.currentTarget as HTMLSelectElement).value);
    onpagesize?.(value);
  }
</script>

<nav class="cursor-pager" aria-label="Observability pagination">
  <span class="page-of" data-testid="obs-pager-page">Page {page}</span>
  <div class="controls">
    <button type="button" onclick={() => onpage?.(page - 1)} disabled={!canPrev} aria-label="Previous page">
      ← Prev
    </button>
    <button
      type="button"
      onclick={() => onpage?.(page + 1)}
      disabled={!hasNext}
      aria-label="Next page"
      title={hasNext ? undefined : 'No further rows — the runtime returned an empty next cursor'}
    >
      Next →
    </button>
  </div>
  <label class="size">
    Rows
    <select value={pageSize} onchange={changeSize} aria-label="Rows per page">
      {#each pageSizeOptions ?? [50, 100, 250] as size (size)}
        <option value={size}>{size}</option>
      {/each}
    </select>
  </label>
</nav>

<style>
  .cursor-pager {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .page-of {
    margin-right: auto;
  }

  .controls {
    display: flex;
    gap: var(--space-2);
  }

  .controls button {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .controls button:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .size select {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
  }
</style>
