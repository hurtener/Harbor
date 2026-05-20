<script lang="ts" module>
  // Harbor Console — Live Runtime main-canvas tab strip (Phase 73b /
  // D-126). Topology / Timeline / Metrics / Health (Brief 11 §LR-2).
  //
  // Topology + Timeline are sibling projections of the same
  // `topology.snapshot` data and ship live in Phase 73b. Metrics +
  // Health depend on the `metrics.snapshot` / `runtime.health`
  // primitives that land in Phase 72f — until then those tabs render an
  // empty-state pointer (the 404/405/501 → SKIP analogue).
  //
  // Svelte 5 runes mode (D-092); design tokens only.

  /** The closed set of main-canvas tabs. */
  export type LiveRuntimeTab = 'topology' | 'timeline' | 'metrics' | 'health';
</script>

<script lang="ts">
  let {
    active,
    onselect
  }: {
    /** The currently-selected tab. */
    active: LiveRuntimeTab;
    /** Emitted with the requested new tab. */
    onselect: (tab: LiveRuntimeTab) => void;
  } = $props();

  const tabs: { id: LiveRuntimeTab; label: string }[] = [
    { id: 'topology', label: 'Topology' },
    { id: 'timeline', label: 'Timeline' },
    { id: 'metrics', label: 'Metrics' },
    { id: 'health', label: 'Health' }
  ];
</script>

<nav class="tab-strip" data-testid="live-runtime-tab-strip" aria-label="Live Runtime view">
  {#each tabs as tab (tab.id)}
    <button
      type="button"
      class="tab"
      class:on={tab.id === active}
      data-testid={`tab-${tab.id}`}
      aria-current={tab.id === active ? 'page' : undefined}
      onclick={() => onselect(tab.id)}
    >
      {tab.label}
    </button>
  {/each}
</nav>

<style>
  .tab-strip {
    display: flex;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
  }

  .tab {
    background: transparent;
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-bottom: none;
    border-radius: var(--radius-sm) var(--radius-sm) var(--space-0) var(--space-0);
    padding: var(--space-2) var(--space-4);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .tab.on {
    color: var(--color-accent);
    border-color: var(--color-accent);
    font-weight: 600;
  }
</style>
