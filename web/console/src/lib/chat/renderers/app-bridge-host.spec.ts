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
  containerDimensionsFromBox,
  createAppHandlers,
  isTrustedAppMessage,
  MCPAppToolNotFoundError,
  qualifyAppToolName,
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
  readResource: Array<[string, string, string | undefined]>;
  callTool: Array<[string, string, unknown, string | undefined]>;
  listResources: Array<[string, string | undefined]>;
  listTools: string[];
}

function makeFakeClient(overrides: Partial<MCPAppHostClient> = {}): {
  client: MCPAppHostClient;
  calls: FakeCalls;
} {
  const calls: FakeCalls = { readResource: [], callTool: [], listResources: [], listTools: [] };
  const client: MCPAppHostClient = {
    async readResource(serverID, uri, agentID): Promise<MCPAppResource> {
      calls.readResource.push([serverID, uri, agentID]);
      return { resourceUri: uri, mimeType: 'text/html', content: '<p>hi</p>' };
    },
    async callTool(serverID, tool, args, agentID): Promise<MCPAppToolResult> {
      calls.callTool.push([serverID, tool, args, agentID]);
      return { tool, content: { ok: true }, isError: false };
    },
    async listResources(serverID, agentID) {
      calls.listResources.push([serverID, agentID]);
      return [{ uri: 'ui://srv/app.html', name: 'app', mimeType: 'text/html' }];
    },
    async listResourceTemplates() {
      return [];
    },
    async listTools(serverID) {
      calls.listTools.push(serverID);
      return [{ name: 'srv_echo', description: 'echo' }];
    },
    async resolveArtifact(id) {
      return `https://artifacts.example/${id}`;
    },
    async toolContext() {
      return null;
    },
    async fetchArtifactText() {
      return '{}';
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
  it('oncalltool qualifies the app-supplied BARE name into the server namespace', async () => {
    // A spec-conformant app knows only its SERVER-side tool names (`echo`), not
    // Harbor's `<source>_<tool>` catalog keys. The host qualifies the name so
    // the call resolves at all — and so the app is confined (next case).
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv', agentID: 'agent-app' });
    const result = await handlers.oncalltool({ name: 'echo', arguments: { q: 1 } });
    expect(calls.callTool).toEqual([['srv', 'srv_echo', { q: 1 }, 'agent-app']]);
    expect(result.isError).toBe(false);
    expect(result.structuredContent).toEqual({ ok: true });
  });

  it('oncalltool CONFINES the app to its own server — a cross-server name cannot escape', async () => {
    // The confinement property. An app that tries to name another server's tool
    // (or an in-proc / HTTP / A2A catalog entry by its exact catalog key) is
    // still prefixed, so the dispatched name stays inside `<serverID>_` and
    // cannot resolve to the tool it aimed at. There is no input the app can
    // supply that reaches outside its own namespace.
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    await handlers.oncalltool({ name: 'otherserver_drop_table' });
    await handlers.oncalltool({ name: 'harbor_spawn_task' });
    expect(calls.callTool.map(([, t]) => t)).toEqual([
      'srv_otherserver_drop_table',
      'srv_harbor_spawn_task',
    ]);
    for (const [, dispatched] of calls.callTool) {
      expect(String(dispatched).startsWith('srv_')).toBe(true);
    }
  });

  it('oncalltool qualifies even a name that ALREADY looks qualified (no escape hatch)', async () => {
    // The subtle regression an `if (name.startsWith(serverID + "_")) return name`
    // shortcut would introduce: it looks like a harmless idempotence tweak, and
    // the cross-server case above still passes under it — but it hands an App a
    // way to bypass qualification by prefixing its own server id itself, which
    // is the first half of reaching a NEIGHBOUR namespace. The prefix is
    // unconditional: a bare `srv_echo` from the app on server `srv` dispatches
    // `srv_srv_echo`, not `srv_echo`.
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    await handlers.oncalltool({ name: 'srv_echo' });
    expect(calls.callTool).toEqual([['srv', 'srv_srv_echo', undefined, undefined]]);
  });

  it('qualifyAppToolName is unconditional — every name lands inside the server namespace', () => {
    expect(qualifyAppToolName('srv', 'echo')).toBe('srv_echo');
    expect(qualifyAppToolName('srv', 'other_echo')).toBe('srv_other_echo');
    // Idempotence is deliberately NOT a property here — see the handler case.
    expect(qualifyAppToolName('srv', 'srv_echo')).toBe('srv_srv_echo');
    // Even an attempt to spoof separators stays under the prefix.
    expect(qualifyAppToolName('srv', '../evil').startsWith('srv_')).toBe(true);
    expect(qualifyAppToolName('srv', '').startsWith('srv_')).toBe(true);
  });

  it('oncalltool raises a TYPED not-found naming the bare name + the confining server', async () => {
    // The distinguishable-degradation property: an app must be able to tell
    // "there is no such tool here" from "the transport broke", and must see the
    // name IT asked for, not Harbor's internal catalog key.
    const { client } = makeFakeClient({
      async callTool(_serverID, tool) {
        throw new MCPAppToolNotFoundError(tool);
      },
    });
    const handlers = createAppHandlers({ client, serverID: 'srv', agentID: 'agent-app' });
    await expect(handlers.oncalltool({ name: 'nope' })).rejects.toBeInstanceOf(
      MCPAppToolNotFoundError,
    );
    const err = await handlers.oncalltool({ name: 'nope' }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(MCPAppToolNotFoundError);
    expect((err as MCPAppToolNotFoundError).tool).toBe('nope');
    expect((err as MCPAppToolNotFoundError).serverID).toBe('srv');
    expect((err as Error).message).toContain('srv');
  });

  it('oncalltool propagates a NON-not-found failure unchanged (no swallowing)', async () => {
    const { client } = makeFakeClient({
      async callTool() {
        throw new Error('southbound transport reset');
      },
    });
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    await expect(handlers.oncalltool({ name: 'echo' })).rejects.toThrow(
      /southbound transport reset/,
    );
  });

  it('onlistresourcetemplates answers the advertised capability instead of erroring', async () => {
    // The host advertises `serverResources`; an app probing
    // `resources/templates/list` must get an answer, not a method-not-found.
    const { client } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const res = await handlers.onlistresourcetemplates();
    expect(res.resourceTemplates).toEqual([]);
  });

  it('onlistresourcetemplates maps rows through when the host has templates', async () => {
    const { client } = makeFakeClient({
      async listResourceTemplates() {
        return [{ uriTemplate: 'ui://srv/{id}.html', mimeType: 'text/html' }];
      },
    });
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const res = await handlers.onlistresourcetemplates();
    expect(res.resourceTemplates).toEqual([
      { uriTemplate: 'ui://srv/{id}.html', name: 'ui://srv/{id}.html', mimeType: 'text/html' },
    ]);
  });

  it('oncalltool READS a heavy by-reference result and delivers it as structured data', async () => {
    // The by-reference arm used to push a bare `[artifact <id> · <n> bytes]`
    // block and return with `structuredContent` unset, so an app that called
    // its own tool and got a large answer received prose ABOUT its data instead
    // of the data. Now the bytes are read through the injected client — the
    // driver-independent byte read (D-353) — and delivered exactly as an
    // inline-sized result is, so SIZE no longer decides the shape.
    const reads: string[] = [];
    const { client } = makeFakeClient({
      async callTool(_serverID, tool) {
        return { tool, isError: false, artifactRef: { id: 'art_abc', sizeBytes: 9_000_000 } };
      },
      async fetchArtifactText(id) {
        reads.push(id);
        return '{"revenue":42}';
      },
    });
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const result = await handlers.oncalltool({ name: 'srv_big' });
    expect(reads).toEqual(['art_abc']);
    expect(result.structuredContent).toEqual({ revenue: 42 });
    expect((result.content[0] as { text: string }).text).toBe('{"revenue":42}');
  });

  it('oncalltool delivers a FAITHFUL notice when the heavy result cannot be read', async () => {
    // Fail loud, never silently empty: the app is told which artifact could not
    // be read, and `structuredContent` stays absent so its absence keeps
    // meaning "there is no data here".
    const { client } = makeFakeClient({
      async callTool(_serverID, tool) {
        return { tool, isError: false, artifactRef: { id: 'art_abc', sizeBytes: 9_000_000 } };
      },
      async fetchArtifactText() {
        throw new Error('artifacts.get: runtime_error');
      },
    });
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const result = await handlers.oncalltool({ name: 'srv_big' });
    const text = result.content[0] as { type: string; text: string };
    expect(text.text).toContain('art_abc');
    expect(text.text).toContain('9000000 bytes');
    expect(result.structuredContent).toBeUndefined();
  });

  it('onreadresource routes to read_resource and returns inline contents', async () => {
    const { client, calls } = makeFakeClient();
    const handlers = createAppHandlers({ client, serverID: 'srv', agentID: 'agent-app' });
    const res = await handlers.onreadresource({ uri: 'ui://srv/app.html' });
    expect(calls.readResource).toEqual([['srv', 'ui://srv/app.html', 'agent-app']]);
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
    const handlers = createAppHandlers({ client, serverID: 'srv', agentID: 'agent-app' });
    const res = await handlers.onlistresources();
    expect(calls.listResources).toEqual([['srv', 'agent-app']]);
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

describe('containerDimensionsFromBox — the host-context container snapshot', () => {
  it('emits a fixed width + a maximum height (the host-owned growth bound)', () => {
    expect(containerDimensionsFromBox({ width: 640, maxHeight: 400 })).toEqual({
      width: 640,
      maxHeight: 400,
    });
  });

  it('omits an unknown / non-positive maxHeight rather than sending zero', () => {
    expect(containerDimensionsFromBox({ width: 640 })).toEqual({ width: 640 });
    expect(containerDimensionsFromBox({ width: 640, maxHeight: 0 })).toEqual({ width: 640 });
  });

  it('emits nothing at all for an unmeasured box — a zero box is a lie', () => {
    expect(containerDimensionsFromBox({ width: 0, maxHeight: 400 })).toBeUndefined();
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
    await handlers.oncalltool({ name: 'echo', arguments: {} });
    await handlers.onreadresource({ uri: 'ui://srv/app.html' });
    await handlers.onlistresources();
    await handlers.onlistresourcetemplates();
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

  it('the HEAVY by-reference arm reads through the injected client too — no direct transport', async () => {
    // The case above only ever drove the INLINE arm, so the by-reference arm —
    // the one that now performs an artifact READ — was outside the guard. The
    // read must ride the injected client like everything else: the chat module
    // never issues a network call itself (D-173), and a raw `fetch` added here
    // would be invisible to a test that never takes this branch.
    let read = 0;
    const { client } = makeFakeClient({
      async callTool(_serverID, tool) {
        return { tool, isError: false, artifactRef: { id: 'art_heavy', sizeBytes: 90_000 } };
      },
      async fetchArtifactText() {
        read += 1;
        return '{"revenue":42}';
      },
    });
    const handlers = createAppHandlers({ client, serverID: 'srv' });
    const result = await handlers.oncalltool({ name: 'big', arguments: {} });

    expect(read).toBe(1);
    expect(result.structuredContent).toEqual({ revenue: 42 });
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
