<script lang="ts">
  // Harbor Console — Live Runtime Health-tab empty state (Phase 73b /
  // D-126).
  //
  // The Health tab content depends on the `runtime.health` primitive
  // that lands in Phase 72f. Phase 73b ships the tab in the strip but
  // renders this empty-state pointer until 72f lands (Brief 11 §LR-2).
  // The "Phase 72f" copy is asserted by the phase-73b smoke's static
  // guard so a renumber forces an update.
  //
  // Svelte 5 runes mode (D-092); design tokens only.
</script>

<div class="tab-empty" data-testid="health-tab-empty">
  <p class="headline">Health is not available yet</p>
  <p class="detail">
    The Health tab consumes the <code>runtime.health</code> Protocol surface,
    which lands in Phase 72f. This tab activates automatically once that
    surface ships.
  </p>
</div>

<style>
  .tab-empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-12) var(--space-4);
    text-align: center;
  }

  .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .detail {
    margin: var(--space-0);
    max-width: var(--size-search-min);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  code {
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    padding: var(--space-0) var(--space-1);
    font-size: var(--text-xs);
  }
</style>
