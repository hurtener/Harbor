<script lang="ts">
  // Harbor Console — Memory page right-rail live event feed
  // (Phase 108n / D-186). Supersedes the pre-chrome deferred placeholder
  // ("identity-rejection + recovery-dropout feeds need the events-stream
  // wiring", D-132 / W5) — the three `memory.*` event types are shipped and
  // `events.subscribe` is wired, so the feed is now LIVE.
  //
  // # Live `events.subscribe` projection + honest empty state (§13)
  //
  // Fed by the controller's `events.subscribe` stream, projected to the
  // `memory.*` events (`projectMemoryEvents`): `memory.identity_rejected`
  // (D-033 fail-loud), `memory.health_changed`, `memory.recovery_dropped`
  // (D-035). LIVE-only — the runtime has no durable event read-back — so it
  // renders an HONEST "no recent activity" empty state when quiet; it is NEVER
  // faked with synthetic rows. `entries` is the projected page the controller
  // supplies; `streamState` drives the live indicator.
  import type { MemoryEventEntry } from '$lib/memory/derive.js';

  let {
    entries,
    streamState = 'idle'
  }: {
    entries: MemoryEventEntry[];
    /** The SSE connection state — drives the live dot ('open' = live). */
    streamState?: string;
  } = $props();
</script>

<div class="memory-events" data-testid="memory-events-feed">
  <div class="feed-head">
    <span
      class="live-dot"
      class:live={streamState === 'open'}
      data-testid="memory-events-stream-state"
      title={`stream: ${streamState}`}
    ></span>
    <span class="live-label">
      {streamState === 'open' ? 'Live' : streamState === 'error' ? 'reconnecting…' : streamState}
    </span>
  </div>
  {#if entries.length === 0}
    <p class="empty" data-testid="memory-events-empty">
      No recent activity. Identity-rejection, driver-health, and recovery-drop
      events surface here as they stream in.
    </p>
  {:else}
    <ul>
      {#each entries as entry (entry.sequence)}
        <li data-testid="memory-event-row">
          <span class="event-type mono">{entry.type}</span>
          <span class="event-summary">{entry.summary}</span>
          <span class="event-time">{entry.at}</span>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .memory-events {
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
