// The re-landed MCP-Apps HOST OBLIGATIONS, at the component level.
//
// The `io.modelcontextprotocol/ui` ext-apps dialect is a two-way contract: a
// conformant App relies on the host to consume its notifications and to
// populate the `ui/initialize` host-context. These obligations were written
// once and reverted wholesale with the rest of the Console MCP-Apps surface;
// this spec is the regression guard for each of them.
//
// Covered here (the renderer half — the pure-handler half lives in
// `app-bridge-host.spec.ts`):
//
//   - `ui/notifications/size-changed` is consumed and drives the inline frame's
//     height, coalesced, with the clamp delegated to the host's own CSS tokens.
//     An App that never reports keeps the previous fixed-height behaviour.
//   - `ui/resource-teardown` is sent on unmount — and NEVER before the App has
//     completed `ui/initialize` (the handshake-safety rule the revert taught).
//   - `ui/notifications/request-teardown` is granted: teardown, close, unmount.
//   - host-context `toolInfo` + `containerDimensions` reach `ui/initialize`.
//
// AND, threaded through every one of them, the four D-342 lifecycle invariants:
// ONE bridge construction with the FINAL host-context, a lifecycle effect
// isolated from reactivity, every host→app send gated behind `oninitialized`,
// and never a teardown-rebuild. Each new obligation touches exactly that
// machinery, so each is asserted against it rather than in isolation.
//
// The bridge is mocked (a real `ui/initialize` over a real iframe is the
// Playwright spec's job); what is under test here is OUR lifecycle wiring.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import type { McpUiHostContext } from '@modelcontextprotocol/ext-apps/app-bridge';

import type { MCPAppHostClient } from './app-bridge-host.js';

interface MockBridge {
  hostContext: McpUiHostContext | undefined;
  oninitialized?: (params: unknown) => void;
  onsizechange?: (params: { width?: number; height?: number }) => void;
  onrequestteardown?: (params: unknown) => void;
  onlistresourcetemplates?: () => Promise<unknown>;
  setHostContextCalls: McpUiHostContext[];
  teardownCalls: unknown[];
  closeCalls: number;
  /** Order-of-operations tape: 'teardown' / 'close', appended as they happen. */
  tape: string[];
  teardownRejects: boolean;
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
    onsizechange: ((params: { width?: number; height?: number }) => void) | undefined;
    onrequestteardown: ((params: unknown) => void) | undefined;
    oninitialized: ((params: unknown) => void) | undefined;
    hostContext: McpUiHostContext | undefined;
    setHostContextCalls: McpUiHostContext[] = [];
    teardownCalls: unknown[] = [];
    closeCalls = 0;
    tape: string[] = [];
    teardownRejects = false;
    constructor(..._args: unknown[]) {
      // The vendored constructor signature is
      // `(client, hostInfo, capabilities, { hostContext })`.
      const opts = _args[3] as { hostContext?: McpUiHostContext } | undefined;
      this.hostContext = opts?.hostContext;
      captured.instances.push(this as unknown as MockBridge);
    }
    async connect(): Promise<void> {}
    async close(): Promise<void> {
      this.closeCalls += 1;
      this.tape.push('close');
    }
    setHostContext(ctx: McpUiHostContext): void {
      this.setHostContextCalls.push(ctx);
    }
    async teardownResource(params: unknown): Promise<Record<string, unknown>> {
      this.teardownCalls.push(params);
      this.tape.push('teardown');
      if (this.teardownRejects) throw new Error('app is wedged');
      return {};
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

function installMatchMedia(): void {
  (window as unknown as { matchMedia: (q: string) => MediaQueryList }).matchMedia = () =>
    ({
      matches: true,
      media: '(prefers-color-scheme: dark)',
      addEventListener: () => {},
      removeEventListener: () => {},
    }) as unknown as MediaQueryList;
}

function fakeClient(): MCPAppHostClient {
  return {
    async readResource(_s, uri) {
      return { resourceUri: uri, mimeType: 'text/html', content: '<p>app body</p>' };
    },
    async readRenderDocument(_s, uri) {
      return { resourceUri: uri, mimeType: 'text/html', content: '<p>app body</p>' };
    },
    async callTool(_serverID, tool) {
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
}

const APP = {
  resourceUri: 'ui://srv/app.html',
  displayMode: 'inline' as const,
  rawHtmlTrusted: false,
  toolCallId: 'tc_1',
  // The BARE server-side tool name, which is what the runtime puts on
  // `mcp.app_available` (the driver publishes the name it called the MCP server
  // with, not Harbor's `<source>_<tool>` catalog key). Using a qualified name
  // here would encode the wrong model in the very file documenting the
  // namespace confinement.
  toolName: 'report',
};

/** Mount and drain the preload + lifecycle effects so the bridge connects. */
async function mountAndConnect(
  target: HTMLElement,
  app: typeof APP | Record<string, unknown> = APP,
): Promise<ReturnType<typeof mount>> {
  const component = mount(McpAppRenderer, {
    target,
    props: {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app,
      serverID: 'srv',
      appHostClient: fakeClient(),
    } as never,
  });
  for (let i = 0; i < 12 && captured.instances.length === 0; i++) {
    flushSync();
    await Promise.resolve();
  }
  flushSync();
  return component;
}

/**
 * Let a queued animation frame (jsdom schedules rAF on a ~16 ms timer) and any
 * pending microtask drain, then flush Svelte. Real-time, not a sleep-as-sync:
 * the rAF callback IS the thing being waited on, and the loop exits as soon as
 * a caller-supplied condition holds.
 */
async function settleFrame(done?: () => boolean): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 5));
    flushSync();
    if (done?.() ?? false) return;
  }
  if (!done) flushSync();
}

