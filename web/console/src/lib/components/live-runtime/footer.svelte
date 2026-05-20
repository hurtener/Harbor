<script lang="ts">
  // Harbor Console — Live Runtime page footer (Phase 73b / D-126).
  //
  // The page-spec mockup §12 footer: Protocol version + Event Stream
  // connection state + Console version. This is a page-local footer
  // strip rendered ABOVE the shared `<ConnectionFooter>` (the app shell
  // still renders the shared footer) — it carries the Live-Runtime-
  // specific Event-Stream connection state the shared footer does not.
  //
  // Svelte 5 runes mode (D-092); design tokens only.

  let {
    protocolVersion,
    streamState,
    consoleVersion
  }: {
    /** The Protocol version the Runtime answered under. */
    protocolVersion: string;
    /** The Event Stream SSE connection state. */
    streamState: 'idle' | 'connecting' | 'open' | 'closed' | 'error';
    /** The Console build version. */
    consoleVersion: string;
  } = $props();

  const streamLabel = $derived(
    streamState === 'open'
      ? 'streaming'
      : streamState === 'connecting'
        ? 'connecting'
        : streamState === 'error'
          ? 'reconnecting'
          : 'idle'
  );
</script>

<div class="lr-footer" data-testid="live-runtime-footer">
  <span class="seg">Protocol {protocolVersion}</span>
  <span class="seg" data-state={streamState} data-testid="footer-stream-state">
    Event Stream: {streamLabel}
  </span>
  <span class="seg">Console {consoleVersion}</span>
</div>

<style>
  .lr-footer {
    display: flex;
    gap: var(--space-4);
    flex-wrap: wrap;
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    border-top: var(--border-hairline);
    padding-top: var(--space-2);
  }

  .seg[data-state='open'] {
    color: var(--color-success);
  }

  .seg[data-state='error'] {
    color: var(--color-warning);
  }
</style>
