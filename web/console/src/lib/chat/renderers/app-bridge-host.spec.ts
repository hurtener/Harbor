// Phase 109b — the MCP Apps host wrapper unit + integration tests.
//
// Covers the D-173 invariants that do not need a real browser: the sandbox /
// CSP constants, the postMessage origin/source guard, the manual handlers
// (each dispatches to the injected Protocol surface, NEVER a direct
// transport), and the AppBridge wrapper's manual-handler mode. The real-iframe
// sandbox-escape + handshake assertions live in the Playwright spec
// (`tests/mcp-app-host.spec.ts`).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

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
  listTools: string[];
}

function makeFakeClient(overrides: Partial<MCPAppHostClient> = {}): {
  client: MCPAppHostClient;
  calls: FakeCalls;
} {
  const calls: FakeCalls = { readResource: [], callTool: [], listResources: [], listTools: [] };
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
    async listTools(serverID) {
      calls.listTools.push(serverID);
      return [{ name: 'srv_echo', description: 'echo' }];
    },
    ...overrides,
  };
  return { client, calls };
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
  it('oncalltool proxies tool name + args and maps the result', async () => {
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const result = await handlers.oncalltool({ name: 'srv_echo', arguments: { q: 1 } });
    expect(calls.callTool).toEqual([['srv_echo', { q: 1 }]]);
    expect(result.isError).toBe(false);
    expect(result.structuredContent).toEqual({ ok: true });
  });

  it('oncalltool surfaces a heavy result by reference, never silently inlined', async () => {
    const { client } = makeFakeClient({
      async callTool(tool) {
        return { tool, isError: false, artifactRef: { id: 'art_abc', sizeBytes: 9_000_000 } };
      },
    });
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const result = await handlers.oncalltool({ name: 'srv_big' });
    const text = result.content[0] as { type: string; text: string };
    expect(text.text).toContain('art_abc');
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
});