function frameOf(target: HTMLElement): HTMLIFrameElement | null {
  return target.querySelector('iframe');
}

describe('MCP Apps host obligations — size-changed (HA-38)', () => {
  afterEach(() => {
    captured.instances.length = 0;
  });

  it('consumes size-changed and drives the inline frame height from the reported value', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);

    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    // Baseline: nothing reported yet, so the frame carries NO inline height and
    // keeps exactly the pre-existing fixed `min-height` behaviour.
    expect(frameOf(target)?.style.height).toBe('');

    expect(bridge.onsizechange).toBeTypeOf('function');
    bridge.onsizechange?.({ width: 400, height: 320 });
    await settleFrame();

    expect(frameOf(target)?.style.height).toBe('320px');

    unmount(component);
    document.body.removeChild(target);
  });

  it('coalesces a resize storm — only the LAST report in a frame is applied', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    for (const h of [200, 240, 260, 999]) bridge.onsizechange?.({ height: h });
    await settleFrame();
    // One applied value, and it is the last one — not four layout thrashes.
    expect(frameOf(target)?.style.height).toBe('999px');

    unmount(component);
    document.body.removeChild(target);
  });

  it('ignores a nonsense height — the previous valid height survives', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    bridge.onsizechange?.({ height: 300 });
    await settleFrame(() => frameOf(target)?.style.height === '300px');
    expect(frameOf(target)?.style.height).toBe('300px');

    // Each nonsense shape is rejected INDIVIDUALLY — batching them would let a
    // dropped guard hide behind whichever report happened to land last.
    for (const bad of [{ height: 0 }, { height: Number.NaN }, { height: -50 }, {}]) {
      bridge.onsizechange?.(bad);
      await settleFrame();
      expect(frameOf(target)?.style.height, `rejected ${JSON.stringify(bad)}`).toBe('300px');
    }

    unmount(component);
    document.body.removeChild(target);
  });

  it('D-342: a size report NEVER closes or reconstructs the bridge', async () => {
    // `appHeightPx` is reactive state the TEMPLATE reads. If it ever became a
    // tracked dependency of the bridge-owning lifecycle effect, every resize
    // would tear the transport down — the reverted-work outage reached through
    // a new door. Exactly one bridge, zero closes, after a resize storm.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    for (const h of [180, 260, 400, 260]) {
      bridge.onsizechange?.({ height: h });
      await settleFrame();
    }

    expect(captured.instances).toHaveLength(1);
    expect(bridge.closeCalls).toBe(0);

    unmount(component);
    document.body.removeChild(target);
  });
  it('does NOT clamp or size-drive the frame in fullscreen / pip', async () => {
    // The page-level AppPanel reuses `.mcp-app__frame` and sizes it to fill the
    // panel. An inline growth envelope applied there CAPS the panel — a 900px
    // fullscreen frame clamped to the inline maximum. So both the envelope
    // modifier and the reported-height inline style are gated on `isInline`:
    // the surface that owns the layout owns the bound.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);

    const component = await mountAndConnect(target, { ...APP, displayMode: 'fullscreen' });
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    const frame = frameOf(target);
    expect(frame?.classList.contains('mcp-app__frame--inline')).toBe(false);

    // An app in fullscreen still reports its size; the host must not act on it.
    bridge.onsizechange?.({ height: 900 });
    await settleFrame();
    expect(frame?.style.height, 'no inline height is imposed on a page-level panel').toBe('');
    expect(frame?.getAttribute('data-app-height')).toBeNull();

    unmount(component);
    document.body.removeChild(target);
  });

  it('DOES carry the inline envelope when inline (the control case)', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    const frame = frameOf(target);
    expect(frame?.classList.contains('mcp-app__frame--inline')).toBe(true);
    bridge.onsizechange?.({ height: 420 });
    await settleFrame(() => frameOf(target)?.style.height === '420px');
    expect(frame?.style.height).toBe('420px');

    unmount(component);
    document.body.removeChild(target);
  });
});

