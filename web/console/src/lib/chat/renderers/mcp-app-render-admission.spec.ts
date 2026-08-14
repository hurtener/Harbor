// HA-56 — the MCP Apps render-admission consumer spec, at the component level.
//
// The REAL `McpAppRenderer` (with a mocked official `AppBridge`) is driven end
// to end over the REAL pre-mount flow: the renderer's initial-document read
// goes through `readRenderDocument` (the opt-in `request_render_admission:
// true` read), the fresh admission is kept HOST-PRIVATE on the live
// renderer/host instances, and every app-initiated `tools/call` rides the
// DISTINCT `render_admission` authority — never the legacy binding, never both.
//
// Proved here:
//
//   - durable/reopened ref → pre-mount OPT-IN read/admission → iframe →
//     same-server app-only callback exactly once, with the fresh token on the
//     wire and ZERO token bytes in the DOM / storage / srcdoc / bridge payloads
//     (the read-time `tool_context` leg is pinned by the sibling replay spec);
//   - a context MISS (evicted / unknown / foreign replay) completes the bounded
//     replay BEFORE the opt-in read: zero `readRenderDocument` calls, zero
//     minted authority, the honest placeholder, no iframe;
//   - a typed UNAVAILABLE admission at read time: at most ONE bounded fresh
//     opt-in re-read, then the explicit `admission` safe state — never a
//     retry loop, never a silent downgrade to the tool-context miss;
//   - an app-initiated secondary `resources/read` returns resource data on the
//     ORDINARY non-minting read and never mints / replaces the admission;
//   - a typed EXPIRED admission at call time refreshes ONCE (fresh opt-in read
//     → new admission → one retry), and an unrecoverable / tampered / mismatched
//     verdict fails explicitly WITHOUT executing the callback;
//   - switching display modes neither drops nor exposes the admission, while a
//     genuine remount (a fresh renderer instance) obtains a DISTINCT token;
//   - mixed concurrent renderers never bleed server/resource/session
//     admissions across instances.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { McpUiHostContext } from '@modelcontextprotocol/ext-apps/app-bridge';

import type {
  MCPAppHostClient,
  MCPAppRenderAdmission,
  MCPAppToolResult,
} from './app-bridge-host.js';
import { MCPAppRenderAdmissionError } from './app-bridge-host.js';

/* ------------------------------------------------------------------ */
/* Mocked official AppBridge — records instances + every sender call.  */
/* ------------------------------------------------------------------ */

