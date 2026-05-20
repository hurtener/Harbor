<script lang="ts">
  // Harbor Console — shared BulkActionBar (D-121, CONVENTIONS.md §3).
  //
  // The action bar shown when ≥1 `DataTable` row is selected. The audit
  // found bulk-action toolbars forked across pages; this is the one. The
  // `actions` slot carries page-specific bulk actions — each either calls
  // a real Protocol method or renders disabled-with-tooltip (CONVENTIONS.md
  // §5). Svelte 5 runes mode (D-092); design tokens only.
  import type { Snippet } from 'svelte';

  let {
    count,
    actions,
    onclear
  }: {
    /** The number of currently-selected rows. The bar hides when 0. */
    count: number;
    /** Slot of page-specific bulk-action controls. */
    actions?: Snippet;
    /** Emitted when the operator clears the selection. */
    onclear?: () => void;
  } = $props();
</script>

{#if count > 0}
  <div class="bulk-action-bar" role="toolbar" aria-label="Bulk actions">
    <span class="count" data-testid="bulk-selection-count">{count} selected</span>
    {#if actions}
      <div class="actions">{@render actions()}</div>
    {/if}
    <button type="button" class="clear" onclick={() => onclear?.()}>Clear</button>
  </div>
{/if}

<style>
  .bulk-action-bar {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .count {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .clear {
    margin-left: auto;
    background: transparent;
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }
</style>
