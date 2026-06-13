<script lang="ts">
  // The MCP Apps inline renderer — a sandboxed iframe hosting a `ui://`
  // MCP App, bridged to the Harbor Runtime through the manual-handler
  // AppBridge (D-173). Lives in the shared chat module ($lib/chat/) and
  // imports NOTHING from outside it; the Harbor Protocol surface is the
  // INJECTED `appHostClient` (D-091, CLAUDE.md §4.5 #11).
  //
  // Lifecycle:
  //   1. Preload the app's `ui://` HTML via the injected client
  //      (→ mcp.servers.read_resource). Heavy content fails loudly — an
  //      app document is never silently truncated or inlined past the
  //      threshold (D-026).
  //   2. Wrap the HTML with the strict CSP and load it into the iframe via
  //      `srcdoc` under a sandbox with NO `allow-same-origin` — the iframe
  //      is forced to an opaque origin (no parent-DOM / cookie / localStorage
  //      access).
  //   3. Construct the AppBridge host (manual-handler mode) and connect it to
  //      the iframe's `contentWindow`, completing the `ui/initialize`
  //      handshake. Every app→host request routes through `appHostClient`.
  import type { RendererProps } from './index.js';
  import { AppBridgeHost } from './app-bridge-host.js';
  import { appIframeSandbox, buildAppCSP, wrapAppDocument } from './sandbox-policy.js';

  let {
    app,
    serverID,
    appHostClient,
    availableDisplayModes,
    onDisplayModeRequest
  }: RendererProps = $props();

  type LoadState = 'loading' | 'ready' | 'error' | 'empty';

  let loadState = $state<LoadState>('loading');
  let errorMessage = $state('');
  let srcdoc = $state('');
  let iframeEl = $state<HTMLIFrameElement | null>(null);
  let host: AppBridgeHost | undefined;

  // The sandbox token set + CSP are derived from the per-server raw-HTML
  // trust posture. `appIframeSandbox` GUARANTEES `allow-same-origin` is never
  // present (it throws otherwise), so the iframe can never escape the sandbox.
  const trusted = $derived(app?.rawHtmlTrusted ?? false);
  const sandbox = $derived(appIframeSandbox(trusted));

  async function preload(): Promise<void> {
    if (!app || !serverID || !appHostClient) {
      loadState = 'empty';
      return;
    }
    loadState = 'loading';
    errorMessage = '';
    try {
      const resource = await appHostClient.readResource(serverID, app.resourceUri);
      if (resource.artifactRef) {
        // A `ui://` document at/above the heavy threshold is a server bug for
        // an inline app — refuse loudly rather than render a blank frame.
        throw new Error(
          `app document ${app.resourceUri} exceeds the inline heavy-content ` +
            `threshold (artifact ${resource.artifactRef.id})`,
        );
      }
      if (!resource.content) {
        loadState = 'empty';
        return;
      }
      srcdoc = wrapAppDocument(resource.content, buildAppCSP(trusted));
      loadState = 'ready';
    } catch (err) {
      errorMessage = err instanceof Error ? err.message : String(err);
      loadState = 'error';
    }
  }

  // Once the iframe is mounted with the app document, connect the bridge to
  // its contentWindow. The PostMessageTransport only accepts messages from
  // that exact window (origin / source validation), so a foreign frame's
  // messages are rejected.
  async function connectBridge(): Promise<void> {
    if (!iframeEl?.contentWindow || !app || !serverID || !appHostClient) return;
    if (host) return;
    host = new AppBridgeHost({
      client: appHostClient,
      serverID,
      availableDisplayModes,
      onDisplayModeRequest
    });
    try {
      await host.connect(iframeEl.contentWindow);
    } catch (err) {
      errorMessage = err instanceof Error ? err.message : String(err);
      loadState = 'error';
    }
  }

  $effect(() => {
    void preload();
  });

  $effect(() => {
    if (loadState === 'ready' && iframeEl) {
      void connectBridge();
    }
    return () => {
      if (host) {
        void host.close();
        host = undefined;
      }
    };
  });
</script>

<div class="mcp-app" data-renderer-source="mcp-app" data-display-mode={app?.displayMode || 'inline'}>
  {#if loadState === 'loading'}
    <div class="mcp-app__state" data-state="loading" role="status">
      <span class="mcp-app__spinner" aria-hidden="true"></span>
      <span>Loading app…</span>
    </div>
  {:else if loadState === 'error'}
    <div class="mcp-app__state" data-state="error" role="alert">
      <span class="mcp-app__state-title">App failed to load</span>
      <span class="mcp-app__state-detail">{errorMessage}</span>
    </div>
  {:else if loadState === 'empty'}
    <div class="mcp-app__state" data-state="empty">
      <span>No app content.</span>
    </div>
  {:else}
    <iframe
      bind:this={iframeEl}
      class="mcp-app__frame"
      title="MCP App"
      {sandbox}
      {srcdoc}
      allow=""
      referrerpolicy="no-referrer"
      data-trusted={trusted}
    ></iframe>
  {/if}
</div>

<style>
  .mcp-app {
    width: 100%;
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    overflow: hidden;
    background: var(--color-surface-raised);
  }

  .mcp-app__frame {
    width: 100%;
    min-height: var(--size-app-inline-min);
    border: none;
    display: block;
  }

  .mcp-app__state {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    padding: var(--space-4);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .mcp-app__state[data-state='loading'] {
    flex-direction: row;
    align-items: center;
  }

  .mcp-app__state[data-state='error'] {
    color: var(--color-danger);
  }

  .mcp-app__state-title {
    font-weight: 600;
  }

  .mcp-app__state-detail {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .mcp-app__spinner {
    width: var(--space-4);
    height: var(--space-4);
    border: var(--border-hairline);
    border-top-color: var(--color-accent);
    border-radius: var(--radius-pill);
    animation: mcp-app-spin var(--motion-slow) linear infinite;
  }

  @keyframes mcp-app-spin {
    to {
      transform: rotate(360deg);
    }
  }
</style>
