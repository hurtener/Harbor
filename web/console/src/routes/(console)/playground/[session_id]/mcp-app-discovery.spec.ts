// Inline MCP-App discovery → renderer-mount regression guard.
//
// This spec mounts the REAL shipped Svelte components (not a synthetic DOM
// harness): the real `MessageBubble` mounts the real `McpAppRenderer` when a
// `ChatMessage` carries an app ref + server id + an injected `appHostClient`,
// and the real `AppPanel` (driven by the real `reduceLayout` / `computeRegion`
// state machine) hosts the same renderer in the page-level fullscreen / pip
// regions. If the discovery→render wiring is reverted (the bubble's
// `{#if message.app}` block, the threaded `appHostClient`, or the registry
// dispatch), the `data-renderer-source='mcp-app'` element never appears and
// these tests fail — exactly the guard the wave-end audit asked for, replacing
// the prior synthetic-DOM `tests/mcp-app-displaymode.spec.ts` that re-built the
// DOM by hand and re-implemented the clamp.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

import MessageBubble from '$lib/chat/MessageBubble.svelte';
import AppPanel from '$lib/components/playground/AppPanel.svelte';
import type { ChatMessage, ChatProtocolClient } from '$lib/chat/types.js';
import type {
  DisplayModeRequest,
  MCPAppHostClient,
  MCPAppRefView
} from '$lib/chat/renderers/app-bridge-host.js';
import {
  appId,
  computeRegion,
  INITIAL_LAYOUT,
  reduceLayout,
  type AppRef,
  type OpenApp
} from '$lib/components/playground/layout.js';

// A fake injected Protocol surface (D-173 — the renderer NEVER opens a direct
// MCP transport; it drives every app→host request through this). The
// renderer's pre-mount document read routes through `readRenderDocument` (the
// HA-56 opt-in admission-requesting read), which records the (serverID,
// resourceURI) pair so the test can assert the renderer resolved the document
// from the discovered server; the app-initiated `resources/read` handler stays
// on the ordinary non-minting `readResource`.
function fakeHostClient(): MCPAppHostClient & { reads: Array<[string, string]> } {
  const reads: Array<[string, string]> = [];
  return {
    reads,
    async readResource(_serverID, resourceURI) {
      reads.push(['ordinary', resourceURI]);
      return { resourceUri: resourceURI, mimeType: 'text/html', content: '<p>app body</p>' };
    },
    async readRenderDocument(serverID, resourceURI) {
      reads.push([serverID, resourceURI]);
      return { resourceUri: resourceURI, mimeType: 'text/html', content: '<p>app body</p>' };
    },
    async callTool(_serverID, tool) {
      return { tool, content: { ok: true }, isError: false };
    },
    async listResources() {
      return [];
    },
    async listResourceTemplates() {
      return [];
    },
    async listTools() {
      return [];
    },
    async resolveArtifact(id) {
      return `https://artifacts.example/${id}`;
    },
    async toolContext() {
      return null;
    },
    async fetchArtifactText() {
      return '{}';
    }
  };
}

// A no-op chat client — MessageBubble requires it but the MCP-app path never
// calls it.
const noopChatClient = {} as unknown as ChatProtocolClient;

const appView: MCPAppRefView = {
  resourceUri: 'ui://weather/main.html',
  displayMode: 'inline',
  rawHtmlTrusted: false
};

function agentMessageWithApp(): ChatMessage {
  return {
    id: 'm-agent-1',
    role: 'agent',
    text: 'Here is the weather.',
    at: new Date().toISOString(),
    taskID: 'run-1',
    app: appView,
    serverID: 'weather-server'
  };
}

let mounted: ReturnType<typeof mount> | undefined;
let target: HTMLElement | undefined;

// `mount`'s typed overload demands props matching the concrete component; this
// generic harness intentionally erases that to keep the test data-driven, so
// the component + props are cast at the call boundary.
function render(Component: unknown, props: Record<string, unknown>): HTMLElement {
  target = document.createElement('div');
  document.body.appendChild(target);
  mounted = mount(Component as Parameters<typeof mount>[0], {
    target,
    props: props as Record<string, never>
  });
  flushSync();
  return target;
}

afterEach(() => {
  if (mounted) {
    unmount(mounted);
    mounted = undefined;
  }
  target?.remove();
  target = undefined;
  vi.restoreAllMocks();
});

