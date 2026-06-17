// Phase 109b — the MCP Apps host wrapper unit + integration tests.
//
// Covers the D-173 invariants that do not need a real browser: the sandbox /
// CSP constants, the postMessage origin/source guard, the manual handlers
// (each dispatches to the injected Protocol surface, NEVER a direct
// transport), and the AppBridge wrapper's manual-handler mode. The real-iframe
// sandbox-escape + handshake assertions live in the Playwright spec
// (`tests/mcp-app-host.spec.ts`).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// The Data Delivery push (`sendToolInput` / `sendToolResult`) is driven by the
// AppBridge `oninitialized` callback. The real bridge only fires that after a
// live `ui/notifications/initialized` over a postMessage transport (the
// Playwright spec covers that path). To drive the push deterministically in a
// unit test we replace the bridge with a fake that captures the handler
// assignments + records the push order; the fake constructor opens NO network
// connection, so the D-173 no-transport tests below still hold.
const bridgeHooks = vi.hoisted(() => {
  const instances: Array<{
    oninitialized?: () => void;
    onlistresourcetemplates?: () => Promise<unknown>;
    pushOrder: string[];
    hostContext: Record<string, unknown> | undefined;
    listeners: Record<string, Array<(p: unknown) => void>>;
    emit: (event: string, params: unknown) => void;
    sendToolInput: ReturnType<typeof vi.fn>;
    sendToolResult: ReturnType<typeof vi.fn>;
    setHostContext: ReturnType<typeof vi.fn>;
    teardownResource: ReturnType<typeof vi.fn>;
    close: ReturnType<typeof vi.fn>;
  }> = [];
  return { instances };
});

vi.mock('@modelcontextprotocol/ext-apps/app-bridge', () => {
  class FakeAppBridge {
    oncalltool: unknown;
    onreadresource: unknown;
    onlistresources: unknown;
    onlistresourcetemplates: (() => Promise<unknown>) | undefined;
    onrequestdisplaymode: unknown;
    oninitialized: (() => void) | undefined;
    pushOrder: string[] = [];
    hostContext: Record<string, unknown> | undefined;
    listeners: Record<string, Array<(p: unknown) => void>> = {};
    sendToolInput = vi.fn(async () => {
      this.pushOrder.push('input');
    });
    sendToolResult = vi.fn(async () => {
      this.pushOrder.push('result');
    });
    setHostContext = vi.fn((_ctx: unknown) => {
      this.pushOrder.push('setHostContext');
    });
    teardownResource = vi.fn(async () => {
      this.pushOrder.push('teardown');
      return {};
    });
    connect = vi.fn(async () => {});
    close = vi.fn(async () => {
      this.pushOrder.push('close');
    });
    addEventListener(event: string, handler: (p: unknown) => void): void {
      (this.listeners[event] ??= []).push(handler);
    }
    /** Test helper: fire a notification event to the registered listeners. */
    emit(event: string, params: unknown): void {
      for (const h of this.listeners[event] ?? []) h(params);
    }
    constructor(
      _client: unknown,
      _hostInfo: unknown,
      _caps: unknown,
      options?: { hostContext?: Record<string, unknown> },
    ) {
      this.hostContext = options?.hostContext;
      bridgeHooks.instances.push(this);
    }
  }
  class FakePostMessageTransport {
    constructor(..._args: unknown[]) {}
  }
  return { AppBridge: FakeAppBridge, PostMessageTransport: FakePostMessageTransport };
});

import {
  APP_IFRAME_SANDBOX_BASE,
  AppBridgeHost,
  appIframeSandbox,
  assertSandboxTokensSafe,
  buildAppCSP,
  createAppHandlers,
  isTrustedAppMessage,
  wrapAppDocument,
  type MCPAppHostClient,
  type MCPAppResource,
  type MCPAppToolContext,
  type MCPAppToolResult,
} from './app-bridge-host.js';

/* ------------------------------------------------------------------ */
/* A fake injected Protocol surface that records every call and opens   */
/* no network connection of any kind.                                   */
/* ------------------------------------------------------------------ */

interface FakeCalls {
  readResource: Array<[string, string]>;
  callTool: Array<[string, unknown]>;
  listResources: string[];
  listResourceTemplates: string[];
  listTools: string[];
  toolContext: Array<[string, string]>;
  fetchArtifactText: string[];
}

