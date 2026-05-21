<script lang="ts">
  // Harbor Console — Live Runtime Event Stream dock (Phase 73b / D-126).
  //
  // The bottom-dock left pane (Brief 11 §LR-5): the live event stream
  // for the page's session, fed by the shipped `events.subscribe` SSE
  // endpoint filtered to the session's `(tenant, user, session)`. When
  // the Trace toggle is on, the stream is additionally run-scoped (the
  // D-082 run carrier) so events correlate to one topology node.
  //
  // The dock does NOT hand-roll an `EventSource`: it receives the
  // already-tailed event page as a prop from the page, which owns the
  // `EventsSubscription` wrapper on `HarborClient` (CONVENTIONS.md §6).
  //
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { Event } from '$lib/protocol/events.js';
  import TraceToggle from './trace-toggle.svelte';

  let {
    events,
    streamState,
    traceOn,
    traceRunID,
    ontracetoggle
  }: {
    /** The rolling page of received events, newest-first. */
    events: Event[];
    /** The SSE connection state — drives the status dot. */
    streamState: 'idle' | 'connecting' | 'open' | 'closed' | 'error';
    /** Whether the Trace overlay (run-scoped filter) is active. */
    traceOn: boolean;
    /** The run id the Trace toggle narrows to (the selected node's run). */
    traceRunID: string;
    /** Emitted when the operator toggles trace mode. */
    ontracetoggle: (next: boolean) => void;
  } = $props();

  const stateLabel = $derived(
    streamState === 'open'
      ? 'Streaming'
      : streamState === 'connecting'
        ? 'Connecting…'
        : streamState === 'error'
          ? 'Reconnecting…'
          : 'Idle'
  );
</script>

<section class="event-dock" data-testid="event-stream-dock">
  <header class="dock-header">
    <h3 class="dock-title">Event Stream</h3>
    <span class="stream-state" data-state={streamState} data-testid="event-stream-state">
      {stateLabel}
    </span>
    <TraceToggle on={traceOn} runID={traceRunID} ontoggle={ontracetoggle} />
  </header>

  {#if events.length === 0}
    <p class="dock-empty" data-testid="event-stream-empty">
      No events yet — the stream is live and will fill as the session runs.
    </p>
  {:else}
    <ul class="event-list">
      {#each events as ev, i (`${ev.sequence}-${i}`)}
        <li class="event-row" data-testid="event-stream-row" data-run={ev.run ?? ''}>
          <span class="ev-type">{ev.type}</span>
          <span class="ev-run">{ev.run ?? '—'}</span>
          <span class="ev-time">{ev.occurred_at}</span>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .event-dock {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-height: var(--space-12);
  }

  .dock-header {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .dock-title {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .stream-state {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .stream-state[data-state='open'] {
    color: var(--color-success);
  }

  .stream-state[data-state='error'] {
    color: var(--color-warning);
  }

  .dock-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    padding: var(--space-4);
    text-align: center;
  }

  .event-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    max-height: var(--size-graph-max-height);
    overflow: auto;
  }

  .event-row {
    display: grid;
    grid-template-columns: 2fr 1fr 1.5fr;
    gap: var(--space-2);
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
    border-bottom: var(--border-hairline);
  }

  .ev-type {
    color: var(--color-accent);
    font-weight: 600;
  }

  .ev-run,
  .ev-time {
    color: var(--color-text-muted);
  }
</style>
