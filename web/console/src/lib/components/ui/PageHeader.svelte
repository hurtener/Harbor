<script lang="ts">
  // Harbor Console — shared PageHeader (D-121, CONVENTIONS.md §3/§5).
  //
  // The title + subtitle block at the top of every Console page. The
  // `actions` snippet slot carries page-level buttons (Upload, Export, …).
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { Snippet } from 'svelte';

  let {
    title,
    subtitle,
    actions
  }: {
    /** The page title — rendered as the page's single `<h1>`. */
    title: string;
    /** Optional one-line subtitle under the title. */
    subtitle?: string;
    /** Optional snippet of page-level action controls, right-aligned. */
    actions?: Snippet;
  } = $props();
</script>

<header class="page-header">
  <div class="titles">
    <h1>{title}</h1>
    {#if subtitle}
      <p class="subtitle">{subtitle}</p>
    {/if}
  </div>
  {#if actions}
    <div class="actions">{@render actions()}</div>
  {/if}
</header>

<style>
  .page-header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--space-4);
    padding-bottom: var(--space-4);
    border-bottom: var(--border-hairline);
  }

  .titles {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  h1 {
    margin: var(--space-0);
    font-size: var(--text-xl);
    font-weight: 600;
    color: var(--color-text);
  }

  .subtitle {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-shrink: 0;
  }
</style>