function makeFakeClient(overrides: Partial<MCPAppHostClient> = {}): {
  client: MCPAppHostClient;
  calls: FakeCalls;
} {
  const calls: FakeCalls = {
    readResource: [],
    callTool: [],
    listResources: [],
    listResourceTemplates: [],
    listTools: [],
    toolContext: [],
    fetchArtifactText: [],
  };
  const client: MCPAppHostClient = {
    async readResource(serverID, uri): Promise<MCPAppResource> {
      calls.readResource.push([serverID, uri]);
      return { resourceUri: uri, mimeType: 'text/html', content: '<p>hi</p>' };
    },
    async callTool(tool, args): Promise<MCPAppToolResult> {
      calls.callTool.push([tool, args]);
      return { tool, content: { ok: true }, isError: false };
    },
    async listResources(serverID) {
      calls.listResources.push(serverID);
      return [{ uri: 'ui://srv/app.html', name: 'app', mimeType: 'text/html' }];
    },
    async listResourceTemplates(serverID) {
      calls.listResourceTemplates.push(serverID);
      return [];
    },
    async listTools(serverID) {
      calls.listTools.push(serverID);
      return [{ name: 'srv_echo', description: 'echo' }];
    },
    async resolveArtifact(id) {
      return `https://artifacts.example/${id}`;
    },
    async toolContext(serverID, toolCallID): Promise<MCPAppToolContext | null> {
      calls.toolContext.push([serverID, toolCallID]);
      return {
        tool: 'srv_echo',
        input: { content: { q: 1 } },
        result: { content: { ok: true } },
        isError: false,
      };
    },
    async fetchArtifactText(id) {
      calls.fetchArtifactText.push(id);
      return '{}';
    },
    ...overrides,
  };
  return { client, calls };
}

/** Flush enough microtasks for the fire-and-forget delivery chain to settle. */
async function flush(): Promise<void> {
  for (let i = 0; i < 8; i++) {
    await Promise.resolve();
  }
}

describe('sandbox + CSP constants', () => {
  it('untrusted sandbox is allow-scripts only — never allow-same-origin', () => {
    const tokens = appIframeSandbox(false);
    expect(tokens).toBe(APP_IFRAME_SANDBOX_BASE);
    expect(tokens.split(/\s+/)).not.toContain('allow-same-origin');
  });

  it('trusted sandbox relaxes interaction tokens but never adds allow-same-origin', () => {
    const tokens = appIframeSandbox(true);
    expect(tokens.split(/\s+/)).toContain('allow-scripts');
    expect(tokens.split(/\s+/)).toContain('allow-forms');
    expect(tokens.split(/\s+/)).not.toContain('allow-same-origin');
  });

  it('assertSandboxTokensSafe throws on the forbidden allow-same-origin token', () => {
    expect(() => assertSandboxTokensSafe('allow-scripts allow-same-origin')).toThrow(
      /allow-same-origin is forbidden/,
    );
    expect(() => assertSandboxTokensSafe('allow-scripts')).not.toThrow();
  });

  it('CSP forbids the app opening its own transport (connect-src none) in both states', () => {
    expect(buildAppCSP(false)).toContain("connect-src 'none'");
    expect(buildAppCSP(true)).toContain("connect-src 'none'");
    expect(buildAppCSP(false)).toContain("default-src 'none'");
  });

  it('trusted CSP relaxes media/style sources but keeps connect-src none', () => {
    const trusted = buildAppCSP(true);
    expect(trusted).toContain('img-src data: blob: https:');
    expect(trusted).toContain("connect-src 'none'");
    const untrusted = buildAppCSP(false);
    expect(untrusted).toContain('img-src data: blob:');
    expect(untrusted).not.toContain('https:');
  });

  it('wrapAppDocument injects the CSP meta into <head>', () => {
    const csp = buildAppCSP(false);
    const wrapped = wrapAppDocument('<html><head><title>x</title></head><body>y</body></html>', csp);
    expect(wrapped).toContain('http-equiv="Content-Security-Policy"');
    expect(wrapped.indexOf('Content-Security-Policy')).toBeLessThan(wrapped.indexOf('<title>'));
  });

  it('wrapAppDocument synthesises a head when the document has none', () => {
    const wrapped = wrapAppDocument('<p>bare</p>', buildAppCSP(false));
    expect(wrapped).toContain('http-equiv="Content-Security-Policy"');
    expect(wrapped).toContain('<p>bare</p>');
  });
});

