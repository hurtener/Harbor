// Host-identity + theme injection + host-context (theme / styles.variables) +
// Data-Delivery tests for the MCP Apps host wrapper.
//
// D-091 module-encapsulation hardening: the host identity (`ui/initialize`
// name/version), theme, and style variables are injected through
// `AppBridgeHostOptions`, not baked into the module — so a second framework
// surface mounting the chat module advertises its own identity/theme. These
// tests mock the official `AppBridge` to capture its constructor arguments AND
// its post-init sender calls (`setHostContext` / `sendToolInput` /
// `sendToolResult` / `close`), asserting: (1) the default preserves the prior
// Console behaviour; (2) an injected hostInfo/theme/styles flows into the
// `ui/initialize` host-context; (3) after `oninitialized` the host delivers the
// captured tool input then result; and (4) a live theme relay patches the
// LIVE bridge via `setHostContext` and NEVER calls `close` (the reverted-work
// handshake break).

import { describe, expect, it, vi } from 'vitest';
import type {
  McpUiHostContext,
  McpUiStyleVariableKey,
  McpUiStyles,
} from '@modelcontextprotocol/ext-apps/app-bridge';

import type { MCPAppHostClient, MCPAppToolContext } from './app-bridge-host.js';

// A mock `AppBridge` that records constructor args AND exposes every recorded
// instance so a test can fire `oninitialized` and inspect the sender calls.
interface MockBridge {
  oninitialized?: (params: unknown) => void;
  setHostContextCalls: McpUiHostContext[];
  toolInputCalls: unknown[];
  toolResultCalls: unknown[];
  closeCalls: number;
  fireInitialized: () => void;
}

// Capture every `new AppBridge(...)` argument list + instance. `vi.hoisted` so
// the mock factory (hoisted above the imports) can close over it.
const captured = vi.hoisted(() => ({
  ctorArgs: [] as unknown[][],
  instances: [] as MockBridge[],
}));

vi.mock('@modelcontextprotocol/ext-apps/app-bridge', () => {
  class AppBridge {
    oncalltool: unknown;
    onreadresource: unknown;
    onlistresources: unknown;
    onrequestdisplaymode: unknown;
    oninitialized: ((params: unknown) => void) | undefined;
    setHostContextCalls: McpUiHostContext[] = [];
    toolInputCalls: unknown[] = [];
    toolResultCalls: unknown[] = [];
    closeCalls = 0;
    constructor(...args: unknown[]) {
      captured.ctorArgs.push(args);
      captured.instances.push(this as unknown as MockBridge);
    }
    async connect(): Promise<void> {}
    async close(): Promise<void> {
      this.closeCalls += 1;
    }
    setHostContext(ctx: McpUiHostContext): void {
      this.setHostContextCalls.push(ctx);
    }
    async sendToolInput(p: unknown): Promise<void> {
      this.toolInputCalls.push(p);
    }
    async sendToolResult(p: unknown): Promise<void> {
      this.toolResultCalls.push(p);
    }
    fireInitialized(): void {
      this.oninitialized?.({});
    }
  }
  class PostMessageTransport {
    constructor(..._args: unknown[]) {}
  }
  return { AppBridge, PostMessageTransport };
});

// Imported AFTER the mock declaration; vitest hoists vi.mock above this.
const { AppBridgeHost, DEFAULT_HOST_INFO } = await import('./app-bridge-host.js');

/** Flush the microtask queue so the async delivery chain settles. */
async function flushMicrotasks(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
  await Promise.resolve();
}

