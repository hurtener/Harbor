<script lang="ts">
  // Harbor Console — Live Runtime session-scoped route
  // (`/live-runtime/<session_id>`), Phase 73b / D-126.
  //
  // The deep-link target for a specific session. The Live Runtime page
  // is connection-scoped — it resolves its `(tenant, user, session)`
  // triple from `connection.ts` — so this route renders the SAME page
  // composition as `/live-runtime`; the `session_id` URL segment is the
  // deep-link discriminant (it lets an operator bookmark / share a
  // session view). It does NOT fork the page (CONVENTIONS.md §3 — no
  // duplicated implementation): it renders the one Live Runtime page
  // component.
  //
  // Svelte 5 runes mode (D-092); design tokens only.
  import { page } from '$app/state';
  import LiveRuntimePage from '../+page.svelte';

  // The session id from the URL — surfaced for the breadcrumb / a future
  // explicit-session attach. The page itself reads the live connection.
  const sessionID = $derived(page.params.session_id ?? '');
</script>

<svelte:head>
  <title>Live Runtime · {sessionID} · Harbor Console</title>
</svelte:head>

<div data-testid="live-runtime-session-route" data-session-id={sessionID}>
  <LiveRuntimePage />
</div>
