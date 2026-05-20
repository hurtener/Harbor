<script lang="ts">
  // Harbor Console app shell (Svelte 5 runes mode — D-092).
  //
  // Phase 72h ships the minimal shell: it imports the design-token surface
  // and renders the active route. Every downstream Stage-2 page phase adds
  // its route under `src/routes/` and rides on this shell + the token CSS.
  //
  // Phase 73f stamps the `console-hydrated` marker the Playwright harness
  // (Phase 75 `BasePage.waitForHydration`) waits on — the harness baseline
  // already expects it (`harness.spec.ts`); the marker was missing from
  // the Phase 72h scaffold. The marker appears once SvelteKit has
  // hydrated and `onMount` has run, so specs wait on a real signal rather
  // than a fixed timeout (CLAUDE.md §17.4 — no sleeps as synchronisation).
  import { onMount } from 'svelte';
  import '$lib/tokens.css';

  let { children } = $props();

  let hydrated = $state(false);
  onMount(() => {
    hydrated = true;
  });
</script>

<main data-testid={hydrated ? 'console-hydrated' : 'console-hydrating'}>
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
