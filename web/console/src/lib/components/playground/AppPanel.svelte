<script lang="ts">
  // Harbor Console — Playground App panel (DisplayMode layout).
  //
  // The page-level region that hosts an MCP App when it is in `fullscreen` or
  // `pip` mode (as opposed to the `inline` widget in the chat scroll). It
  // REUSES the Phase 109b MCP-Apps renderer — it does NOT fork it — by pulling
  // the registered renderer component out of the shared renderer registry
  // (`dispatchRenderer(MCP_APP_INLINE_MIME)`), the public §4.5#11 seam. The
  // renderer's iframe host + AppBridge are unchanged; this component only frames
  // it with a header carrying the operator affordances (switch mode / close)
  // and forwards the app's `onrequestdisplaymode` requests up to the page's
  // layout state machine.
  //
  // Design tokens only.
  import {
    dispatchRenderer,
    MCP_APP_INLINE_MIME,
    type RendererProps
  } from '$lib/chat/renderers/index.js';
  import type {
    DisplayModeRequest,
    MCPAppHostClient
  } from '$lib/chat/renderers/app-bridge-host.js';
  import type { DisplayMode, OpenApp } from './layout.js';

  let {
    app,
    appHostClient,
    onrequestmode,
    onclose
  }: {
    /** The page-level app to host (carries server id, resource uri, mode). */
    app: OpenApp;
    /** The injected Harbor Protocol surface the renderer drives app→host on. */
    appHostClient: MCPAppHostClient;
    /** Emitted when the app or the operator requests a different display mode. */
    onrequestmode: (mode: DisplayMode) => void;
    /** Emitted when the operator closes the app. */
    onclose: () => void;
  } = $props();

  // Reuse the registered 109b renderer (no fork) via the public registry seam.
  const Renderer = dispatchRenderer(MCP_APP_INLINE_MIME).component;

  // The host now supports the full DisplayMode set (109a/109b advertised inline
  // only); the page is the surface that applies fullscreen / pip, so the app's
  // requests are granted here and routed to the page's layout machine.
  const AVAILABLE: DisplayMode[] = ['inline', 'fullscreen', 'pip'];

  const rendererProps = $derived<RendererProps>({
    mime: MCP_APP_INLINE_MIME,
    src: '',
    app: {
      resourceUri: app.resourceUri,
      displayMode: app.mode,
      rawHtmlTrusted: app.rawHtmlTrusted,
      toolCallId: app.toolCallId,
      toolName: app.toolName
    },
    serverID: app.serverID,
    appHostClient,
    availableDisplayModes: AVAILABLE,
    onDisplayModeRequest: (req: DisplayModeRequest) => onrequestmode(req.granted)
  });

  // Operator affordances — the alternate modes the operator can switch to.
  const SWITCHES: { mode: DisplayMode; label: string }[] = [
    { mode: 'inline', label: 'Inline' },
    { mode: 'pip', label: 'PiP' },
    { mode: 'fullscreen', label: 'Fullscreen' }
  ];
</script>

<div class="app-panel" data-testid="app-panel" data-app-id={app.id} data-mode={app.mode}>
  <header class="app-panel-bar">
    <span class="app-title" title={app.resourceUri}>{app.title}</span>
    <div class="app-actions">
      {#each SWITCHES as sw (sw.mode)}
        <button
          type="button"
          class="mode-btn"
          data-testid="app-mode-{sw.mode}"
          data-active={app.mode === sw.mode}
          disabled={app.mode === sw.mode}
          onclick={() => onrequestmode(sw.mode)}
        >
          {sw.label}
        </button>
      {/each}
      <button type="button" class="close-btn" data-testid="app-panel-close" onclick={onclose}>
        Close
      </button>
    </div>
  </header>
  <div class="app-panel-body" data-testid="app-panel-body">
    <Renderer {...rendererProps} />
  </div>
</div>

<style>
  .app-panel {
    display: flex;
    flex-direction: column;
    min-height: 0;
    height: 100%;
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    overflow: hidden;
    background: var(--color-surface-raised);
  }

  .app-panel-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-2);
    border-bottom: var(--border-hairline);
    background: var(--color-surface);
  }

  .app-title {
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .app-actions {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
  }

  .mode-btn,
  .close-btn {
    background: none;
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    color: var(--color-text-muted);
    font-size: var(--text-2xs);
    padding: var(--space-0) var(--space-1);
    cursor: pointer;
  }

  .mode-btn[data-active='true'] {
    color: var(--color-accent);
    border-color: var(--color-accent);
    background: var(--color-accent-soft);
    cursor: default;
  }

  .mode-btn:disabled {
    cursor: default;
  }

  .close-btn:hover {
    color: var(--color-danger);
    border-color: var(--color-danger);
  }

  .app-panel-body {
    flex: 1;
    min-height: 0;
    overflow: auto;
    display: flex;
    flex-direction: column;
  }

  /* The reused renderer fills the framed body in fullscreen / pip. */
  .app-panel-body :global(.mcp-app),
  .app-panel-body :global(.mcp-app__frame) {
    flex: 1;
    min-height: 0;
    height: 100%;
  }
</style>
