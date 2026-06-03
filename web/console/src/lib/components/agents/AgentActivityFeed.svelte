<script lang="ts">
  // Harbor Console — Agents detail recent-activity feed (Phase 73e /
  // D-124; wired live in Phase 108l / D-184). The agent's `agent.*`
  // lifecycle events (registered / restarted / health / drained /
  // deregistered / paused / restart_requested / force_stopped).
  //
  // # Live `events.subscribe` projection + honest empty state (§13)
  //
  // The feed is fed by the detail controller's `events.subscribe` stream
  // filtered to this agent's events (D-184 wires it). It is a LIVE-only
  // view — the runtime has no durable event read-back — so it renders an
  // HONEST "no recent activity" empty state when the stream is quiet; it is
  // NEVER faked with synthetic rows. `entries` is the projected page the
  // controller supplies; `streamState` drives the live indicator.

  /** One projected `agent.*` lifecycle event row. */
  export interface ActivityEntry {
    /** The event type, e.g. `agent.registered`. */
    type: string;
    /** RFC3339 timestamp of the event. */
    at: string;
    /** A short operator-facing summary. */
    summary: string;
  }

  let {
    entries,
    streamState = 'idle'
  }: {
    entries: ActivityEntry[];
    /** The SSE connection state — drives the live dot ('open' = live). */
    streamState?: string;
  } = $props();
</script>

<div class="activity-feed" data-testid="agent-activity-feed">
  <div class="feed-head">
    <span
      class="live-dot"
      class:live={streamState === 'open'}
      data-testid="agent-activity-stream-state"
      title={`stream: ${streamState}`}
    ></span>
    <span class="live-label">{streamState === 'open' ? 'Live' : streamState}</span>
  </div>
  {#if entries.length === 0}
    <p class="empty">
      No recent activity. Agent lifecycle events surface here as they
      stream in.
    </p>
  {:else}
    <ul>
      {#each entries as entry, i (i)}
        <li data-testid="agent-activity-row">
          <span class="event-type mono">{entry.type}</span>
          <span class="event-summary">{entry.summary}</span>
          <span class="event-time">{entry.at}</span>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .activity-feed {
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