describe('isTrustedAppMessage — origin / source validation', () => {
  const expectedSource = {} as unknown as MessageEventSource;

  it('accepts a message from the expected source + origin', () => {
    expect(
      isTrustedAppMessage({ source: expectedSource, origin: 'null' }, { source: expectedSource, origin: 'null' }),
    ).toBe(true);
  });

  it('rejects a foreign source window', () => {
    const foreign = {} as unknown as MessageEventSource;
    expect(
      isTrustedAppMessage({ source: foreign, origin: 'null' }, { source: expectedSource, origin: 'null' }),
    ).toBe(false);
  });

  it('rejects a foreign origin even from the expected window', () => {
    expect(
      isTrustedAppMessage(
        { source: expectedSource, origin: 'https://evil.example' },
        { source: expectedSource, origin: 'null' },
      ),
    ).toBe(false);
  });

  it('rejects when no peer is expected yet (null source)', () => {
    expect(isTrustedAppMessage({ source: expectedSource, origin: 'null' }, { source: null, origin: 'null' })).toBe(
      false,
    );
  });
});

describe('manual handlers dispatch to the injected client', () => {
  it('oncalltool prefixes the bare tool name with the serverID and maps the result', async () => {
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    // The app supplies a BARE server-side tool name; the host prefixes it with
    // the bridge's serverID before dispatch (catalog keys are `<source>_<tool>`).
    const result = await handlers.oncalltool({ name: 'echo', arguments: { q: 1 } });
    expect(calls.callTool).toEqual([['srv_echo', { q: 1 }]]);
    expect(result.isError).toBe(false);
    expect(result.structuredContent).toEqual({ ok: true });
  });

  it('oncalltool confines an app to its OWN server — a cross-server bare name cannot reach another server', async () => {
    // The app tries to reach another server's tool by passing its bare name.
    // The host prefixes with THIS bridge's serverID, so the dispatched name is
    // confined to `srvA_*` and can never resolve `srvB`'s tool.
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srvA' });
    await handlers.oncalltool({ name: 'secret_tool', arguments: {} });
    expect(calls.callTool).toEqual([['srvA_secret_tool', {}]]);
    // A name that already looks like another server's namespaced tool is STILL
    // prefixed — it cannot escape this bridge's server.
    await handlers.oncalltool({ name: 'srvB_admin', arguments: {} });
    expect(calls.callTool[1]).toEqual(['srvA_srvB_admin', {}]);
  });

  it('oncalltool surfaces a heavy result by reference, never silently inlined', async () => {
    const { client } = makeFakeClient({
      async callTool(tool) {
        return { tool, isError: false, artifactRef: { id: 'art_abc', sizeBytes: 9_000_000 } };
      },
    });
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const result = await handlers.oncalltool({ name: 'big' });
    const text = result.content[0] as { type: string; text: string };
    expect(text.text).toContain('art_abc');
  });

  it('onlistresourcetemplates routes through the client and resolves gracefully (empty, no error)', async () => {
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const res = await handlers.onlistresourcetemplates();
    expect(calls.listResourceTemplates).toEqual(['srv']);
    expect(res.resourceTemplates).toEqual([]);
  });

  it('onreadresource routes to read_resource and returns inline contents', async () => {
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const res = await handlers.onreadresource({ uri: 'ui://srv/app.html' });
    expect(calls.readResource).toEqual([['srv', 'ui://srv/app.html']]);
    expect((res.contents[0] as { text: string }).text).toContain('hi');
  });

  it('onreadresource fails loudly on a heavy resource (no silent truncation)', async () => {
    const { client } = makeFakeClient({
      async readResource(_s, uri) {
        return { resourceUri: uri, artifactRef: { id: 'art_huge' } };
      },
    });
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    await expect(handlers.onreadresource({ uri: 'ui://srv/big.html' })).rejects.toThrow(/art_huge/);
  });

  it('onlistresources routes to the resource list', async () => {
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const res = await handlers.onlistresources();
    expect(calls.listResources).toEqual(['srv']);
    expect(res.resources[0].uri).toBe('ui://srv/app.html');
  });

  it('onrequestdisplaymode records the request and acks inline only (109b)', async () => {
    const { client } = makeFakeClient();
    const seen: string[] = [];
    const handlers = createAppHandlers({
      client,
      serverID: 'srv',
      onDisplayModeRequest: (r) => seen.push(`${r.requested}->${r.granted}`),
    });
    const res = await handlers.onrequestdisplaymode({ mode: 'fullscreen' });
    expect(res.mode).toBe('inline');
    expect(seen).toEqual(['fullscreen->inline']);
  });

  it('onrequestdisplaymode grants fullscreen / pip when the page advertises them (109c)', async () => {
    const { client } = makeFakeClient();
    const seen: string[] = [];
    const handlers = createAppHandlers({
      client,
      serverID: 'srv',
      availableDisplayModes: ['inline', 'fullscreen', 'pip'],
      onDisplayModeRequest: (r) => seen.push(`${r.requested}->${r.granted}`),
    });
    expect((await handlers.onrequestdisplaymode({ mode: 'fullscreen' })).mode).toBe('fullscreen');
    expect((await handlers.onrequestdisplaymode({ mode: 'pip' })).mode).toBe('pip');
    // An unsupported mode still falls back to the always-available inline.
    const handlers2 = createAppHandlers({
      client,
      serverID: 'srv',
      availableDisplayModes: ['inline', 'fullscreen'],
    });
    expect((await handlers2.onrequestdisplaymode({ mode: 'pip' })).mode).toBe('inline');
    expect(seen).toEqual(['fullscreen->fullscreen', 'pip->pip']);
  });
});