describe('inline MCP-app discovery → renderer mount', () => {
  it('mounts the real MCP-app renderer when the message carries an app + serverID + host client', () => {
    const client = fakeHostClient();
    const root = render(MessageBubble, {
      message: agentMessageWithApp(),
      client: noopChatClient,
      appHostClient: client,
      availableDisplayModes: ['inline', 'fullscreen', 'pip']
    });

    // The REAL renderer mounted (its stable marker), inside the bubble's app slot.
    const renderer = root.querySelector("[data-renderer-source='mcp-app']");
    expect(renderer, 'the inline MCP-app renderer mounted').not.toBeNull();
    expect(root.querySelector("[data-testid='chat-mcp-app']")).not.toBeNull();
    // It resolved the document from the discovered server (the server_id wiring).
    expect(client.reads).toContainEqual(['weather-server', 'ui://weather/main.html']);
  });

  it('does NOT mount the renderer when the message has no app ref (regression guard for an ordinary turn)', () => {
    const message = agentMessageWithApp();
    delete message.app;
    delete message.serverID;
    const root = render(MessageBubble, {
      message,
      client: noopChatClient,
      appHostClient: fakeHostClient()
    });
    expect(root.querySelector("[data-renderer-source='mcp-app']")).toBeNull();
  });

  it('does NOT mount the renderer when no host client is injected (no app→host path)', () => {
    const root = render(MessageBubble, {
      message: agentMessageWithApp(),
      client: noopChatClient
      // appHostClient intentionally absent
    });
    expect(root.querySelector("[data-renderer-source='mcp-app']")).toBeNull();
  });
});

describe('inline → fullscreen / pip drives the real layout into the real AppPanel', () => {
  function openRef(): AppRef {
    return {
      id: appId('weather-server', appView.resourceUri),
      title: 'main.html',
      serverID: 'weather-server',
      resourceUri: appView.resourceUri,
      rawHtmlTrusted: false
    };
  }

  it('a fullscreen request (real reducer) yields a region whose real AppPanel hosts the renderer', () => {
    // Drive the SHIPPED reducer + projection — no re-implemented logic.
    const model = reduceLayout(INITIAL_LAYOUT, {
      type: 'request-display-mode',
      app: openRef(),
      mode: 'fullscreen'
    });
    const region = computeRegion(model);
    expect(region.region).toBe('fullscreen');
    const fullscreenApp = region.fullscreenApp as OpenApp;
    expect(fullscreenApp?.mode).toBe('fullscreen');

    const root = render(AppPanel, {
      app: fullscreenApp,
      appHostClient: fakeHostClient(),
      onrequestmode: () => {},
      onclose: () => {}
    });
    expect(root.querySelector("[data-testid='app-panel']")?.getAttribute('data-mode')).toBe('fullscreen');
    expect(root.querySelector("[data-renderer-source='mcp-app']"), 'the page-level panel hosts the real renderer').not.toBeNull();
  });

  it('a pip request (real reducer) yields a single pip app the real AppPanel hosts', () => {
    const model = reduceLayout(INITIAL_LAYOUT, {
      type: 'request-display-mode',
      app: openRef(),
      mode: 'pip'
    });
    const region = computeRegion(model);
    expect(region.region).toBe('pip');
    const pipApp = region.pipApp as OpenApp;
    expect(pipApp?.mode).toBe('pip');

    const root = render(AppPanel, {
      app: pipApp,
      appHostClient: fakeHostClient(),
      onrequestmode: () => {},
      onclose: () => {}
    });
    expect(root.querySelector("[data-testid='app-panel']")?.getAttribute('data-mode')).toBe('pip');
    expect(root.querySelector("[data-renderer-source='mcp-app']")).not.toBeNull();
  });
});

/* ================================================================ */
/* Gap A — a HEAVY `ui://` document renders by fetching the         */
/* offloaded artifact (D-026), NOT the "exceeds threshold" error.  */
/* ================================================================ */