function fakeClient(): MCPAppHostClient {
  return {
    async readResource(_s, uri) {
      return { resourceUri: uri, mimeType: 'text/html', content: '' };
    },
    async callTool(tool) {
      return { tool, content: {}, isError: false };
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

describe('AppBridgeHost host-identity + theme injection', () => {
  it('defaults to the Console host identity and dark theme when not injected', () => {
    captured.ctorArgs.length = 0;
    new AppBridgeHost({ client: fakeClient(), serverID: 'srv' });
    const [, hostInfo, , extra] = captured.ctorArgs[0] as [
      null,
      { name: string; version: string },
      unknown,
      { hostContext: { theme: string } },
    ];
    expect(hostInfo).toEqual(DEFAULT_HOST_INFO);
    expect(hostInfo.name).toBe('harbor-console');
    expect(extra.hostContext.theme).toBe('dark');
  });

  it('advertises the INJECTED host identity and theme through the seam', () => {
    captured.ctorArgs.length = 0;
    const hostInfo = { name: 'harbor-dev-ui', version: '9.9' };
    new AppBridgeHost({ client: fakeClient(), serverID: 'srv', hostInfo, theme: 'light' });
    const [, advertised, , extra] = captured.ctorArgs[0] as [
      null,
      { name: string; version: string },
      unknown,
      { hostContext: { theme: string } },
    ];
    expect(advertised).toEqual(hostInfo);
    expect(advertised.name).not.toBe('harbor-console');
    expect(extra.hostContext.theme).toBe('light');
  });
});

describe('AppBridgeHost host-context — theme + styles.variables (HA-35)', () => {
  it('bakes the injected styles.variables into the ui/initialize host-context', () => {
    captured.ctorArgs.length = 0;
    captured.instances.length = 0;
    // A typed subset of the ext-apps McpUiStyleVariableKey union — a wrong key
    // here fails svelte-check (the §17.8 mechanical guard).
    const variables: Partial<Record<McpUiStyleVariableKey, string>> = {
      '--color-background-primary': '#0d1117',
      '--color-text-primary': '#e6edf3',
      '--color-border-primary': '#30363d',
    };
    new AppBridgeHost({
      client: fakeClient(),
      serverID: 'srv',
      theme: 'dark',
      // `variables` is typed as the closed key union for the wrong-key guard;
      // the vendored `McpUiStyles` is a full Record, so a subset is cast.
      styles: { variables: variables as McpUiStyles },
    });
    const [, , , extra] = captured.ctorArgs[0] as [
      null,
      unknown,
      unknown,
      { hostContext: McpUiHostContext },
    ];
    expect(extra.hostContext.theme).toBe('dark');
    expect(extra.hostContext.styles?.variables).toEqual(variables);
  });

  it('omits styles from the host-context when none are injected', () => {
    captured.ctorArgs.length = 0;
    new AppBridgeHost({ client: fakeClient(), serverID: 'srv' });
    const [, , , extra] = captured.ctorArgs[0] as [
      null,
      unknown,
      unknown,
      { hostContext: McpUiHostContext },
    ];
    expect(extra.hostContext.styles).toBeUndefined();
  });

  it('relays a live theme change onto the LIVE bridge via setHostContext (after init) and NEVER closes it', () => {
    captured.instances.length = 0;
    const host = new AppBridgeHost({ client: fakeClient(), serverID: 'srv', theme: 'dark' });
    const bridge = captured.instances[0];

    // Before init: a setHostContext patch is gated (dropped), never posted onto
    // a bridge that has not finished ui/initialize.
    host.setHostContext({ theme: 'light' });
    expect(bridge.setHostContextCalls).toHaveLength(0);

    // App completes ui/initialize.
    bridge.fireInitialized();
    expect(host.isInitialized).toBe(true);

    // Now the live theme relay patches the SAME bridge — no reconstruction, no
    // close (the reverted-work break was a close() mid/after handshake).
    host.setHostContext({ theme: 'light', styles: { variables: {} as McpUiStyles } });
    expect(bridge.setHostContextCalls).toHaveLength(1);
    expect(bridge.setHostContextCalls[0].theme).toBe('light');
    expect(bridge.closeCalls).toBe(0);
    // The bridge instance is never swapped for a fresh one on a theme change.
    expect(captured.instances).toHaveLength(1);
  });
});

describe('AppBridgeHost Data Delivery — sendToolInput then sendToolResult (HA / D-226)', () => {
  it('after oninitialized, delivers input then result from the captured tool context', async () => {
    captured.instances.length = 0;
    const ctx: MCPAppToolContext = {
      tool: 'srv_report',
      input: { content: { region: 'emea' } },
      result: { content: { revenue: 42 } },
      isError: false,
    };
    const host = new AppBridgeHost({ client: fakeClient(), serverID: 'srv', toolContext: ctx });
    const bridge = captured.instances[0];

    // No delivery before the app initializes.
    expect(bridge.toolInputCalls).toHaveLength(0);

    // The app can only send `ui/notifications/initialized` over a CONNECTED
    // transport, so connect precedes the handshake (as it does in the renderer).
    await host.connect({} as unknown as Window);
    bridge.fireInitialized();
    await flushMicrotasks();

    expect(bridge.toolInputCalls).toEqual([{ arguments: { region: 'emea' } }]);
    expect(bridge.toolResultCalls).toHaveLength(1);
    const result = bridge.toolResultCalls[0] as { structuredContent?: unknown; isError?: boolean };
    expect(result.structuredContent).toEqual({ revenue: 42 });
    expect(result.isError).toBe(false);
    // Ordering: input strictly precedes result (the SDK lifecycle order).
    expect(bridge.toolInputCalls).toHaveLength(1);
  });

  it('performs no delivery when no tool context was supplied', async () => {
    // The renderer resolves the context BEFORE constructing the host, so an
    // absent `toolContext` means "nothing was ever captured for this app" —
    // not "the lookup failed". A resolution MISS never reaches a constructed
    // bridge at all: the renderer shows the placeholder and mounts no iframe
    // (pinned by the mcp-app renderer spec).
    captured.instances.length = 0;
    new AppBridgeHost({ client: fakeClient(), serverID: 'srv' });
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    await flushMicrotasks();
    expect(bridge.toolInputCalls).toHaveLength(0);
    expect(bridge.toolResultCalls).toHaveLength(0);
  });

  it('drops a stale delivery when the bridge is closed mid heavy-payload fetch (no "Not connected")', async () => {
    // A transcript re-render can `close()` the bridge while a heavy
    // by-reference half of the context is being fetched at the iframe edge
    // between the two sends. The resumed delivery must NOT push onto the
    // torn-down transport (that threw "Not connected" and spammed the console
    // on failing turns). This is the surviving shape of the original
    // in-flight-close race: the tool-context FETCH now happens before the
    // bridge exists, so only the artifact fetch can still be in flight.
    captured.instances.length = 0;
    let releaseBytes: (v: string) => void = () => {};
    const gate = new Promise<string>((resolve) => {
      releaseBytes = resolve;
    });
    const client: MCPAppHostClient = { ...fakeClient(), async fetchArtifactText() { return gate; } };
    const ctx: MCPAppToolContext = {
      tool: 'srv_report',
      input: { artifactRef: { id: 'art_heavy' } },
      result: { content: {} },
      isError: false,
    };
    const host = new AppBridgeHost({ client, serverID: 'srv', toolContext: ctx });
    const bridge = captured.instances[0];

    await host.connect({} as unknown as Window);
    bridge.fireInitialized(); // starts #deliverToolContext → awaits the gated fetch
    await flushMicrotasks();
    expect(bridge.toolInputCalls).toHaveLength(0);

    await host.close(); // transport torn down while the fetch is still parked
    releaseBytes('{"region":"emea"}');
    await flushMicrotasks();

    // The stale delivery is dropped — never sent onto the closed transport.
    expect(bridge.toolInputCalls).toHaveLength(0);
    expect(bridge.toolResultCalls).toHaveLength(0);
  });

  it('drops the RESULT send when the bridge is closed mid heavy-result fetch', async () => {
    // The mirror of the case above, on the other side of the input send. The
    // heavy half is far more often the RESULT (the larger payload), and it is
    // fetched AFTER `sendToolInput` has already gone out — so it exercises the
    // SECOND liveness re-check, which the input-parked case never reaches.
    captured.instances.length = 0;
    let releaseBytes: (v: string) => void = () => {};
    const gate = new Promise<string>((resolve) => {
      releaseBytes = resolve;
    });
    const client: MCPAppHostClient = { ...fakeClient(), async fetchArtifactText() { return gate; } };
    const ctx: MCPAppToolContext = {
      tool: 'srv_report',
      input: { content: { region: 'emea' } },
      result: { artifactRef: { id: 'art_heavy_result' } },
      isError: false,
    };
    const host = new AppBridgeHost({ client, serverID: 'srv', toolContext: ctx });
    const bridge = captured.instances[0];

    await host.connect({} as unknown as Window);
    bridge.fireInitialized();
    await flushMicrotasks();

    // The INPUT went out (it was inline); the delivery is now parked on the
    // heavy result fetch.
    expect(bridge.toolInputCalls).toEqual([{ arguments: { region: 'emea' } }]);
    expect(bridge.toolResultCalls).toHaveLength(0);

    await host.close(); // transport torn down mid result-bytes fetch
    releaseBytes('{"revenue":42}');
    await flushMicrotasks();

    // The result send is dropped — never posted onto the closed transport.
    expect(bridge.toolResultCalls).toHaveLength(0);
  });
});