describe('D-173 — the host opens NO direct MCP transport', () => {
  let fetchSpy: ReturnType<typeof vi.fn>;
  let wsSpy: ReturnType<typeof vi.fn>;
  let esSpy: ReturnType<typeof vi.fn>;
  let xhrSpy: ReturnType<typeof vi.fn>;
  const originals: Record<string, unknown> = {};

  beforeEach(() => {
    fetchSpy = vi.fn();
    wsSpy = vi.fn();
    esSpy = vi.fn();
    xhrSpy = vi.fn();
    for (const key of ['fetch', 'WebSocket', 'EventSource', 'XMLHttpRequest']) {
      originals[key] = (globalThis as Record<string, unknown>)[key];
    }
    (globalThis as Record<string, unknown>).fetch = fetchSpy;
    (globalThis as Record<string, unknown>).WebSocket = wsSpy;
    (globalThis as Record<string, unknown>).EventSource = esSpy;
    (globalThis as Record<string, unknown>).XMLHttpRequest = xhrSpy;
  });

  afterEach(() => {
    for (const key of ['fetch', 'WebSocket', 'EventSource', 'XMLHttpRequest']) {
      (globalThis as Record<string, unknown>)[key] = originals[key];
    }
  });

  it('drives every handler through the injected client without touching the network', async () => {
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    await handlers.oncalltool({ name: 'srv_echo', arguments: {} });
    await handlers.onreadresource({ uri: 'ui://srv/app.html' });
    await handlers.onlistresources();
    await handlers.onrequestdisplaymode({ mode: 'inline' });

    // The injected client received the calls...
    expect(calls.callTool.length).toBe(1);
    expect(calls.readResource.length).toBe(1);
    expect(calls.listResources.length).toBe(1);

    // ...and NOTHING opened a direct transport.
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(wsSpy).not.toHaveBeenCalled();
    expect(esSpy).not.toHaveBeenCalled();
    expect(xhrSpy).not.toHaveBeenCalled();
  });

  it('constructing the AppBridgeHost is manual-handler mode and opens no transport', () => {
    const { client } = makeFakeClient();
    const host = new AppBridgeHost({ client, serverID: 'srv' });
    expect(host.mode).toBe('manual-handler');
    expect(host.isInitialized).toBe(false);
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(wsSpy).not.toHaveBeenCalled();
    expect(esSpy).not.toHaveBeenCalled();
  });

  it('the Data Delivery push routes ONLY through the injected client — no global transport', async () => {
    // A heavy result so the delivery path also exercises the artifact fetch
    // (which lives on the injected client, never a global `fetch`).
    const toolContext = vi.fn(async () => ({
      tool: 'srv_echo',
      input: { content: { q: 1 } },
      result: { artifactRef: { id: 'art_big', sizeBytes: 9_000_000 } },
      isError: false,
    }));
    const fetchArtifactText = vi.fn(async () => '{"rows":3}');
    const { client } = makeFakeClient({ toolContext, fetchArtifactText });
    new AppBridgeHost({ client, serverID: 'srv', toolCallId: 'tc_1' });
    const bridge = bridgeHooks.instances.at(-1)!;
    bridge.oninitialized?.();
    await flush();

    // The delivery used the injected client (tool-context fetch + artifact
    // bytes fetch) and pushed both notifications...
    expect(toolContext).toHaveBeenCalledWith('srv', 'tc_1');
    expect(fetchArtifactText).toHaveBeenCalledWith('art_big');
    expect(bridge.pushOrder).toEqual(['input', 'result']);

    // ...and NOTHING opened a direct transport.
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(wsSpy).not.toHaveBeenCalled();
    expect(esSpy).not.toHaveBeenCalled();
    expect(xhrSpy).not.toHaveBeenCalled();
  });
});