// A REALISTIC heavy MCP App document — modelled on a Vite single-file build
// (doctype + head with inlined CSS + a large inlined ES-module bundle in
// <body>), the shape `dockyard build` / go-study-mcp's
// `ui://go-study-mcp/studio/index.html` (86.4 KB) ships. §17.8 forbids a
// trivial stub: the fixture MUST exceed the 32 KiB heavy-content threshold so
// the offload-by-reference path is actually exercised, exactly as a real App
// bundle does. The marker lets the test prove the iframe srcdoc was sourced
// from the FETCHED bytes (not synthesised).
const HEAVY_MARKER = 'HARBOR_HEAVY_APP_MARKER';
function makeHeavyAppHTML(): string {
  // A chunk of realistic minified-bundle-looking JS, repeated until the
  // document clears the 32 KiB threshold (a real Svelte runtime + component
  // tree minifies to well over that).
  const chunk =
    'const $$=(a,b)=>a==null?b:a;function mount(t,p){const c=new Comp({target:t,props:p});return c}' +
    'class Comp{constructor(o){this.t=o.target;this.p=o.props;this.render()}render(){this.t.innerHTML=`<section class="studio"><header>Go Study</header><main></main></section>`}}';
  let bundle = '';
  while (bundle.length < 40 * 1024) {
    bundle += chunk;
  }
  return (
    '<!doctype html><html lang="en"><head><meta charset="utf-8">' +
    `<title>Go Study Studio</title><style>:root{color-scheme:light dark}.studio{font-family:system-ui;padding:1rem}/*${HEAVY_MARKER}*/</style>` +
    '</head><body><div id="app"></div>' +
    `<script type="module">${bundle}\nmount(document.getElementById('app'),{});</script>` +
    '</body></html>'
  );
}

// A host client whose document read OFFLOADS the document by reference (the
// D-026 heavy path) instead of returning inline `content`, and whose
// resolveArtifact hands back a presigned URL. Records both legs so the test
// asserts the renderer walked artifactRef → resolveArtifact → fetch.
function heavyHostClient(): MCPAppHostClient & { resolved: string[] } {
  const resolved: string[] = [];
  return {
    resolved,
    async readResource(_serverID, resourceURI) {
      return {
        resourceUri: resourceURI,
        mimeType: 'text/html',
        artifactRef: { id: 'art_studio_abc', mimeType: 'text/html', sizeBytes: 88_500 }
      };
    },
    async readRenderDocument(_serverID, resourceURI) {
      return {
        resourceUri: resourceURI,
        mimeType: 'text/html',
        artifactRef: { id: 'art_studio_abc', mimeType: 'text/html', sizeBytes: 88_500 }
      };
    },
    async callTool(_serverID, tool) {
      return { tool, content: { ok: true }, isError: false };
    },
    async listResources() {
      return [];
    },
    async listResourceTemplates() {
      return [];
    },
    async listTools() {
      return [];
    },
    async resolveArtifact(id) {
      resolved.push(id);
      return `https://artifacts.example/${id}`;
    },
    async toolContext() {
      return null;
    },
    async fetchArtifactText() {
      return '{}';
    }
  };
}

