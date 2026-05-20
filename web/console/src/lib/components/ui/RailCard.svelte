<script lang="ts">
  // Harbor Console — shared RailCard (D-121, CONVENTIONS.md §3).
  //
  // One titled card inside a `DetailRail`. A detail rail composes one or
  // more `RailCard`s (Overview, Status, Metadata, Actions, …).
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { Snippet } from 'svelte';

  let {
    title,
    children
  }: {
    /** The card's heading. */
    title: string;
    /** The card body content. */
    children: Snippet;
  } = $props();
</script>

<section class="rail-card">
  <h3 class="rail-card-title">{title}</h3>
  <div class="rail-card-body">{@render children()}</div>
</section>

<style>
  .rail-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .rail-card-title {
    margin: var(--space-0);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .rail-card-body {
    font-size: var(--text-sm);
    color: var(--color-text);
  }
</style>
