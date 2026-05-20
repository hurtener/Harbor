<script lang="ts">
  // Strategy / overlay chip row — Phase 73j / D-118.
  //
  // Color-coded chips for the V1 memory strategies (Phase 24 taxonomy).
  // Selecting a chip pins a strategy overlay filter applied to the next
  // `memory.list` call. Chips for unshipped overlay strategies
  // (`pinned` / `episodic` / `recent` / `persistent`) are ABSENT — no
  // placeholder UI (page-memory.md §12). D-065: the `Pinned` overlay is
  // a Phase 24 STRATEGY, not a session priority; this row never renders
  // a priority dimension. Svelte 5 runes mode (D-092).
  import type { MemoryStrategyName } from '$lib/protocol-memory';

  let {
    selected,
    onSelect
  }: {
    /** The currently-pinned strategy overlay, or null for "all". */
    selected: MemoryStrategyName | null;
    onSelect: (strategy: MemoryStrategyName | null) => void;
  } = $props();

  // The V1 strategy taxonomy (Phase 24). Each carries a token class for
  // its color-coded chip.
  const strategies: { name: MemoryStrategyName; label: string }[] = [
    { name: 'none', label: 'None' },
    { name: 'truncation', label: 'Truncation' },
    { name: 'rolling_summary', label: 'Rolling summary' }
  ];
</script>

<div class="chip-row" role="group" aria-label="Memory strategy overlay">
  <button
    type="button"
    class="chip"
    class:active={selected === null}
    onclick={() => onSelect(null)}
  >
    All
  </button>
  {#each strategies as s (s.name)}
    <button
      type="button"
      class="chip strategy-{s.name}"
      class:active={selected === s.name}
      onclick={() => onSelect(s.name)}
    >
      {s.label}
    </button>
  {/each}
</div>

<style>
  .chip-row {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .chip {
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-lg);
    border: var(--border-width-hairline) solid var(--color-border);
    background: var(--color-surface);
    color: var(--color-text-muted);
    cursor: pointer;
    transition: background var(--motion-fast) var(--motion-ease);
  }

  .chip:hover {
    background: var(--color-surface-raised);
  }

  .chip.active {
    color: var(--color-text);
    border-color: var(--color-accent);
  }

  .chip.strategy-none.active {
    border-color: var(--color-text-muted);
  }

  .chip.strategy-truncation.active {
    border-color: var(--color-accent);
  }

  .chip.strategy-rolling_summary.active {
    border-color: var(--color-success);
  }
</style>
