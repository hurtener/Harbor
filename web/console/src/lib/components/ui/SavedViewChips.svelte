<script lang="ts" module>
  // Harbor Console — shared SavedViewChips (D-121, CONVENTIONS.md §3/§5).
  //
  // The saved-view chip row every Console list page carries. Saved views
  // are Console-LOCAL state — they live in the Console's IndexedDB store,
  // never in the Runtime (D-061: a Console DB holds Console-local state
  // only). The audit found per-page saved-filter chip rows; this is the
  // one. The page owns the persistence (the `$lib/db` saved-filter
  // wrappers); this component is the presentation. Svelte 5 runes (D-092).

  /** One saved view — a named, persisted filter snapshot. */
  export interface SavedView {
    /** The Console-DB row id. */
    id: string;
    /** The operator-facing name. */
    name: string;
  }
</script>

<script lang="ts">
  let {
    views,
    activeId = null,
    onselect,
    ondelete
  }: {
    /** The saved views for this page, from the Console DB. */
    views: SavedView[];
    /** The currently-applied saved view, or null for none. */
    activeId?: string | null;
    /** Emitted with a view id when a chip is clicked. */
    onselect?: (id: string) => void;
    /** Emitted with a view id when a chip's delete affordance fires. */
    ondelete?: (id: string) => void;
  } = $props();
</script>

<div class="saved-view-chips" role="group" aria-label="Saved views">
  {#if views.length === 0}
    <span class="empty">No saved views</span>
  {:else}
    {#each views as view (view.id)}
      <span class="chip" class:active={view.id === activeId}>
        <button type="button" class="chip-label" onclick={() => onselect?.(view.id)}>
          {view.name}
        </button>
        {#if ondelete}
          <button
            type="button"
            class="chip-delete"
            aria-label={`Delete saved view ${view.name}`}
            onclick={() => ondelete?.(view.id)}
          >
            ×
          </button>
        {/if}
      </span>
    {/each}
  {/if}
</div>

<style>
  .saved-view-chips {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .empty {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    font-style: italic;
  }

  .chip {
    display: inline-flex;
    align-items: center;
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .chip.active {
    border-color: var(--color-accent);
  }

  .chip-label {
    background: transparent;
    border: none;
    color: var(--color-text);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .chip.active .chip-label {
    color: var(--color-accent);
    font-weight: 600;
  }

  .chip-delete {
    background: transparent;
    border: none;
    border-left: var(--border-hairline);
    color: var(--color-text-muted);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
    line-height: 1;
    cursor: pointer;
  }
</style>
