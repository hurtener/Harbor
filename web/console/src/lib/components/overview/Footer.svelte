<script lang="ts">
  // Harbor Console — Overview page footer (Phase 73a / D-127).
  //
  // The page-spec footer (page-overview.md §12): the active runtime
  // name + Protocol version + Events Stream connection state + Console
  // version — `Connected to <runtime> | Protocol v<X.Y.Z> | Events
  // Stream: ON|OFF | Console v<X.Y>`. This is a page-local strip
  // rendered ABOVE the shared `<ConnectionFooter>` (the app shell still
  // renders the shared footer) — it carries the Overview-specific
  // Events-Stream connection posture the shared footer does not.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).

  let {
    runtimeName,
    protocolVersion,
    streamState,
    consoleVersion
  }: {
    /** The active runtime's display name / base URL. */
    runtimeName: string;
    /** The Protocol version the Runtime answered under. */
    protocolVersion: string;
    /** The Events Stream SSE connection state. */
    streamState: 'idle' | 'connecting' | 'open' | 'closed' | 'error';
    /** The Console build version. */
    consoleVersion: string;
  } = $props();

  // The footer renders the stream as ON / OFF per the page spec — ON
  // only while the SSE stream is genuinely open.
  const streamOn = $derived(streamState === 'open');
</script>

<div class="overview-footer" data-testid="overview-footer">
  <span class="seg" data-testid="footer-runtime">Connected to {runtimeName}</span>
  <span class="sep">|</span>
  <span class="seg">Protocol {protocolVersion}</span>
  <span class="sep">|</span>
  <span class="seg" data-on={streamOn} data-testid="footer-stream-state">
    Events Stream: {streamOn ? 'ON' : 'OFF'}
  </span>
  <span class="sep">|</span>
  <span class="seg">Console {consoleVersion}</span>
</div>

<style>
  .overview-footer {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
    align-items: center;
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    border-top: var(--border-hairline);
    padding-top: var(--space-2);
  }

  .sep {
    color: var(--color-border);
  }

  .seg[data-on='true'] {
    color: var(--color-success);
  }
</style>
