<script lang="ts">
  // Harbor Console — Playground bottom status bar (Phase 108 / D-167).
  //
  // Rendered into the shell's `status-bar` slot. Four indicators on a
  // single 28-pt row: streaming-state chip, Protocol version, Events
  // Stream live indicator, Console build chip.
  //
  // Design tokens only; no raw literals.

  import StatusChip from '$lib/components/ui/StatusChip.svelte';

  let {
    streaming,
    protocolVersion,
    eventsStreamLive,
    consoleVersion
  }: {
    /** True when the page is actively receiving stream deltas. */
    streaming: boolean;
    /** The Protocol version string (e.g. "v1.0"). */
    protocolVersion: string;
    /** True when the EventSource is OPEN. */
    eventsStreamLive: boolean;
    /** The Console build version (e.g. "v0.1.x" or "dev"). */
    consoleVersion: string;
  } = $props();

  const streamKind = $derived<'success' | 'warning' | 'neutral'>(
    streaming ? 'success' : 'neutral'
  );
  const streamLabel = $derived(streaming ? 'Streaming' : 'Idle');

  const eventsKind = $derived<'success' | 'neutral'>(
    eventsStreamLive ? 'success' : 'neutral'
  );
  const eventsLabel = $derived(
    eventsStreamLive ? '● Events Stream: Live' : 'Events Stream: Off'
  );
</script>

<div class="playground-status-bar" data-testid="playground-status-bar">
  <div class="status-left">
    <StatusChip kind={streamKind} label={streamLabel} />
    <span class="protocol-version">Protocol {protocolVersion}</span>
    <StatusChip kind={eventsKind} label={eventsLabel} />
  </div>
  <div class="status-right">
    <span class="console-version">Console {consoleVersion}</span>
  </div>
</div>

<style>
  .playground-status-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    height: var(--size-status-bar-height);
    padding: 0 var(--space-4);
    border-top: var(--border-hairline);
    background: var(--color-surface);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .status-left,
  .status-right {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .protocol-version,
  .console-version {
    font-family: var(--font-mono);
  }
</style>
