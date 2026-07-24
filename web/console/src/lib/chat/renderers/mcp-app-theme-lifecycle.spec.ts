// The reverted-work regression guard, at the component level.
//
// The MCP Apps handshake break that got the original live-theme wiring reverted
// was a REACTIVE theme threaded into the Svelte `$effect` that owns the bridge
// lifecycle: the effect's cleanup re-ran on the theme change and called
// `host.close()`, tearing the transport down MID-handshake. This spec mounts
// the REAL `McpAppRenderer` with a mocked `AppBridge`, drives an OS
// `prefers-color-scheme` change, and asserts the fix holds:
//
//   - a theme change NEVER calls `bridge.close()` and NEVER constructs a second
//     bridge (no teardown-rebuild);
//   - the change is relayed onto the LIVE bridge via `setHostContext` — and
//     only AFTER the bridge reports initialized (gated).
//
// The security / handshake-over-a-real-iframe fidelity lives in the Playwright
// spec (`tests/mcp-app-host-handshake.spec.ts`), which drives the REAL vendored
// App client. This spec owns the OUR-code lifecycle-isolation assertion.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { McpUiHostContext } from '@modelcontextprotocol/ext-apps/app-bridge';

import type { DisplayModeRequest, MCPAppHostClient } from './app-bridge-host.js';

interface MockBridge {
  oninitialized?: (params: unknown) => void;
  setHostContextCalls: McpUiHostContext[];
  closeCalls: number;
  fireInitialized: () => void;
}

const captured = vi.hoisted(() => ({ instances: [] as MockBridge[] }));

vi.mock('@modelcontextprotocol/ext-apps/app-bridge', () => {
  class AppBridge {
    oncalltool: unknown;
    onreadresource: unknown;
    onlistresources: unknown;
    onrequestdisplaymode: unknown;
    oninitialized: ((params: unknown) => void) | undefined;
    setHostContextCalls: McpUiHostContext[] = [];
    closeCalls = 0;
    constructor(..._args: unknown[]) {
      captured.instances.push(this as unknown as MockBridge);
    }
    async connect(): Promise<void> {}
    async close(): Promise<void> {
      this.closeCalls += 1;
    }
    setHostContext(ctx: McpUiHostContext): void {
      this.setHostContextCalls.push(ctx);
    }
    async sendToolInput(): Promise<void> {}
    async sendToolResult(): Promise<void> {}
    fireInitialized(): void {
      this.oninitialized?.({});
    }
  }
  class PostMessageTransport {
    constructor(..._args: unknown[]) {}
  }
  return { AppBridge, PostMessageTransport };
});

const McpAppRenderer = (await import('./mcp-app.svelte')).default;
const { mountRendererReactive } = await import('./mcp-app-harness.svelte.js');

// A controllable `prefers-color-scheme` MediaQueryList: the test flips `matches`
// and dispatches a `change` event to the registered listeners.
function installMatchMedia(initialDark: boolean): { setDark: (dark: boolean) => void } {
  let matches = initialDark;
  const listeners = new Set<(e: MediaQueryListEvent) => void>();
  const mql = {
    get matches() {
      return matches;
    },
    media: '(prefers-color-scheme: dark)',
    addEventListener: (_type: string, cb: (e: MediaQueryListEvent) => void) => listeners.add(cb),
    removeEventListener: (_type: string, cb: (e: MediaQueryListEvent) => void) => listeners.delete(cb),
  } as unknown as MediaQueryList;
  (window as unknown as { matchMedia: (q: string) => MediaQueryList }).matchMedia = () => mql;
  return {
    setDark(dark: boolean) {
      matches = dark;
      for (const cb of listeners) cb({ matches: dark } as MediaQueryListEvent);
    },
  };
}

function fakeClient(): MCPAppHostClient {
  return {
    async readResource(_s, uri) {
      return { resourceUri: uri, mimeType: 'text/html', content: '<p>app body</p>' };
    },
    async callTool(tool) {
      return { tool, content: {}, isError: false };
    },
    async listResources() {
      return [];
    },
    async listTools() {
      return [];
    },
    async resolveArtifact(id) {
      return `blob:${id}`;
    },
    async toolContext() {
      return null;
    },
    async fetchArtifactText() {
      return '{}';
    },
  };
}

