<script lang="ts">
  // Harbor Console — shared ConnectionFooter (D-121, CONVENTIONS.md §2/§3).
  //
  // The footer the app shell renders below every page: the Runtime base
  // URL + a connected / reconnecting / disconnected status dot. It reads
  // the connection through `connection.ts` — never `localStorage` directly.
  // Svelte 5 runes mode (D-092); design tokens only.
  import { resolveConnection } from '$lib/connection.js';

  let {
    /** Optional explicit connection status override (the shell may track
        a live reconnect state); defaults to resolving from storage. */
    status
  }: {
    status?: 'connected' | 'reconnecting' | 'disconnected';
  } = $props();

  const connection = $derived(resolveConnection());
  const resolvedStatus = $derived(status ?? (connection ? 'connected' : 'disconnected'));
  const label = $derived(
    resolvedStatus === 'connected'
      ? 'Connected'
      : resolvedStatus === 'reconnecting'
        ? 'Reconnecting…'
        : 'Disconnected'
  );
</script>

<footer class="connection-footer" data-testid="connection-footer">
  <span class="dot" data-status={resolvedStatus} aria-hidden="true"></span>
  <span class="label">{label}</span>
  {#if connection}
    <span class="url" title={connection.baseURL}>{connection.baseURL}</span>
  {:else}
    <span class="url muted">no Runtime attached</span>
  {/if}
</footer>

<style>
  .connection-footer {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-4);
    border-top: var(--border-hairline);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .dot {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: 50%;
    flex-shrink: 0;
  }

  .dot[data-status='connected'] {
    background: var(--color-success);
  }

  .dot[data-status='reconnecting'] {
    background: var(--color-warning);
  }

  .dot[data-status='disconnected'] {
    background: var(--color-danger);
  }

  .url {
    font-family: var(--font-mono);
  }

  .url.muted {
    font-style: italic;
  }
</style>
