<script lang="ts">
  // Harbor Console — shared FilterBar (D-121, CONVENTIONS.md §3/§5).
  //
  // The horizontal filter bar every Console page carries (even if it holds
  // only a search input). Four snippet slots: saved-view chips, facet chips,
  // search, export controls. A page composes whichever slots it needs.
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { Snippet } from 'svelte';

  let {
    saved,
    facets,
    search,
    actions
  }: {
    /** Slot for `SavedViewChips` (Console-DB-backed saved views). */
    saved?: Snippet;
    /** Slot for facet-filter chips. */
    facets?: Snippet;
    /** Slot for the free-text search input. */
    search?: Snippet;
    /** Slot for export / page-level filter actions, right-aligned. */
    actions?: Snippet;
  } = $props();
</script>

<div class="filter-bar" role="search">
  {#if saved}
    <div class="slot saved">{@render saved()}</div>
  {/if}
  {#if facets}
    <div class="slot facets">{@render facets()}</div>
  {/if}
  {#if search}
    <div class="slot search">{@render search()}</div>
  {/if}
  {#if actions}
    <div class="slot actions">{@render actions()}</div>
  {/if}
</div>

<style>
  .filter-bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-0);
  }

  .slot {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .search {
    flex: 1;
    min-width: var(--size-search-min);
  }

  .actions {
    margin-left: auto;
  }
</style>