/** Mount, then drain the preload + lifecycle effects so the bridge connects. */
async function mountAndConnect(target: HTMLElement): Promise<ReturnType<typeof mount>> {
  const component = mount(McpAppRenderer, {
    target,
    props: {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app: { resourceUri: 'ui://srv/app.html', displayMode: 'inline', rawHtmlTrusted: false },
      serverID: 'srv',
      appHostClient: fakeClient(),
    },
  });
  // Drain: preload (async readResource) → loadState='ready' → lifecycle effect
  // constructs + connects the bridge.
  for (let i = 0; i < 8 && captured.instances.length === 0; i++) {
    flushSync();
    await Promise.resolve();
  }
  flushSync();
  return component;
}

describe('McpAppRenderer — theme lifecycle isolation (reverted-work guard)', () => {
  afterEach(() => {
    captured.instances.length = 0;
  });

  it('a theme change NEVER closes or reconstructs the bridge — it relays via setHostContext', async () => {
    const media = installMatchMedia(true); // start dark
    const target = document.createElement('div');
    document.body.appendChild(target);

    const component = await mountAndConnect(target);
    expect(captured.instances).toHaveLength(1);
    const bridge = captured.instances[0];

    // The bridge reports it finished ui/initialize.
    bridge.fireInitialized();
    flushSync();

    // OS flips to light MID-session.
    media.setDark(false);
    flushSync();

    // The fix: no teardown, no rebuild — exactly ONE bridge, close never called.
    expect(captured.instances).toHaveLength(1);
    expect(bridge.closeCalls).toBe(0);
    // The change was relayed onto the LIVE bridge.
    expect(bridge.setHostContextCalls.length).toBeGreaterThanOrEqual(1);
    expect(bridge.setHostContextCalls.at(-1)?.theme).toBe('light');

    unmount(component);
    document.body.removeChild(target);
  });

  it('a theme change BEFORE init does not patch the bridge (gated) and still never closes it', async () => {
    const media = installMatchMedia(true);
    const target = document.createElement('div');
    document.body.appendChild(target);

    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];

    // Flip theme while the bridge is NOT yet initialized.
    media.setDark(false);
    flushSync();

    // AppBridgeHost.setHostContext gates on init, so no patch is posted and the
    // bridge is never closed/rebuilt.
    expect(bridge.setHostContextCalls).toHaveLength(0);
    expect(bridge.closeCalls).toBe(0);
    expect(captured.instances).toHaveLength(1);

    unmount(component);
    document.body.removeChild(target);
  });

  it('churning a lifecycle-effect input prop (onDisplayModeRequest) NEVER re-runs the effect — no close, one bridge', async () => {
    // Defense-in-depth for the exact outage class: the bridge lifecycle effect
    // must depend ONLY on loadState + iframeEl. `onDisplayModeRequest` is read
    // by `connectBridge` (a lifecycle-effect input) but NOT by `preload`, so it
    // isolates the concern: if the effect tracked the prop, reassigning its
    // identity would re-run the effect → tear the transport down (host.close) +
    // rebuild — the reverted-work break. The untracked snapshot means it can't.
    // Mutation check: revert the untrack snapshot and this test fails (close=1,
    // two bridges).
    installMatchMedia(true);
    const target = document.createElement('div');
    document.body.appendChild(target);

    const { component, props } = mountRendererReactive(target, {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app: { resourceUri: 'ui://srv/app.html', displayMode: 'inline', rawHtmlTrusted: false },
      serverID: 'srv',
      appHostClient: fakeClient(),
      availableDisplayModes: ['inline', 'fullscreen', 'pip'],
      onDisplayModeRequest: () => {},
    });
    // Drain preload → ready → connectBridge.
    for (let i = 0; i < 8 && captured.instances.length === 0; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();
    expect(captured.instances).toHaveLength(1);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    // Churn the prop identities the lifecycle effect used to track through
    // connectBridge — a brand-new callback reference and a new array.
    props.onDisplayModeRequest = (_req: DisplayModeRequest) => {};
    flushSync();
    await Promise.resolve();
    flushSync();
    props.availableDisplayModes = ['inline'];
    flushSync();
    await Promise.resolve();
    flushSync();

    // The effect did not re-run: no teardown, still exactly one bridge.
    expect(bridge.closeCalls).toBe(0);
    expect(captured.instances).toHaveLength(1);

    unmount(component);
    document.body.removeChild(target);
  });
});
