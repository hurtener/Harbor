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