describe('MCP Apps host obligations — graceful teardown', () => {
  afterEach(() => {
    captured.instances.length = 0;
  });

  it('sends ui/resource-teardown BEFORE close on unmount', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    unmount(component);
    await settleFrame();
    await settleFrame();

    expect(bridge.teardownCalls).toHaveLength(1);
    // Order is load-bearing: an app cannot clean up over a closed transport.
    expect(bridge.tape).toEqual(['teardown', 'close']);

    document.body.removeChild(target);
  });

  it('D-342: NEVER sends teardown for a bridge that has not completed ui/initialize', async () => {
    // THE handshake-safety rule. A bridge torn down before the app finished
    // `ui/initialize` (a stale preload losing its generation race, an app that
    // never boots) must close SILENTLY. Posting a request onto a transport the
    // app has not finished initializing on is the exact shape that produced the
    // 30s `ui/initialize` timeout behind the revert.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];
    // Deliberately do NOT fire `oninitialized`.

    unmount(component);
    await settleFrame();
    await settleFrame();

    expect(bridge.teardownCalls).toHaveLength(0);
    expect(bridge.tape).toEqual(['close']);

    document.body.removeChild(target);
  });

  it('closes anyway when the app never acknowledges the teardown (fail-safe unmount)', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];
    bridge.teardownRejects = true;
    bridge.fireInitialized();
    flushSync();

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    unmount(component);
    await settleFrame();
    await settleFrame();

    // The courtesy failed loudly (logged) but the guarantee held: the transport
    // is closed. A wedged app can never pin an effect cleanup open.
    expect(bridge.closeCalls).toBe(1);
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();

    document.body.removeChild(target);
  });

  it('grants an app-initiated request-teardown: teardown, close, unmount the frame', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();
    expect(frameOf(target)).not.toBeNull();

    expect(bridge.onrequestteardown).toBeTypeOf('function');
    bridge.onrequestteardown?.({});
    await settleFrame();
    await settleFrame();

    expect(bridge.tape).toEqual(['teardown', 'close']);
    // The frame is gone and an honest placeholder is in its place — never a
    // dead iframe left in the transcript.
    expect(frameOf(target)).toBeNull();
    expect(target.querySelector('[data-testid="mcp-app-closed"]')).not.toBeNull();

    // D-342: no rebuild. The app asked to be gone; it must stay gone.
    expect(captured.instances).toHaveLength(1);

    unmount(component);
    document.body.removeChild(target);
  });
});

describe('MCP Apps host obligations — ui/initialize host-context', () => {
  afterEach(() => {
    captured.instances.length = 0;
  });

  it('D-342: the bridge is constructed ONCE with the FINAL host-context', async () => {
    // toolInfo + containerDimensions are baked in at construction, alongside the
    // theme/styles D-342 already pinned — never patched in mid-handshake.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);

    expect(captured.instances).toHaveLength(1);
    const ctx = captured.instances[0].hostContext;
    expect(ctx?.toolInfo).toEqual({
      id: 'tc_1',
      tool: { name: 'report', inputSchema: { type: 'object' } },
    });
    // The container snapshot is present as the spec's `{ width, maxHeight? }`
    // shape whenever the frame measured a positive width (jsdom reports 0, so
    // assert the type contract rather than a layout value).
    expect(ctx).toHaveProperty('theme');
    expect(ctx?.availableDisplayModes).toEqual(['inline']);
    // No patch was posted during the handshake.
    expect(captured.instances[0].setHostContextCalls).toHaveLength(0);

    unmount(component);
    document.body.removeChild(target);
  });

  it('containerDimensions reaches the ui/initialize host-context', async () => {
    // Driven at the AppBridgeHost seam rather than through the renderer: jsdom
    // reports a zero-width layout box, so the renderer's measurement correctly
    // yields nothing there and could not distinguish "measured empty" from
    // "the slot was dropped". Passing the measurement explicitly tests the
    // thing that actually has to hold — that the value lands in the spec slot.
    const { AppBridgeHost } = await import('./app-bridge-host.js');
    const host = new AppBridgeHost({
      client: fakeClient(),
      serverID: 'srv',
      containerDimensions: { width: 640, maxHeight: 480 },
    });
    expect(host.mode).toBe('manual-handler');
    expect(captured.instances.at(-1)?.hostContext?.containerDimensions).toEqual({
      width: 640,
      maxHeight: 480,
    });
  });

  it('omits containerDimensions when the container could not be measured', async () => {
    const { AppBridgeHost } = await import('./app-bridge-host.js');
    const host = new AppBridgeHost({ client: fakeClient(), serverID: 'srv' });
    expect(host.mode).toBe('manual-handler');
    expect(captured.instances.at(-1)?.hostContext?.containerDimensions).toBeUndefined();
  });

  it('omits toolInfo entirely when the discovery carried no tool name', async () => {
    // Honest absence beats a fabricated name: a spec-conformant app reads
    // `toolInfo.tool.name`, and inventing one would be a lie about the call.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target, {
      resourceUri: 'ui://srv/app.html',
      displayMode: 'inline',
      rawHtmlTrusted: false,
      toolCallId: 'tc_1',
    });

    expect(captured.instances[0].hostContext?.toolInfo).toBeUndefined();

    unmount(component);
    document.body.removeChild(target);
  });

  it('wires the resources/templates/list handler onto the bridge', async () => {
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);
    const component = await mountAndConnect(target);

    expect(captured.instances[0].onlistresourcetemplates).toBeTypeOf('function');
    await expect(captured.instances[0].onlistresourcetemplates?.()).resolves.toEqual({
      resourceTemplates: [],
    });

    unmount(component);
    document.body.removeChild(target);
  });
});

