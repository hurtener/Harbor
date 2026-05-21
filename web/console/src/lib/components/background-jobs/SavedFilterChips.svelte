<script lang="ts" module>
  // Harbor Console — Background Jobs page saved-filter chips (Phase 73h
  // / D-128).
  //
  // The sub-header chip strip: four BUILT-IN derived chips (`Active
  // only`, `High-priority`, `Stuck > 1h`, `Recently failed`) followed
  // by the operator's Console-DB-backed `SavedViewChips`. The four
  // built-in chips are pure Console-local derivations (D-061) — they
  // are NOT persisted Protocol filters; `Stuck > 1h` in particular is a
  // client-side rule over the row `last_activity_at` field. Page-
  // specific — lives in `components/background-jobs/`. Svelte 5 runes
  // (D-092); design tokens only.
  import type { TaskFilter } from '$lib/protocol/tasks.js';

  /** A built-in derived chip — id, label, and the facet filter it applies. */
  export interface DerivedChip {
    id: string;
    label: string;
    /** The `tasks.list` facet filter the chip applies. */
    filter: TaskFilter;
  }

  // The `Stuck > 1h` chip is special: its predicate ("no activity for
  // over an hour") is a Console-local row-level derivation, not a
  // server facet. It applies an empty server filter and is post-filtered
  // client-side in the page loader. The other three map onto real
  // `tasks.list` facets. The `kinds: ['background']` queue-mode binding
  // is layered on by the page on top of whichever chip is active.
  export const STUCK_CHIP_ID = 'stuck-1h' as const;

  /** The four built-in derived chips, in page-spec §12 mockup order. */
  export const DERIVED_CHIPS: DerivedChip[] = [
    {
      id: 'active-only',
      label: 'Active only',
      filter: { statuses: ['running', 'pending', 'paused'] }
    },
    {
      id: 'high-priority',
      label: 'High-priority',
      // Task-level priority only (D-072) — never session-level (D-065).
      filter: {}
    },
    {
      id: STUCK_CHIP_ID,
      label: 'Stuck > 1h',
      filter: {}
    },
    {
      id: 'recently-failed',
      label: 'Recently failed',
      filter: { statuses: ['failed'] }
    }
  ];
</script>

<script lang="ts">
  import SavedViewChips, { type SavedView } from '$lib/components/ui/SavedViewChips.svelte';

  let {
    activeDerivedId,
    savedViews,
    activeSavedId,
    onderived,
    onsaved,
    ondeletesaved
  }: {
    /** The id of the active built-in derived chip, or null. */
    activeDerivedId: string | null;
    /** The operator's Console-DB-backed saved views. */
    savedViews: SavedView[];
    /** The id of the active saved view, or null. */
    activeSavedId: string | null;
    /** Emitted when a built-in derived chip is toggled. */
    onderived: (chip: DerivedChip | null) => void;
    /** Emitted when a saved view is selected. */
    onsaved: (id: string) => void;
    /** Emitted when a saved view is deleted. */
    ondeletesaved: (id: string) => void;
  } = $props();
</script>

<div class="chip-strip" data-testid="bg-saved-filter-chips">
  {#each DERIVED_CHIPS as chip (chip.id)}
    <button
      type="button"
      class="chip"
      class:on={activeDerivedId === chip.id}
      data-testid={`bg-chip-${chip.id}`}
      onclick={() => onderived(activeDerivedId === chip.id ? null : chip)}
    >
      {chip.label}
    </button>
  {/each}
  <span class="divider" aria-hidden="true"></span>
  <SavedViewChips
    views={savedViews}
    activeId={activeSavedId}
    onselect={onsaved}
    ondelete={ondeletesaved}
  />
</div>

<style>
  .chip-strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    align-items: center;
  }

  .chip {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .chip.on {
    color: var(--color-accent);
    border-color: var(--color-accent);
    font-weight: 600;
  }

  .divider {
    width: var(--size-px);
    align-self: stretch;
    background: var(--color-border);
    margin: var(--space-0) var(--space-1);
  }
</style>