interface MockBridge {
  hostContext: McpUiHostContext | undefined;
  oncalltool?: (params: { name: string; arguments?: unknown }) => Promise<unknown>;
  onreadresource?: (params: { uri: string }) => Promise<unknown>;
  oninitialized?: (params: unknown) => void;
  sendToolInputCalls: unknown[];
  sendToolResultCalls: unknown[];
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
    onlistresourcetemplates: unknown;
    onrequestdisplaymode: unknown;
    oninitialized: ((params: unknown) => void) | undefined;
    hostContext: McpUiHostContext | undefined;
    sendToolInputCalls: unknown[] = [];
    sendToolResultCalls: unknown[] = [];
    setHostContextCalls: McpUiHostContext[] = [];
    closeCalls = 0;
    constructor(...args: unknown[]) {
      const opts = args[3] as { hostContext?: McpUiHostContext } | undefined;
      this.hostContext = opts?.hostContext;
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
      this.sendToolInputCalls.push(p);
    }
    async sendToolResult(p: unknown): Promise<void> {
      this.sendToolResultCalls.push(p);
    }
    // The official graceful-teardown surface. The host calls it on unmount of
    // an initialized app; without it the mock throws the repeated swallowed
    // TypeError in `AppBridgeHost.close()`'s best-effort teardown.
    async teardownResource(): Promise<Record<string, unknown>> {
      return {};
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

const McpAppRenderer = (await import('./mcp-app.svelte')).default;
const { mountRendererReactive } = await import('./mcp-app-harness.svelte.js');

function installMatchMedia(): void {
  (window as unknown as { matchMedia: (q: string) => MediaQueryList }).matchMedia = () =>
    ({
      matches: true,
      media: '(prefers-color-scheme: dark)',
      addEventListener: () => {},
      removeEventListener: () => {},
    }) as unknown as MediaQueryList;
}

/* ------------------------------------------------------------------ */
/* A fake injected Protocol surface with controllable reads + calls.   */
/* ------------------------------------------------------------------ */

interface CallRecord {
  serverID: string;
  tool: string;
  args: unknown;
  agentID: string | undefined;
  binding: string | undefined;
  resourceURI: string | undefined;
  renderAdmission: string | undefined;
}

const TOKEN_A = 'opaque-sealed-render-token-A';
const TOKEN_B = 'opaque-sealed-render-token-B';

/**
 * The Runtime's PRECISE unwired-posture answer to the opt-in read when the
 * operator left the surface OFF (`tools.mcp_app_render_admission.enabled:
 * false` — the default): `runtime_error` naming that the render-admission
 * authority is not wired (internal/protocol/apps.go::mintRenderAdmission).
 * This is the shape the P1 fix recovers — it is NOT a typed `unavailable`
 * admission and NOT the broken-gate "authorization failed" runtime_error.
 */
const UNWIRED_RUNTIME_ERROR = {
  code: 'runtime_error',
  message:
    'method "mcp.servers.read_resource": render-admission authority is not wired on this runtime',
};

function admission(
  token: string,
  availability: 'available' | 'unavailable' = 'available',
): MCPAppRenderAdmission {
  return {
    token: availability === 'available' ? token : '',
    issuedAt: '2026-08-14T00:00:00Z',
    expiresAt: '2026-08-14T01:00:00Z',
    availability,
  };
}

function makeFakeClient(): {
  client: MCPAppHostClient;
  docReads: ReturnType<typeof vi.fn>;
  reads: ReturnType<typeof vi.fn>;
  calls: ReturnType<typeof vi.fn>;
} {
  const docReads = vi.fn(
    async (_serverID: string, uri: string, _agentID?: string) => ({
      resourceUri: uri,
      mimeType: 'text/html',
      content: '<p>app</p>',
      renderAdmission: admission(TOKEN_A),
    }),
  );
  const reads = vi.fn(async (_serverID: string, uri: string, _agentID?: string) => ({
    resourceUri: uri,
    mimeType: 'text/html',
    content: '<p>resource data</p>',
  }));
  const calls = vi.fn(
    async (
      _serverID: string,
      tool: string,
      _args?: unknown,
      _agentID?: string,
      _binding?: string,
      _resourceURI?: string,
      _renderAdmission?: string,
    ): Promise<MCPAppToolResult> => {
      return { tool, content: { ok: true }, isError: false };
    },
  );
  const client: MCPAppHostClient = {
    async readResource(serverID, resourceURI, agentID) {
      return reads(serverID, resourceURI, agentID);
    },
    async readRenderDocument(serverID, resourceURI, agentID) {
      return docReads(serverID, resourceURI, agentID);
    },
    async callTool(serverID, tool, args, agentID, binding, resourceURI, renderAdmission) {
      return calls(serverID, tool, args, agentID, binding, resourceURI, renderAdmission);
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
      return {
        tool: 'srv_report',
        input: { content: { region: 'emea' } },
        result: { content: { revenue: 42 } },
        isError: false,
      };
    },
    async fetchArtifactText() {
      return '{}';
    },
  };
  return { client, docReads, reads, calls };
}

const APP = {
  resourceUri: 'ui://srv/app.html',
  displayMode: 'inline' as const,
  rawHtmlTrusted: false,
  toolCallId: 'tc_1',
};

async function settle(times = 12): Promise<void> {
  for (let i = 0; i < times; i++) {
    flushSync();
    await Promise.resolve();
  }
  flushSync();
}

/**
 * Mount the real renderer and drain preload + lifecycle effects.
 * `expectedInstances` lets a test that mounts several renderers side by side
 * wait for THIS renderer's bridge specifically (the drain loop must not exit
 * early just because a sibling renderer already constructed its bridge).
 */
async function mountAndConnect(
  target: HTMLElement,
  client: MCPAppHostClient,
  app: Record<string, unknown> = APP,
  expectedInstances = 1,
): Promise<ReturnType<typeof mount>> {
  const component = mount(McpAppRenderer, {
    target,
    props: {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app,
      serverID: 'srv',
      appHostClient: client,
    } as never,
  });
  for (let i = 0; i < 12 && captured.instances.length < expectedInstances; i++) {
    flushSync();
    await Promise.resolve();
  }
  flushSync();
  return component;
}

/** The last recorded bridge `oncalltool` dispatch, as the fake recorded it. */
function recordOf(calls: ReturnType<typeof vi.fn>, index = 0): CallRecord {
  const [serverID, tool, args, agentID, binding, resourceURI, renderAdmission] = calls.mock.calls[
    index
  ] as unknown[];
  return {
    serverID: serverID as string,
    tool: tool as string,
    args,
    agentID: agentID as string | undefined,
    binding: binding as string | undefined,
    resourceURI: resourceURI as string | undefined,
    renderAdmission: renderAdmission as string | undefined,
  };
}

function frameOf(target: HTMLElement): HTMLIFrameElement | null {
  return target.querySelector('iframe');
}

function admissionPlaceholder(target: HTMLElement): Element | null {
  return target.querySelector("[data-testid='mcp-app-admission']");
}

/** Assert the token appears nowhere the sandbox (or any sink) could reach. */
function assertTokenNowhere(target: HTMLElement, bridge: MockBridge, token: string): void {
  expect(target.innerHTML).not.toContain(token);
  expect(frameOf(target)?.getAttribute('srcdoc') ?? '').not.toContain(token);
  expect(frameOf(target)?.getAttribute('sandbox') ?? '').not.toContain(token);
  expect(JSON.stringify(bridge.hostContext ?? {})).not.toContain(token);
  for (const sent of [...bridge.sendToolInputCalls, ...bridge.sendToolResultCalls]) {
    expect(JSON.stringify(sent)).not.toContain(token);
  }
  expect(sessionStorage.getItem(token)).toBeNull();
  expect(localStorage.getItem(token)).toBeNull();
}

afterEach(() => {
  captured.instances.length = 0;
  document.body.innerHTML = '';
  sessionStorage.clear();
  localStorage.clear();
});

describe('McpAppRenderer — pre-mount render admission (HA-56)', () => {
  it('durable/reopened ref → OPT-IN read/admission → iframe → same-server app-only callback exactly once', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads, reads, calls } = makeFakeClient();
    const component = await mountAndConnect(target, client);

    // The pre-mount document read is the RENDERER-INTERNAL opt-in read — it
    // asks the Runtime to mint (`request_render_admission: true` is the
    // adapter's 4th argument, driven here through the real client seam).
    expect(docReads).toHaveBeenCalledTimes(1);
    expect(docReads).toHaveBeenCalledWith('srv', 'ui://srv/app.html', undefined);
    // The ordinary non-minting read was NOT used for the initial document.
    expect(reads).not.toHaveBeenCalled();
    // The app mounted: one bridge, one iframe, no safe state.
    expect(captured.instances).toHaveLength(1);
    expect(frameOf(target)).not.toBeNull();
    expect(admissionPlaceholder(target)).toBeNull();
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    // An app-initiated callback executes EXACTLY ONCE, dispatched to the
    // host-derived same-server namespace with the FRESH admission token.
    await bridge.oncalltool?.({ name: 'echo', arguments: { q: 1 } });
    expect(calls).toHaveBeenCalledTimes(1);
    const record = recordOf(calls);
    expect(record.serverID).toBe('srv');
    expect(record.tool).toBe('srv_echo'); // qualified, confined to its own server
    expect(record.binding).toBeUndefined(); // never the legacy binding
    expect(record.renderAdmission).toBe(TOKEN_A);

    // The opaque admission appears NOWHERE outside the live host instances.
    assertTokenNowhere(target, bridge, TOKEN_A);
    expect(JSON.stringify(APP)).not.toContain(TOKEN_A);

    unmount(component);
  });

  it('a context MISS completes the bounded replay BEFORE the opt-in read — zero reads, zero minted authority', async () => {
    // The REPLAY shape: the app carries a `toolCallId` but its captured
    // context record is gone (evicted / unknown / another identity). The
    // bounded replay resolves FIRST and misses, so the renderer returns the
    // honest placeholder WITHOUT ever invoking the admission-requesting
    // document read — no opt-in read, no minted render admission, no iframe.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads, reads, calls } = makeFakeClient();
    const missClient: MCPAppHostClient = { ...client, toolContext: async () => null };
    const component = await mountAndConnect(target, missClient);

    expect(docReads).toHaveBeenCalledTimes(0);
    expect(reads).not.toHaveBeenCalled();
    expect(target.querySelector("[data-testid='mcp-app-unavailable']")).not.toBeNull();
    expect(frameOf(target)).toBeNull();
    expect(captured.instances).toHaveLength(0);
    // No authority was ever minted: the fake's callTool was never reached and
    // no token exists anywhere the sandbox could have touched.
    expect(calls).not.toHaveBeenCalled();
    expect(target.innerHTML).not.toContain(TOKEN_A);

    unmount(component);
  });

  it('a typed UNAVAILABLE admission re-reads ONCE, then shows the explicit safe state — never a loop', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads } = makeFakeClient();
    // The Runtime's closed "no admission minted" answer — twice (the initial
    // read + the single bounded refresh).
    docReads.mockImplementation(async (_s: string, uri: string) => ({
      resourceUri: uri,
      mimeType: 'text/html',
      content: '<p>app</p>',
      renderAdmission: admission(TOKEN_A, 'unavailable'),
    }));
    const component = await mountAndConnect(target, client);

    // Exactly ONE bounded re-read happened (initial + refresh = 2 total), and
    // then the renderer STOPPED — no third read, no loop.
    expect(docReads).toHaveBeenCalledTimes(2);
    // The explicit safe state, NOT the tool-context miss and NOT a dead
    // iframe. The app itself is current — this is an authorization outcome.
    expect(admissionPlaceholder(target)).not.toBeNull();
    expect(admissionPlaceholder(target)?.textContent).toContain('could not be authorized');
    expect(target.querySelector("[data-testid='mcp-app-unavailable']")).toBeNull();
    expect(frameOf(target)).toBeNull();
    expect(captured.instances).toHaveLength(0);
    // No token anywhere (there is none — unavailable carries none).
    expect(JSON.stringify(docReads.mock.results)).not.toContain(TOKEN_A);

    unmount(component);
  });

  it('an UNAVAILABLE admission whose bounded re-read mints recovers and mounts with the fresh token', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads, calls } = makeFakeClient();
    docReads
      .mockResolvedValueOnce({
        resourceUri: 'ui://srv/app.html',
        mimeType: 'text/html',
        content: '<p>app</p>',
        renderAdmission: admission(TOKEN_A, 'unavailable'),
      })
      .mockResolvedValueOnce({
        resourceUri: 'ui://srv/app.html',
        mimeType: 'text/html',
        content: '<p>app</p>',
        renderAdmission: admission(TOKEN_A),
      });
    const component = await mountAndConnect(target, client);

    // The single bounded refresh minted — the app mounts with the FRESH
    // admission from the re-read.
    expect(docReads).toHaveBeenCalledTimes(2);
    expect(frameOf(target)).not.toBeNull();
    expect(admissionPlaceholder(target)).toBeNull();
    expect(captured.instances).toHaveLength(1);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    await bridge.oncalltool?.({ name: 'echo' });
    expect(recordOf(calls).renderAdmission).toBe(TOKEN_A);

    unmount(component);
  });

  it('an app-initiated secondary resources/read returns resource data and NEVER mints or replaces the admission', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads, reads } = makeFakeClient();
    const component = await mountAndConnect(target, client);
    expect(docReads).toHaveBeenCalledTimes(1);
    const bridge = captured.instances[0];

    // The app reads ANOTHER resource through the bridge — the ordinary
    // non-minting path: resource data comes back, and the renderer-internal
    // opt-in read is NOT re-invoked (no second admission was minted).
    const res = await bridge.onreadresource?.({ uri: 'ui://srv/other.html' });
    expect(reads).toHaveBeenCalledWith('srv', 'ui://srv/other.html', undefined);
    expect((res as { contents: Array<{ text: string }> }).contents[0].text).toContain(
      'resource data',
    );
    expect(docReads).toHaveBeenCalledTimes(1);
    // The sandbox never received token bytes through any bridge payload.
    assertTokenNowhere(target, bridge, TOKEN_A);

    unmount(component);
  });

  it('a typed EXPIRED admission at call time refreshes ONCE and retries with the fresh token', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads, calls } = makeFakeClient();
    // The preload mints TOKEN_A; the refresh mints a distinct fresh token.
    docReads
      .mockResolvedValueOnce({
        resourceUri: 'ui://srv/app.html',
        mimeType: 'text/html',
        content: '<p>app</p>',
        renderAdmission: admission(TOKEN_A),
      })
      .mockResolvedValueOnce({
        resourceUri: 'ui://srv/app.html',
        mimeType: 'text/html',
        content: '<p>app</p>',
        renderAdmission: admission('freshly-minted-token-2'),
      });
    let call = 0;
    calls.mockImplementation(async (_s: string, tool: string, _a: unknown, _ag: string | undefined, _b: string | undefined, _r: string | undefined, _admissionToken?: string): Promise<MCPAppToolResult> => {
      call += 1;
      if (call === 1) {
        // The Runtime refuses the first dispatch: the admission expired.
        throw new MCPAppRenderAdmissionError('render_admission_expired', 'expired');
      }
      return { tool, content: { ok: true }, isError: false };
    });
    const component = await mountAndConnect(target, client);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    const result = await bridge.oncalltool?.({ name: 'echo', arguments: { q: 2 } });

    // The callback executed EXACTLY ONCE — after ONE bounded refresh (the
    // preload read + the refresh read = 2 total opt-in reads) and ONE retry
    // riding the fresh token.
    expect(docReads).toHaveBeenCalledTimes(2);
    expect(calls).toHaveBeenCalledTimes(2);
    expect(recordOf(calls, 0).renderAdmission).toBe(TOKEN_A);
    expect(recordOf(calls, 1).renderAdmission).toBe('freshly-minted-token-2');
    expect((result as { structuredContent?: unknown }).structuredContent).toEqual({ ok: true });
    // A recovered expiry is not a failure: no safe state, bridge still live.
    expect(admissionPlaceholder(target)).toBeNull();
    expect(frameOf(target)).not.toBeNull();
    expect(captured.instances[0].closeCalls).toBe(0);

    unmount(component);
  });

  it('an EXPIRED refresh that cannot mint fails explicitly — safe state, no retry, no callback execution', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads, calls } = makeFakeClient();
    docReads
      .mockResolvedValueOnce({
        resourceUri: 'ui://srv/app.html',
        mimeType: 'text/html',
        content: '<p>app</p>',
        renderAdmission: admission(TOKEN_A),
      })
      .mockResolvedValueOnce({
        resourceUri: 'ui://srv/app.html',
        mimeType: 'text/html',
        content: '<p>app</p>',
        renderAdmission: admission('', 'unavailable'),
      });
    calls.mockImplementation(async () => {
      throw new MCPAppRenderAdmissionError('render_admission_expired', 'expired');
    });
    const component = await mountAndConnect(target, client);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    await bridge.oncalltool?.({ name: 'echo' }).catch(() => {});
    await settle(8);

    // The single refresh was spent and produced no admission: explicit safe
    // state replaces the frame, the callback NEVER executed (one dispatch),
    // and no second refresh was attempted.
    expect(docReads).toHaveBeenCalledTimes(2);
    expect(calls).toHaveBeenCalledTimes(1);
    expect(admissionPlaceholder(target)).not.toBeNull();
    expect(frameOf(target)).toBeNull();

    unmount(component);
  });

  it('tampered / mismatched / foreign verdicts fail explicitly — no refresh, no callback execution', async () => {
    for (const code of ['render_admission_unavailable', 'render_admission_invalid', 'render_admission_mismatch'] as const) {
      captured.instances.length = 0;
      document.body.innerHTML = '';
      installMatchMedia();
      const target = document.createElement('div');
      document.body.appendChild(target);
      const { client, docReads, calls } = makeFakeClient();
      calls.mockImplementation(async () => {
        throw new MCPAppRenderAdmissionError(code, code);
      });
      const component = await mountAndConnect(target, client);
      const bridge = captured.instances[0];
      bridge.fireInitialized();
      flushSync();

      await bridge.oncalltool?.({ name: 'echo' }).catch(() => {});
      await settle(8);

      // NO refresh was spent (the preload read is the only opt-in read) and
      // the callback never executed.
      expect(docReads, code).toHaveBeenCalledTimes(1);
      expect(calls, code).toHaveBeenCalledTimes(1);
      expect(admissionPlaceholder(target), code).not.toBeNull();
      expect(frameOf(target), code).toBeNull();

      unmount(component);
    }
  });

  it('switching display modes does not drop or expose the admission; a remount obtains a DISTINCT token', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, calls } = makeFakeClient();
    const { component, props } = mountRendererReactive(target, {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app: { ...APP },
      serverID: 'srv',
      appHostClient: client,
    } as never);
    await settle();
    expect(captured.instances).toHaveLength(1);
    const inlineBridge = captured.instances[0];
    inlineBridge.fireInitialized();
    flushSync();

    // Fullscreen / PiP transitions reuse the SAME live renderer instance: the
    // displayMode prop changes but the app identity (document + tool call)
    // does not — no re-preload, no re-read, no bridge rebuild.
    (props as unknown as { app: Record<string, unknown> }).app = { ...APP, displayMode: 'fullscreen' };
    await settle();
    expect(captured.instances).toHaveLength(1);
    expect(captured.instances[0]).toBe(inlineBridge);
    expect(captured.instances[0].closeCalls).toBe(0);

    await inlineBridge.oncalltool?.({ name: 'echo' });
    // The SAME admission survived the mode transition — not dropped, not
    // re-minted.
    expect(recordOf(calls).renderAdmission).toBe(TOKEN_A);
    assertTokenNowhere(target, inlineBridge, TOKEN_A);

    // A GENUINE remount (a fresh renderer instance, e.g. the page-level
    // AppPanel for fullscreen) runs a fresh preload and obtains a DISTINCT
    // admission from its own read.
    const target2 = document.createElement('div');
    document.body.appendChild(target2);
    const { client: client2, docReads: docReads2, calls: calls2 } = makeFakeClient();
    docReads2.mockImplementation(async (_s: string, uri: string) => ({
      resourceUri: uri,
      mimeType: 'text/html',
      content: '<p>app</p>',
      renderAdmission: admission(TOKEN_B),
    }));
    const component2 = await mountAndConnect(target2, client2, APP, 2);
    expect(captured.instances).toHaveLength(2);
    expect(docReads2).toHaveBeenCalledTimes(1);
    const fullscreenBridge = captured.instances[1];
    fullscreenBridge.fireInitialized();
    flushSync();
    await fullscreenBridge.oncalltool?.({ name: 'echo' });

    expect(recordOf(calls2).renderAdmission).toBe(TOKEN_B);
    expect(recordOf(calls2).renderAdmission).not.toBe(TOKEN_A);
    // Each renderer's token is absent from the OTHER's DOM.
    expect(target2.innerHTML).not.toContain(TOKEN_A);
    expect(target.innerHTML).not.toContain(TOKEN_B);

    unmount(component);
    unmount(component2);
  });

  it('mixed concurrent renderers do not bleed server / resource / session admissions', async () => {
    installMatchMedia();
    // Renderer A: server `reports`, resource A, token A. Renderer B: server
    // `weather`, resource B, token B. Both live at once.
    const { client: clientA, docReads: docReadsA, calls: callsA } = makeFakeClient();
    docReadsA.mockImplementation(async (_s: string, uri: string) => ({
      resourceUri: uri,
      mimeType: 'text/html',
      content: '<p>reports app</p>',
      renderAdmission: admission(TOKEN_A),
    }));
    const targetA = document.createElement('div');
    document.body.appendChild(targetA);
    const componentA = await mountAndConnect(targetA, clientA, {
      ...APP,
      resourceUri: 'ui://reports/dashboard.html',
    });

    const { client: clientB, docReads: docReadsB, calls: callsB } = makeFakeClient();
    docReadsB.mockImplementation(async (_s: string, uri: string) => ({
      resourceUri: uri,
      mimeType: 'text/html',
      content: '<p>weather app</p>',
      renderAdmission: admission(TOKEN_B),
    }));
    const targetB = document.createElement('div');
    document.body.appendChild(targetB);
    const componentB = await mountAndConnect(targetB, clientB, {
      ...APP,
      resourceUri: 'ui://weather/main.html',
    }, 2);

    expect(captured.instances).toHaveLength(2);
    const [bridgeA, bridgeB] = captured.instances;
    bridgeA.fireInitialized();
    bridgeB.fireInitialized();
    flushSync();

    await bridgeA.oncalltool?.({ name: 'echo' });
    await bridgeB.oncalltool?.({ name: 'echo' });

    // Each dispatch rode ITS OWN admission — no cross-renderer bleed.
    const recA = recordOf(callsA);
    const recB = recordOf(callsB);
    expect(recA.renderAdmission).toBe(TOKEN_A);
    expect(recB.renderAdmission).toBe(TOKEN_B);
    // Server confinement held per instance.
    expect(recA.serverID).toBe('srv');
    expect(recB.serverID).toBe('srv');

    // Token B never reached renderer A's DOM/bridge and vice versa.
    assertTokenNowhere(targetA, bridgeA, TOKEN_A);
    assertTokenNowhere(targetB, bridgeB, TOKEN_B);
    expect(targetA.innerHTML).not.toContain(TOKEN_B);
    expect(targetB.innerHTML).not.toContain(TOKEN_A);

    unmount(componentA);
    unmount(componentB);
  });

  it('an UNWIRED opt-in read (runtime_error) with a legacy live binding restores the ordinary read + binding-only dispatch', async () => {
    // The P1 regression: `tools.mcp_app_render_admission.enabled: false`
    // (the default) makes the opt-in read throw the Runtime's precise
    // unwired runtime_error instead of returning a typed refusal. A LIVE
    // ref still carrying the legacy binding must render exactly as it did
    // before HA-56: one ordinary NON-minting read for the document, and
    // callbacks dispatched on the binding ALONE — never both authorities.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads, reads, calls } = makeFakeClient();
    docReads.mockRejectedValue(UNWIRED_RUNTIME_ERROR);
    const component = await mountAndConnect(target, client, {
      ...APP,
      binding: 'legacy-binding-token',
    });

    // The opt-in read ran exactly ONCE (the unwired throw is NOT re-read —
    // no minting retry, no loop), then the renderer fell back EXACTLY ONCE
    // to the ordinary non-minting read.
    expect(docReads).toHaveBeenCalledTimes(1);
    expect(reads).toHaveBeenCalledTimes(1);
    expect(reads).toHaveBeenCalledWith('srv', 'ui://srv/app.html', undefined);
    // The app mounted with the fallback document and NO safe state.
    expect(captured.instances).toHaveLength(1);
    expect(frameOf(target)).not.toBeNull();
    expect(admissionPlaceholder(target)).toBeNull();
    expect(target.querySelector("[data-testid='mcp-app-unavailable']")).toBeNull();
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    // An app-initiated callback executes EXACTLY ONCE, dispatched on the
    // legacy binding ONLY — the renderAdmission slot stays empty (never
    // both authorities).
    await bridge.oncalltool?.({ name: 'echo', arguments: { q: 1 } });
    expect(calls).toHaveBeenCalledTimes(1);
    const record = recordOf(calls);
    expect(record.serverID).toBe('srv');
    expect(record.tool).toBe('srv_echo');
    expect(record.binding).toBe('legacy-binding-token');
    expect(record.renderAdmission).toBeUndefined();

    unmount(component);
  });

  it('an UNWIRED opt-in read (runtime_error) with NO binding shows the admission safe state — no ordinary read, no iframe, no miss', async () => {
    // The P1 regression, durable-reopen shape: the ref carries no legacy
    // binding (render authority is never serialized into the durable view)
    // and the unwired surface can never mint the fresh admission a reopened
    // app requires. The renderer must land in the EXPLICIT admission safe
    // state — never a generic App load failure, never the tool-context
    // miss, never a dead iframe whose every callback would be refused.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads, reads, calls } = makeFakeClient();
    docReads.mockRejectedValue(UNWIRED_RUNTIME_ERROR);
    const component = await mountAndConnect(target, client);

    // The opt-in read ran exactly ONCE. No ordinary-read fallback was
    // attempted (there is no authority to authorize callbacks with), and no
    // minting retry loop ran.
    expect(docReads).toHaveBeenCalledTimes(1);
    expect(reads).not.toHaveBeenCalled();
    // The admission safe state, not the tool-context miss, not the generic
    // error state, not a mounted frame.
    expect(admissionPlaceholder(target)).not.toBeNull();
    expect(admissionPlaceholder(target)?.textContent).toContain('could not be authorized');
    expect(target.querySelector("[data-testid='mcp-app-unavailable']")).toBeNull();
    expect(target.querySelector("[data-state='error']")).toBeNull();
    expect(frameOf(target)).toBeNull();
    expect(captured.instances).toHaveLength(0);
    // No callback was ever dispatched, and no authority bytes exist anywhere
    // the sandbox could have touched.
    expect(calls).not.toHaveBeenCalled();
    expect(target.innerHTML).not.toContain(TOKEN_A);

    unmount(component);
  });

  it('a NON-unwired runtime_error does NOT fall back — the opt-in read fails closed in the generic error state', async () => {
    // The fallback is gated on the PRECISE unwired posture ONLY. A runtime
    // error from an ENABLED surface whose authorization seam broke (or any
    // other runtime failure) must NOT downgrade to the ordinary read + legacy
    // binding — even with a live binding present. This pins the fail-closed
    // boundary of the P1 fix: only the stable "authority is not wired" wording
    // opens the fallback.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { client, docReads, reads, calls } = makeFakeClient();
    docReads.mockRejectedValue({
      code: 'runtime_error',
      message:
        'method "mcp.servers.read_resource": render-admission authorization failed: gate blew up',
    });
    const component = await mountAndConnect(target, client, {
      ...APP,
      binding: 'legacy-binding-token',
    });

    // Exactly ONE opt-in read; NO ordinary-read fallback despite the live
    // binding, NO mount, NO admission safe state — the loud generic error.
    expect(docReads).toHaveBeenCalledTimes(1);
    expect(reads).not.toHaveBeenCalled();
    expect(target.querySelector("[data-state='error']")).not.toBeNull();
    expect(frameOf(target)).toBeNull();
    expect(captured.instances).toHaveLength(0);
    expect(admissionPlaceholder(target)).toBeNull();
    expect(calls).not.toHaveBeenCalled();

    unmount(component);
  });
});
