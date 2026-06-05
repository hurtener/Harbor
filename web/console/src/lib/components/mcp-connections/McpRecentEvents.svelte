<script lang="ts">
  // Harbor Console — MCP Connections detail recent-events card
  // (Phase 108m / D-185). The server's `mcp.resource_updated` +
  // `tool.auth_required` + transport-error subset of `tool.failed` events
  // (page-mcp-connections.md §4 right-rail).
  //
  // # Live `events.subscribe` projection + honest empty state (§13)
  //
  // The card is fed by the detail controller's `events.subscribe` stream,
  // projected to THIS server's events (`projectServerEvents`). It is a
  // LIVE-only view — the runtime has no durable event read-back — so it
  // renders an HONEST "no recent activity" empty state when the stream is
  // quiet; it is NEVER faked with synthetic rows. `entries` is the projected
  // page the controller supplies; `streamState` drives the live indicator.
  import type { RecentEventEntry } from '$lib/mcp-connections/derive.js';

  let {
    entries,
    streamState = 'idle'
  }: {
    entries: RecentEventEntry[];
    /** The SSE connection state — drives the live dot ('open' = live). */
    streamState?: string;
  } = $props();
</script>

<div class="recent-events" data-testid="mcp-recent-events">
  <div class="feed-head">
    <span
      class="live-dot"
      class:live={streamState === 'open'}
      data-testid="mcp-events-stream-state"
      title={`stream: ${streamState}`}
    ></span>
    <span class="live-label">
      {streamState === 'open' ? 'Live' : streamState === 'error' ? 'reconnecting…' : streamState}
    </span>
  </div>
  {#if entries.length === 0}
    <p class="empty" data-testid="mcp-recent-events-empty">
      No recent activity. Resource-update, OAuth, and transport-error events
      for this server surface here as they stream in.
    </p>
  {:else}
    <ul>
      {#each entries as entry (entry.sequence)}
        <li data-testid="mcp-recent-event-row">
          <span class="event-type mono">{entry.type}</span>
          <span class="event-summary">{entry.summary}</span>
          <span class="event-time">{entry.at}</span>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .recent-events {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .feed-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .live-dot {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: var(--radius-pill);
    background: var(--color-text-muted);
  }

  .live-dot.live {
    background: var(--color-success);
  }

  .live-label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  ul {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  li {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-0);
    border-bottom: var(--border-hairline);
  }

  .event-type {
    font-size: var(--text-xs);
    color: var(--color-accent);
  }

  .event-summary {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .event-time {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .mono {
    font-family: var(--font-mono);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
