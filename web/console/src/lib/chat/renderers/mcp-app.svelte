<script lang="ts">
  // The MCP Apps inline renderer — a sandboxed iframe hosting a `ui://`
  // MCP App, bridged to the Harbor Runtime through the manual-handler
  // AppBridge (D-173). Lives in the shared chat module ($lib/chat/) and
  // imports NOTHING from outside it; the Harbor Protocol surface is the
  // INJECTED `appHostClient` (D-091, CLAUDE.md §4.5 #11).
  //
  // Lifecycle:
  //   1. Preload the app's `ui://` HTML via the injected client
  //      (→ mcp.servers.read_resource). A document at/above the heavy-content
  //      threshold (D-026) is offloaded to the ArtifactStore by reference;
  //      the host resolves that stub to a presigned URL and fetches the bytes
  //      (→ resolveArtifact → artifacts.get_ref). EITHER content source feeds
  //      the SAME sandboxed `srcdoc` — heavy bytes are NEVER inlined through
  //      the context plane, only fetched at the iframe edge.
  //   2. Wrap the HTML with the strict CSP and load it into the iframe via
  //      `srcdoc` under a sandbox with NO `allow-same-origin` — the iframe
  //      is forced to an opaque origin (no parent-DOM / cookie / localStorage
  //      access).
  //   3. Construct the AppBridge host (manual-handler mode) and connect it to
  //      the iframe's `contentWindow`, completing the `ui/initialize`
  //      handshake. Every app→host request routes through `appHostClient`.
  import type { RendererProps } from './index.js';
  import {
    AppBridgeHost,
    DEFAULT_HOST_INFO,
    type McpUiDisplayMode
  } from './app-bridge-host.js';
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

  // The operator "pop to side-by-side" affordance (host-side, app-independent).
  // The app can ask to grow via the AppBridge `ui/request-display-mode` handshake;
  // this gives the OPERATOR the same reach without the app asking, dispatched
  // through the SAME injected `onDisplayModeRequest` seam (never by reaching into
  // the page — chat-module encapsulation, D-091). The affordance shows ONLY while
  // the app is inline (the page-level fullscreen/pip panels carry their own mode
  // bar) and ONLY for modes the host advertises it can apply (`availableDisplayModes`).
  const isInline = $derived((app?.displayMode ?? 'inline') === 'inline' || app?.displayMode === '');
  const canPip = $derived((availableDisplayModes ?? []).includes('pip'));
  const canFullscreen = $derived((availableDisplayModes ?? []).includes('fullscreen'));
  const showExpand = $derived(
    isInline && onDisplayModeRequest != null && (canPip || canFullscreen)
  );

  // Pop the inline app to a host-applied display mode. The host knows it can
  // apply `mode` (it is in `availableDisplayModes`), so it grants it directly —
  // the same `{requested, granted}` shape the AppBridge handler produces — and
  // the page's layout reducer moves the app to that region (D-062, D-214).
  function popTo(mode: McpUiDisplayMode): void {
    onDisplayModeRequest?.({ requested: mode, granted: mode });
  }

  async function preload(): Promise<void> {
    if (!app || !serverID || !appHostClient) {
      loadState = 'empty';
      return;
    }
    loadState = 'loading';
    errorMessage = '';
    try {
      const resource = await appHostClient.readResource(serverID, app.resourceUri);
      let documentHTML: string;
      if (resource.artifactRef) {
        // The `ui://` document is at/above the heavy-content threshold (D-026),
        // so the Runtime offloaded it to the ArtifactStore by reference rather
        // than inlining it. This is the COMMON case, not a server bug — real
        // Svelte/React App bundles routinely exceed the 32 KiB threshold.
        // Resolve the by-reference stub to a presigned URL and fetch the bytes
        // at the iframe edge, exactly as the inline path uses `content`; the
        // offload (never inlining heavy bytes through the context plane) is
        // correct and preserved — only the document SOURCE differs here.
        const url = await appHostClient.resolveArtifact(resource.artifactRef.id);
        const resp = await fetch(url);
        if (!resp.ok) {
          throw new Error(
            `failed to fetch app document artifact ${resource.artifactRef.id}: HTTP ${resp.status}`,
          );
        }
        documentHTML = await resp.text();
      } else {
        documentHTML = resource.content ?? '';
      }
      if (documentHTML === '') {
        loadState = 'empty';
        return;
      }
      srcdoc = wrapAppDocument(documentHTML, buildAppCSP(trusted));
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
      onDisplayModeRequest,
      // Host identity is injected through the seam (not baked into the module).
      // The Console supplies its own identity; theme stays at the seam default
      // ('dark'), preserving prior behaviour until a theme prop is threaded.
      hostInfo: DEFAULT_HOST_INFO
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
    {#if showExpand}
      <!-- Host-side operator "pop to side-by-side" affordance. Dispatches the
           display-mode request through the injected callback (D-091) — it never
           reaches into the page. The app does not have to ask. -->
      <div class="mcp-app__toolbar" data-testid="mcp-app-expand-bar">
        {#if canPip}
          <button
            type="button"
            class="mcp-app__expand"
            data-testid="mcp-app-expand-pip"
            aria-label="Pop app to side-by-side"
            title="Pop to side-by-side"
            onclick={() => popTo('pip')}
          >
            <span aria-hidden="true">⤢</span>
          </button>
        {/if}
        {#if canFullscreen}
          <button
            type="button"
            class="mcp-app__expand"
            data-testid="mcp-app-expand-fullscreen"
            aria-label="Pop app to fullscreen"
            title="Pop to fullscreen"
            onclick={() => popTo('fullscreen')}
          >
            <span aria-hidden="true">⛶</span>
          </button>
        {/if}
      </div>
    {/if}
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
    position: relative;
    width: 100%;
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    overflow: hidden;
    background: var(--color-surface-raised);
  }

  .mcp-app__toolbar {
    position: absolute;
    top: var(--space-2);
    right: var(--space-2);
    z-index: 1;
    display: flex;
    gap: var(--space-1);
  }

  .mcp-app__expand {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--space-6);
    height: var(--space-6);
    padding: var(--space-0);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    line-height: 1;
    cursor: pointer;
  }

  .mcp-app__expand:hover {
    color: var(--color-text);
    border-color: var(--color-accent);
  }

  .mcp-app__expand:focus-visible {
    outline: var(--border-hairline-width) solid var(--color-accent);
    outline-offset: var(--space-1);
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