describe('MCP Apps host obligations — stale-bridge callback isolation', () => {
  afterEach(() => {
    captured.instances.length = 0;
  });

  it('a STALE bridge cannot close the SAME component’s newer app, nor drive its frame', async () => {
    // The window is real and it is INSIDE one component: `close()` awaits a
    // teardown the APP acks, so a never-acking app stalls it for the full
    // timeout. Meanwhile the `app` prop churns (the replay scenario), the
    // lifecycle effect tears down and rebuilds, and a NEW bridge owns the same
    // component state — and only THEN does the stale bridge's callback fire.
    //
    // `loadState = 'closed'` is deliberately STICKY, so a stale teardown
    // callback would kill the live successor permanently. The preload
    // generation token guards PRELOAD writes only — it is keyed to the preload,
    // not the bridge — so it does not cover this. The guard is bridge-instance
    // identity, captured per callback.
    installMatchMedia();
    const target = document.createElement('div');
    document.body.appendChild(target);

    const { component, props } = mountRendererReactive(target, {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app: { ...APP },
      serverID: 'srv',
      appHostClient: fakeClient()
    } as never);
    for (let i = 0; i < 12 && captured.instances.length === 0; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();
    const stale = captured.instances[0];
    stale.fireInitialized();
    flushSync();

    // Churn to a genuinely DIFFERENT app so the preload re-runs, `loadState`
    // leaves 'ready', the lifecycle effect tears the first bridge down, and a
    // second bridge is built for the same component.
    (props as unknown as { app: Record<string, unknown> }).app = {
      resourceUri: 'ui://srv/other.html',
      displayMode: 'inline',
      rawHtmlTrusted: false,
      toolCallId: 'tc_2',
      toolName: 'other'
    };
    for (let i = 0; i < 30 && captured.instances.length < 2; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();
    expect(captured.instances.length, 'a second bridge owns the component').toBe(2);
    const current = captured.instances[1];
    expect(current).not.toBe(stale);
    current.fireInitialized();
    flushSync();

    // The stale bridge now fires late. Every callback must be ignored.
    stale.onsizechange?.({ height: 77 });
    stale.onrequestteardown?.({});
    stale.oninitialized?.({});
    await settleFrame();
    await settleFrame();

    // The successor survived: no sticky `closed` state, iframe still mounted,
    // and its height was never driven from the dead app's report.
    expect(
      target.querySelector('[data-testid="mcp-app-closed"]'),
      'a stale teardown must not close the live app'
    ).toBeNull();
    expect(frameOf(target), 'the successor iframe is still mounted').not.toBeNull();
    expect(frameOf(target)?.style.height).not.toBe('77px');

    // …and the CURRENT bridge's own callbacks still work, so the guard is a
    // filter on staleness, not a blanket mute.
    current.onsizechange?.({ height: 333 });
    await settleFrame(() => frameOf(target)?.style.height === '333px');
    expect(frameOf(target)?.style.height).toBe('333px');

    unmount(component);
    document.body.removeChild(target);
  });
});
