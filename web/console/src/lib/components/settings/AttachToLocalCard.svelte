<script lang="ts">
  // Settings — Attach to local Runtime card (Phase 105 / V1.2).
  //
  // Renders ONLY when the Console has no active Runtime connection
  // (resolveConnection() === null). Offers a one-click "Attach to
  // local Runtime" button that POSTs to the co-resident bootstrap
  // endpoint and seeds the full connection envelope into localStorage.
  //
  // The card is HIDDEN when a connection already exists.
  import { resolveConnection, attachConnection } from '$lib/connection.js';
  import { runAttachToLocal } from './attach-to-local.js';

  let busy = $state(false);
  let status: 'idle' | 'busy' | 'info' | 'error' = $state('idle');
  let statusText = $state('');

  async function attachToLocal(): Promise<void> {
    busy = true;
    status = 'busy';
    statusText = 'Attaching to local Runtime…';
    const outcome = await runAttachToLocal({
      fetch: fetch.bind(window),
      origin: window.location.origin
    });
    if (outcome.kind === 'attached') {
      attachConnection(outcome.envelope.base_url, {
        token: outcome.envelope.token,
        identity: outcome.envelope.identity,
        scopes: outcome.envelope.scopes
      });
      window.location.reload();
      return;
    }
    status = outcome.kind;
    statusText = outcome.message;
    busy = false;
  }
</script>

{#if resolveConnection() === null}
  <div class="card-body" data-testid="attach-to-local-card">
    <p class="note">
      The Console is running co-resident with a Harbor Runtime on this port.
      Attach to it with one click.
    </p>
    <button
      type="button"
      class="primary"
      data-testid="attach-to-local-runtime"
      disabled={busy}
      onclick={() => void attachToLocal()}
    >
      {busy ? 'Attaching…' : 'Attach to local Runtime'}
    </button>
    {#if status === 'info'}
      <p class="info-banner" data-testid="attach-to-local-info">{statusText}</p>
    {/if}
    {#if status === 'error'}
      <p class="error-banner" data-testid="attach-to-local-error">{statusText}</p>
    {/if}
  </div>
{/if}

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }
  .note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    margin: var(--space-0);
  }
  .primary {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
    background: var(--color-accent);
    border-color: var(--color-accent);
    color: var(--color-bg);
    align-self: flex-start;
  }
  .primary:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
  .info-banner {
    margin: var(--space-0);
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
  .error-banner {
    margin: var(--space-0);
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
    color: var(--color-danger);
    font-size: var(--text-xs);
  }
</style>