describe('Data Delivery — host pushes the captured tool context after init', () => {
  beforeEach(() => {
    bridgeHooks.instances.length = 0;
  });

  function lastBridge() {
    const b = bridgeHooks.instances.at(-1);
    if (!b) throw new Error('no AppBridge instance was constructed');
    return b;
  }

  it('pushes sendToolInput BEFORE sendToolResult, both after initialized, with the right payloads', async () => {
    const toolContext = vi.fn(async () => ({
      tool: 'srv_chart',
      input: { content: { symbol: 'AAPL' } },
      result: { content: { points: [1, 2, 3] } },
      isError: false,
    }));
    const { client } = makeFakeClient({ toolContext });
    const host = new AppBridgeHost({ client, serverID: 'srv', toolCallId: 'tc_42' });
    const bridge = lastBridge();

    // No push before the app initializes.
    expect(bridge.sendToolInput).not.toHaveBeenCalled();
    expect(bridge.sendToolResult).not.toHaveBeenCalled();

    bridge.oninitialized?.();
    await flush();

    expect(host.isInitialized).toBe(true);
    expect(toolContext).toHaveBeenCalledWith('srv', 'tc_42');
    // Order: input before result.
    expect(bridge.pushOrder).toEqual(['input', 'result']);
    expect(bridge.sendToolInput).toHaveBeenCalledWith({ arguments: { symbol: 'AAPL' } });
    const resultArg = bridge.sendToolResult.mock.calls[0][0] as {
      content: Array<{ type: string; text: string }>;
      structuredContent?: Record<string, unknown>;
      isError: boolean;
    };
    expect(resultArg.isError).toBe(false);
    expect(resultArg.structuredContent).toEqual({ points: [1, 2, 3] });
    expect(resultArg.content[0].text).toContain('points');
  });

  it('resolves a heavy (artifactRef) result via fetchArtifactText and delivers the fetched bytes', async () => {
    const { client, calls } = makeFakeClient({
      async toolContext() {
        return {
          tool: 'srv_big',
          input: { content: {} },
          result: { artifactRef: { id: 'art_heavy', sizeBytes: 5_000_000 } },
          isError: false,
        };
      },
      async fetchArtifactText(id) {
        calls.fetchArtifactText.push(id);
        return 'HEAVY-RESULT-BYTES';
      },
    });
    new AppBridgeHost({ client, serverID: 'srv', toolCallId: 'tc_heavy' });
    const bridge = lastBridge();
    bridge.oninitialized?.();
    await flush();

    expect(calls.fetchArtifactText).toEqual(['art_heavy']);
    const resultArg = bridge.sendToolResult.mock.calls[0][0] as {
      content: Array<{ type: string; text: string }>;
    };
    expect(resultArg.content[0].text).toBe('HEAVY-RESULT-BYTES');
  });

  it('on a fetchArtifactText failure delivers a faithful by-reference stub, never empty', async () => {
    const { client } = makeFakeClient({
      async toolContext() {
        return {
          tool: 'srv_big',
          input: { content: {} },
          result: { artifactRef: { id: 'art_unreachable', sizeBytes: 4242 } },
          isError: false,
        };
      },
      async fetchArtifactText() {
        throw new Error('presign unsupported on this store');
      },
    });
    new AppBridgeHost({ client, serverID: 'srv', toolCallId: 'tc_stub' });
    const bridge = lastBridge();
    bridge.oninitialized?.();
    await flush();

    const resultArg = bridge.sendToolResult.mock.calls[0][0] as {
      content: Array<{ type: string; text: string }>;
    };
    const text = resultArg.content[0].text;
    expect(text).not.toBe('');
    expect(text).toContain('art_unreachable');
    expect(text).toContain('4242');
    expect(text).toContain('unavailable');
  });

  it('a heavy INPUT artifactRef is fetched + JSON-parsed into the tool arguments', async () => {
    const { client } = makeFakeClient({
      async toolContext() {
        return {
          tool: 'srv_form',
          input: { artifactRef: { id: 'art_in', sizeBytes: 100 } },
          result: { content: { ok: true } },
          isError: false,
        };
      },
      async fetchArtifactText() {
        return '{"location":"NYC"}';
      },
    });
    new AppBridgeHost({ client, serverID: 'srv', toolCallId: 'tc_in' });
    const bridge = lastBridge();
    bridge.oninitialized?.();
    await flush();

    expect(bridge.sendToolInput).toHaveBeenCalledWith({ arguments: { location: 'NYC' } });
  });

  it('a not-found tool context (null) triggers NO push and NO thrown error', async () => {
    const toolContext = vi.fn(async () => null);
    const { client } = makeFakeClient({ toolContext });
    const host = new AppBridgeHost({ client, serverID: 'srv', toolCallId: 'tc_missing' });
    const bridge = lastBridge();
    expect(() => bridge.oninitialized?.()).not.toThrow();
    await flush();

    expect(host.isInitialized).toBe(true);
    expect(toolContext).toHaveBeenCalledWith('srv', 'tc_missing');
    expect(bridge.sendToolInput).not.toHaveBeenCalled();
    expect(bridge.sendToolResult).not.toHaveBeenCalled();
  });

  it('a toolContext fetch failure is swallowed (best-effort) — no push, no throw', async () => {
    const { client } = makeFakeClient({
      async toolContext() {
        throw new Error('runtime exploded');
      },
    });
    new AppBridgeHost({ client, serverID: 'srv', toolCallId: 'tc_err' });
    const bridge = lastBridge();
    expect(() => bridge.oninitialized?.()).not.toThrow();
    await flush();
    expect(bridge.sendToolInput).not.toHaveBeenCalled();
    expect(bridge.sendToolResult).not.toHaveBeenCalled();
  });

  it('no toolCallId → no fetch and no push (the guard)', async () => {
    const { client, calls } = makeFakeClient();
    new AppBridgeHost({ client, serverID: 'srv' });
    const bridge = lastBridge();
    bridge.oninitialized?.();
    await flush();
    expect(calls.toolContext).toEqual([]);
    expect(bridge.sendToolInput).not.toHaveBeenCalled();
    expect(bridge.sendToolResult).not.toHaveBeenCalled();
  });
});

