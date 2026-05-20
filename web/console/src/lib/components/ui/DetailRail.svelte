<script lang="ts">
  // Harbor Console — shared DetailRail (D-121, CONVENTIONS.md §3/§5).
  //
  // The right-hand detail rail container. Fixed width via the single
  // `--size-rail` token. A page slots one or more `RailCard`s into it,
  // optionally wrapped in a nested `<PageState>` for rail-scoped async
  // state (CONVENTIONS.md §4). Svelte 5 runes mode (D-092); tokens only.
  import type { Snippet } from 'svelte';

  let {
    children
  }: {
    /** The rail body — typically a stack of `RailCard`s. */
    children: Snippet;
  } = $props();
</script>

<aside class="detail-rail">{@render children()}</aside>

<style>
  .detail-rail {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    width: var(--size-rail);
    flex-shrink: 0;
    min-height: var(--layout-detail-min-height);
    overflow-y: auto;
  }
</style>
