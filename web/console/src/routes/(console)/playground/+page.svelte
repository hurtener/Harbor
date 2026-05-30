<script lang="ts">
  // Harbor Console — Playground index (`/playground`), Phase 73n / D-130.
  //
  // The Playground is a session-level surface (CONVENTIONS.md §2 — it is
  // NOT a sidebar entry; it is reached from within a session). This
  // index route resolves the operator's active session from
  // `connection.ts` and redirects to the session-scoped deep-link
  // `/playground/<session_id>`. When no Runtime is attached it renders
  // the Disconnected state — never a request to nowhere.
  //
  // Svelte 5 runes mode (D-092); design tokens only.
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  import { resolveConnection } from '$lib/connection.js';

  let status = $state<PageStatus>('loading');

  onMount(() => {
    const connection = resolveConnection();
    if (connection === null) {
      status = 'disconnected';
      return;
    }
    // D-171: the connection token is a per-backend credential and no
    // longer pins a session, so `identity.session` is normally blank.
    // Mint a fresh conversation id in that case — sessions are
    // create-on-first-use, so the new id materialises on the first send
    // (this matches the "New session" action). Without this, a blank
    // session deep-linked to `/playground/` (an empty id) and rendered
    // nothing.
    const session =
      connection.identity.session !== ''
        ? connection.identity.session
        : `sess-${crypto.randomUUID().slice(0, 12)}`;
    void goto(`/playground/${encodeURIComponent(session)}`, { replaceState: true });
  });
</script>

<svelte:head>
  <title>Playground · Harbor Console</title>
</svelte:head>

<div class="playground-index" data-testid="playground-index">
  <PageState status={status} />
</div>

<style>
  .playground-index {
    padding: var(--space-6);
  }
</style>
