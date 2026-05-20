<script lang="ts">
  // Harbor Console — root `/` route (D-121, CONVENTIONS.md §1).
  //
  // The root path redirects to `/overview` — the first page of the
  // Runtime cluster. The redirect runs client-side on mount (the Console
  // is an SSR-disabled SPA — D-091, `+layout.ts` sets `ssr = false`).
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';

  onMount(() => {
    void goto('/overview', { replaceState: true });
  });
</script>

<noscript>
  <p>Harbor Console requires JavaScript. Open <a href="/overview">/overview</a>.</p>
</noscript>