describe('AppBridgeHost — host-obligation seams', () => {
  beforeEach(() => {
    bridgeHooks.instances.length = 0;
  });

  function lastBridge() {
    const b = bridgeHooks.instances.at(-1);
    if (!b) throw new Error('no AppBridge instance was constructed');
    return b;
  }

  it('threads toolInfo (id + tool name) and containerDimensions into the ui/initialize host-context', () => {
    const { client } = makeFakeClient();
    new AppBridgeHost({
      client,
      serverID: 'srv',
      toolCallId: 'tc_1',
      toolName: 'get_weather',
      containerDimensions: { width: 640, height: 480 },
    });
    const ctx = lastBridge().hostContext as {
      toolInfo?: { id?: string; tool?: { name?: string } };
      containerDimensions?: { width?: number; height?: number };
    };
    expect(ctx.toolInfo?.id).toBe('tc_1');
    expect(ctx.toolInfo?.tool?.name).toBe('get_weather');
    expect(ctx.containerDimensions).toEqual({ width: 640, height: 480 });
  });

  it('omits toolInfo when no tool name is supplied', () => {
    const { client } = makeFakeClient();
    new AppBridgeHost({ client, serverID: 'srv', toolCallId: 'tc_1' });
    const ctx = lastBridge().hostContext as { toolInfo?: unknown };
    expect(ctx.toolInfo).toBeUndefined();
  });

  it('initializes the host-context with the injected theme', () => {
    const { client } = makeFakeClient();
    new AppBridgeHost({ client, serverID: 'srv', theme: 'light' });
    const ctx = lastBridge().hostContext as { theme?: string };
    expect(ctx.theme).toBe('light');
  });

  it('setTheme pushes a host-context-changed (setHostContext) on a theme change, and is a no-op when unchanged', () => {
    const { client } = makeFakeClient();
    const host = new AppBridgeHost({ client, serverID: 'srv', theme: 'dark' });
    const bridge = lastBridge();
    // Same theme — no push.
    host.setTheme('dark');
    expect(bridge.setHostContext).not.toHaveBeenCalled();
    // Changed theme — exactly one push with the new theme.
    host.setTheme('light');
    expect(bridge.setHostContext).toHaveBeenCalledTimes(1);
    expect(bridge.setHostContext).toHaveBeenCalledWith({ theme: 'light' });
  });

  it('forwards the app-emitted size-changed to onSizeChanged', () => {
    const { client } = makeFakeClient();
    const sizes: Array<{ width?: number; height?: number }> = [];
    new AppBridgeHost({
      client,
      serverID: 'srv',
      onSizeChanged: (s) => sizes.push(s),
    });
    const bridge = lastBridge();
    bridge.emit('sizechange', { width: 320, height: 700 });
    expect(sizes).toEqual([{ width: 320, height: 700 }]);
  });

  it('close() sends ui/resource-teardown BEFORE bridge.close()', async () => {
    const { client } = makeFakeClient();
    const host = new AppBridgeHost({ client, serverID: 'srv' });
    const bridge = lastBridge();
    await host.connect({} as unknown as Window);
    await host.close();
    expect(bridge.teardownResource).toHaveBeenCalledWith({});
    // Order: teardown is recorded before close in the shared pushOrder log.
    expect(bridge.pushOrder).toEqual(['teardown', 'close']);
  });

  it('close() is idempotent — teardown is sent at most once', async () => {
    const { client } = makeFakeClient();
    const host = new AppBridgeHost({ client, serverID: 'srv' });
    const bridge = lastBridge();
    await host.connect({} as unknown as Window);
    await host.close();
    await host.close();
    expect(bridge.teardownResource).toHaveBeenCalledTimes(1);
  });

  it('an app request-teardown triggers a graceful close + the injected callback', async () => {
    const { client } = makeFakeClient();
    let toreDown = false;
    const host = new AppBridgeHost({
      client,
      serverID: 'srv',
      onRequestTeardown: () => {
        toreDown = true;
      },
    });
    const bridge = lastBridge();
    await host.connect({} as unknown as Window);
    bridge.emit('requestteardown', {});
    await flush();
    expect(toreDown).toBe(true);
    expect(bridge.teardownResource).toHaveBeenCalledWith({});
  });

  it('availableDisplayModes from the caller (runtime.info) reaches the host-context', () => {
    const { client } = makeFakeClient();
    new AppBridgeHost({
      client,
      serverID: 'srv',
      availableDisplayModes: ['inline', 'pip'],
    });
    const ctx = lastBridge().hostContext as { availableDisplayModes?: string[] };
    expect(ctx.availableDisplayModes).toEqual(['inline', 'pip']);
  });

  it('wiring the new seams opens NO direct transport (the D-173 spy holds on close/teardown/setTheme/size)', async () => {
    const fetchSpy = vi.fn();
    const wsSpy = vi.fn();
    const esSpy = vi.fn();
    const xhrSpy = vi.fn();
    const originals: Record<string, unknown> = {};
    for (const key of ['fetch', 'WebSocket', 'EventSource', 'XMLHttpRequest']) {
      originals[key] = (globalThis as Record<string, unknown>)[key];
    }
    (globalThis as Record<string, unknown>).fetch = fetchSpy;
    (globalThis as Record<string, unknown>).WebSocket = wsSpy;
    (globalThis as Record<string, unknown>).EventSource = esSpy;
    (globalThis as Record<string, unknown>).XMLHttpRequest = xhrSpy;
    try {
      const { client } = makeFakeClient();
      const host = new AppBridgeHost({ client, serverID: 'srv', theme: 'dark' });
      const bridge = lastBridge();
      host.setTheme('light');
      bridge.emit('sizechange', { width: 1, height: 1 });
      await host.connect({} as unknown as Window);
      await host.close();
      expect(fetchSpy).not.toHaveBeenCalled();
      expect(wsSpy).not.toHaveBeenCalled();
      expect(esSpy).not.toHaveBeenCalled();
      expect(xhrSpy).not.toHaveBeenCalled();
    } finally {
      for (const key of ['fetch', 'WebSocket', 'EventSource', 'XMLHttpRequest']) {
        (globalThis as Record<string, unknown>)[key] = originals[key];
      }
    }
  });
});
