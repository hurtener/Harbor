<script lang="ts">
  // Harbor Console — global app status bar (108a; supersedes the
  // Playground-scoped status bar from 108 + the bare ConnectionFooter).
  //
  // Rendered ONCE by the app shell on EVERY page (mock Image 9): the
  // connection status + runtime name on the left, the live Protocol
  // version + Events-Stream availability in the centre, the Console build
  // on the right. It reads the connection via `connection.ts` (never
  // localStorage directly) and resolves the runtime name from the Console
  // DB address book — the name the operator typed in Settings.
  //
  // The connection segment keeps `data-testid="connection-footer"` so the
  // shared disconnected-state contract (D-161) and the per-page footer
  // assertions continue to resolve against the single global bar.
  //
  // Design tokens only; no raw literals.
  import { onMount } from 'svelte';
  import { resolveConnection } from '$lib/connection.js';
  import { HarborClient } from '$lib/protocol/harbor.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { operatorIdOf } from '$lib/db/schema.js';

  let {
    status
  }: {
    /** Optional explicit connection-status override (shell reconnect state). */
    status?: 'connected' | 'reconnecting' | 'disconnected';
  } = $props();

  const connection = $derived(resolveConnection());
  const resolvedStatus = $derived(status ?? (connection ? 'connected' : 'disconnected'));
  const statusLabel = $derived(
    resolvedStatus === 'connected'
      ? 'Connected'
      : resolvedStatus === 'reconnecting'
        ? 'Reconnecting…'
        : 'Disconnected'
  );

  const CONSOLE_VERSION = import.meta.env.VITE_CONSOLE_VERSION ?? 'dev';

  let runtimeName = $state('');
  let protocolVersion = $state('—');
  let eventsLive = $state(false);

  onMount(() => {
    const conn = resolveConnection();
    if (conn === null) return;
    void (async () => {
      // Protocol version + events-stream availability from runtime.info.
      try {
        const client = new HarborClient({ connection: conn });
        const info = await client.posture.info<{
          protocol_version?: string;
          capabilities?: string[];
        }>();
        if (info.protocol_version) protocolVersion = info.protocol_version;
        eventsLive = (info.capabilities ?? []).includes('events_subscribe');
      } catch {
        /* leave defaults — bar still shows the connection segment */
      }
      // Runtime display name from the Console DB address book.
      try {
        const db = await openListPageDB(conn);
        const operator = await operatorIdOf(conn.identity.tenant, conn.identity.user);
        const runtimes = await db.runtimes.list(operator);
        const hit = runtimes.find((r) => r.base_url === conn.baseURL);
        if (hit?.name) runtimeName = hit.name;
      } catch {
        /* fall back to the base URL */
      }
    })();
  });

  const leftLabel = $derived(
    connection
      ? `${statusLabel} to ${runtimeName || connection.baseURL}`
      : 'No Runtime attached'
  );
</script>

<footer class="app-status-bar" data-testid="app-status-bar">
  <div class="seg left" data-testid="connection-footer">
    <span class="dot" data-status={resolvedStatus} aria-hidden="true"></span>
    <span class="label">{leftLabel}</span>
  </div>

  <div class="seg center">
    {#if connection}
      <span class="meta mono">Protocol {protocolVersion}</span>
      <span class="events">
        <span class="dot" data-status={eventsLive ? 'connected' : 'disconnected'} aria-hidden="true"></span>
        Events Stream: {eventsLive ? 'Live' : 'Off'}
      </span>
    {/if}
  </div>

  <div class="seg right">
    <span class="meta mono">Console {CONSOLE_VERSION}</span>
  </div>
</footer>

<style>
  .app-status-bar {
    display: grid;
    grid-template-columns: 1fr auto 1fr;
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-2) var(--space-4);
    border-top: var(--border-hairline);
    background: var(--color-surface);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    flex-shrink: 0;
  }

  .seg {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }

  .seg.left {
    justify-self: start;
  }

  .seg.center {
    justify-self: center;
    gap: var(--space-4);
  }

  .seg.right {
    justify-self: end;
  }

  .events {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
  }

  .label {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .meta {
    color: var(--color-text-muted);
  }

  .mono {
    font-family: var(--font-mono);
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
</style>
