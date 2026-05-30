<script lang="ts">
  // Harbor Console — root layout (D-121).
  //
  // The root layout's only job is to import the design-token surface so it
  // is in scope for every route, then render the active route. The app
  // shell (sidebar / top bar / footer) lives in `(console)/+layout.svelte`
  // — the `(console)` route group attaches it to every Console page
  // (CONVENTIONS.md §1/§2). The root `/` route redirects into the group.
  import '$lib/tokens.css';

  let { children } = $props();
</script>

{@render children?.()}

<style>
  /* Global reset. `tokens.css` defines the palette; this gives html/body
     the dark app background and removes the default 8px body margin that
     otherwise produced a full-page scroll (moving the whole shell — header
     + sidebar) and a white gutter around the app. The console shell owns
     all scrolling (each page scrolls its own region), so the document
     itself never scrolls. */
  :global(html),
  :global(body) {
    margin: var(--space-0);
    padding: var(--space-0);
    height: 100vh;
    overflow: hidden;
    background: var(--color-bg);
  }
</style>
