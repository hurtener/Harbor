<script lang="ts">
  // Bulk-action toolbar — Phase 73j / D-118.
  //
  // V1 is READ-ONLY (page-memory.md §10 / §12). The mockup's bulk
  // actions (`Delete selected`, `Refresh TTL`, `Pin`) render as
  // disabled `<button>` elements with a tooltip — NOT hidden elements,
  // so a screen-reader user hears the deferral carve-out. The buttons
  // MUST NOT wire to any Protocol call (§13 forbidden practice "two
  // parallel implementations" — the memory mutation surface lands in
  // Phase 73 / post-V1, not here). Svelte 5 runes mode (D-092).
  let { selectedCount }: { selectedCount: number } = $props();

  const tooltip = 'Memory mutation surface deferred — Phase 73';
  const actions = ['Delete selected', 'Refresh TTL', 'Pin'] as const;
</script>

<div
  class="toolbar"
  class:active={selectedCount > 0}
  aria-label="Bulk actions (disabled at V1)"
>
  <span class="count">{selectedCount} selected</span>
  {#each actions as action (action)}
    <!-- Disabled-with-tooltip: the deferred mutation surface. The
         `disabled` attribute + the title tooltip make the carve-out
         audible to screen readers. NO onclick — the button wires to
         nothing (§13). -->
    <button type="button" disabled title={tooltip} aria-disabled="true">
      {action}
    </button>
  {/each}
</div>

<style>
  .toolbar {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    background: var(--color-surface-raised);
    border: var(--border-width-hairline) solid var(--color-border);
    border-radius: var(--radius-sm);
    opacity: 0.6;
  }

  .toolbar.active {
    opacity: 1;
  }

  .count {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    margin-right: var(--space-2);
  }

  button {
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
    background: var(--color-surface);
    color: var(--color-text-muted);
    border: var(--border-width-hairline) solid var(--color-border);
    border-radius: var(--radius-sm);
    cursor: not-allowed;
  }
</style>