describe('Gap A — heavy MCP-App document renders via the offloaded artifact', () => {
  it('fetches the artifact bytes and loads them into the sandboxed srcdoc (no "exceeds threshold" error)', async () => {
    const heavyHTML = makeHeavyAppHTML();
    // The fixture is genuinely heavy — the property the whole fix turns on.
    expect(heavyHTML.length).toBeGreaterThan(32 * 1024);

    const fetchSpy = vi.fn(async (url: string) => ({
      ok: true,
      status: 200,
      async text() {
        return url === 'https://artifacts.example/art_studio_abc' ? heavyHTML : '';
      }
    }));
    vi.stubGlobal('fetch', fetchSpy);

    const client = heavyHostClient();
    const root = render(MessageBubble, {
      message: agentMessageWithApp(),
      client: noopChatClient,
      appHostClient: client,
      availableDisplayModes: ['inline', 'fullscreen', 'pip']
    });

    // The renderer resolves the by-reference stub and loads the FETCHED bytes
    // into the iframe srcdoc — the inline-path destination, just a different
    // content source. If the artifactRef branch reverts to throwing
    // "exceeds the inline heavy-content threshold", loadState flips to 'error',
    // the iframe never mounts, srcdoc stays empty, and this waitFor times out.
    await vi.waitFor(
      () => {
        flushSync();
        const iframe = root.querySelector<HTMLIFrameElement>('iframe.mcp-app__frame');
        expect(iframe, 'the sandboxed app iframe mounted').not.toBeNull();
        expect(iframe?.getAttribute('srcdoc') ?? '').toContain(HEAVY_MARKER);
      },
      { timeout: 30_000 }
    );

    // The error path is GONE — the gap-A bug ("server bug" refusal) is fixed.
    expect(root.querySelector("[data-state='error']")).toBeNull();
    // Proof the renderer walked artifactRef → resolveArtifact → fetch.
    expect(client.resolved).toContain('art_studio_abc');
    expect(fetchSpy).toHaveBeenCalledWith('https://artifacts.example/art_studio_abc');
  }, 30_000);

  it('inline-path regression: a below-threshold document still loads from inline content', async () => {
    const root = render(MessageBubble, {
      message: agentMessageWithApp(),
      client: noopChatClient,
      appHostClient: fakeHostClient(),
      availableDisplayModes: ['inline', 'fullscreen', 'pip']
    });
    await vi.waitFor(
      () => {
        flushSync();
        const iframe = root.querySelector<HTMLIFrameElement>('iframe.mcp-app__frame');
        expect(iframe?.getAttribute('srcdoc') ?? '').toContain('<p>app body</p>');
      },
      { timeout: 30_000 }
    );
    expect(root.querySelector("[data-state='error']")).toBeNull();
  }, 30_000);
});

/* ================================================================ */
/* Gap B — the host-side operator "pop to side-by-side" affordance.  */
/* ================================================================ */

describe('Gap B — operator expand affordance pops the inline app to side-by-side', () => {
  it('dispatches a pip display-mode request through the injected callback (never the page)', async () => {
    const requests: DisplayModeRequest[] = [];
    const root = render(MessageBubble, {
      message: agentMessageWithApp(),
      client: noopChatClient,
      appHostClient: fakeHostClient(),
      availableDisplayModes: ['inline', 'fullscreen', 'pip'],
      onAppDisplayModeRequest: (req: DisplayModeRequest) => requests.push(req)
    });

    // The affordance shows once the inline app is ready.
    let btn: HTMLButtonElement | null = null;
    await vi.waitFor(
      () => {
        flushSync();
        btn = root.querySelector<HTMLButtonElement>("[data-testid='mcp-app-expand-pip']");
        expect(btn, 'the expand-to-side-by-side button is present on the inline frame').not.toBeNull();
      },
      { timeout: 30_000 }
    );
    expect(btn!.getAttribute('aria-label')).toBe('Pop app to side-by-side');

    btn!.click();
    flushSync();

    // The request rode the SAME injected seam an app-initiated request uses —
    // host-granted directly because the host advertised it can apply pip.
    expect(requests).toEqual([{ requested: 'pip', granted: 'pip' }]);

    // End-to-end: feeding that request into the REAL layout reducer (exactly
    // as the page's onInlineAppDisplayModeRequest does) moves the app to pip.
    const ref: AppRef = {
      id: appId('weather-server', appView.resourceUri),
      title: 'main.html',
      serverID: 'weather-server',
      resourceUri: appView.resourceUri,
      rawHtmlTrusted: false
    };
    const region = computeRegion(
      reduceLayout(INITIAL_LAYOUT, {
        type: 'request-display-mode',
        app: ref,
        mode: requests[0].granted
      })
    );
    expect(region.region).toBe('pip');
    expect((region.pipApp as OpenApp)?.mode).toBe('pip');
  }, 30_000);

  it('does NOT show the expand affordance when no non-inline mode is available (inline-only host)', async () => {
    const root = render(MessageBubble, {
      message: agentMessageWithApp(),
      client: noopChatClient,
      appHostClient: fakeHostClient(),
      availableDisplayModes: ['inline'],
      onAppDisplayModeRequest: () => {}
    });
    await vi.waitFor(
      () => {
        flushSync();
        expect(root.querySelector('iframe.mcp-app__frame'), 'app mounted').not.toBeNull();
      },
      { timeout: 30_000 }
    );
    expect(root.querySelector("[data-testid='mcp-app-expand-pip']")).toBeNull();
    expect(root.querySelector("[data-testid='mcp-app-expand-bar']")).toBeNull();
  }, 30_000);
});
