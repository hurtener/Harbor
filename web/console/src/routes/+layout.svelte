<script lang="ts">
  // Harbor Console app shell (Svelte 5 runes mode — D-092).
  //
  // Phase 72h ships the minimal shell: it imports the design-token surface
  // and renders the active route. Every downstream Stage-2 page phase adds
  // its route under `src/routes/` and rides on this shell + the token CSS.
  import '$lib/tokens.css';

  let { children } = $props();
</script>

<main>
  {@render children?.()}
</main>

<style>
  main {
    min-height: 100vh;
    background: var(--color-bg);
    color: var(--color-text);
    font-family: var(--font-sans);
    font-size: var(--text-base);
  }
</style>
