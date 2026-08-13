// The MISS-path guard for a replayed (or live) MCP App (D-348).
//
// A replayed App re-mounts from the durable `mcp.app_available` discovery and
// re-reads its captured tool context by the deterministic `toolCallId`. That
// read is the one thing that can legitimately have gone away — the record can
// be unknown, evicted, or another identity's. When it does, the host must NOT
// mount a dataless iframe whose delivery silently never arrives (CLAUDE.md §13
// silent degradation): the renderer resolves the context BEFORE mounting and
// renders a stable, honest placeholder instead.
//
// This spec mounts the REAL `McpAppRenderer` with a mocked `AppBridge` and
// pins all four outcomes:
//   1. context resolves    → iframe mounts, bridge is built, context delivered;
//   2. context is `null`   → placeholder, NO iframe, NO bridge (the miss);
//   3. no `toolCallId`     → iframe mounts, no delivery (nothing was captured);
//   4. context read THROWS → the loud error state (never swallowed).

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

import type { MCPAppHostClient, MCPAppToolContext } from './app-bridge-host.js';

interface MockBridge {
  oninitialized?: (params: unknown) => void;
  toolInputCalls: unknown[];
  toolResultCalls: unknown[];
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
    toolInputCalls: unknown[] = [];
    toolResultCalls: unknown[] = [];
    closeCalls = 0;
    constructor(..._args: unknown[]) {
      captured.instances.push(this as unknown as MockBridge);
    }
    async connect(): Promise<void> {}
    async close(): Promise<void> {
      this.closeCalls += 1;
    }
    setHostContext(): void {}
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

const McpAppRenderer = (await import('./mcp-app.svelte')).default;
const { mountRendererReactive } = await import('./mcp-app-harness.svelte.js');

/** The app ref a REPLAYED turn carries onto the bubble. */
const APP_REF = {
  resourceUri: 'ui://reports/dashboard.html',
  displayMode: 'inline' as const,
  rawHtmlTrusted: false,
  toolCallId: 'tc_9f2c1a',
};

/** A SECOND app ref whose context is gone — the miss the newer preload hits. */
const MISSING_REF = {
  resourceUri: 'ui://reports/other.html',
  displayMode: 'inline' as const,
  rawHtmlTrusted: false,
  toolCallId: 'tc_gone',
};

/** The captured context a REPLAYED app re-reads by its deterministic id. */
const CONTEXT: MCPAppToolContext = {
  tool: 'reports_render',
  input: { content: { region: 'emea' } },
  result: { content: { revenue: 42 } },
  isError: false,
};

/**
 * A host client whose `toolContext` behaves as the caller dictates:
 * a value (hit), `null` (the Runtime's `not_found` → the MISS), or a throw
 * (a real transport / scope failure).
 */
function fakeClient(toolContext: () => Promise<MCPAppToolContext | null>): MCPAppHostClient {
  return {
    async readResource(_s, uri) {
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
    toolContext,
    async fetchArtifactText() {
      return '{}';
    },
  };
}

/** Mount the renderer and drain the preload + lifecycle effects. */
async function mountRenderer(
  target: HTMLElement,
  client: MCPAppHostClient,
  toolCallId: string | undefined,
): Promise<ReturnType<typeof mount>> {
  const component = mount(McpAppRenderer, {
    target,
    props: {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app: {
        resourceUri: 'ui://reports/dashboard.html',
        displayMode: 'inline' as const,
        rawHtmlTrusted: false,
        toolCallId,
      },
      serverID: 'reports',
      appHostClient: client,
    },
  });
  // Drain the async preload (readResource → toolContext) and the lifecycle
  // effect that constructs + connects the bridge.
  for (let i = 0; i < 12; i++) {
    flushSync();
    await Promise.resolve();
  }
  flushSync();
  return component;
}

describe('McpAppRenderer — replayed-App tool-context resolution (D-348)', () => {
  afterEach(() => {
    captured.instances.length = 0;
    document.body.innerHTML = '';
  });

  it('resolves the context BEFORE mounting, then mounts the app and delivers it', async () => {
    const target = document.createElement('div');
    document.body.appendChild(target);
    const client = fakeClient(async () => CONTEXT);
    const seen = vi.spyOn(client, 'toolContext');

    const component = await mountRenderer(target, client, 'tc_9f2c1a');

    // Read by the deterministic (serverID, toolCallId) pair — no new storage,
    // no caller-controlled identifier.
    expect(seen).toHaveBeenCalledWith('reports', 'tc_9f2c1a');
    expect(target.querySelector('iframe')).not.toBeNull();
    expect(target.querySelector('[data-testid="mcp-app-unavailable"]')).toBeNull();
    expect(captured.instances).toHaveLength(1);

    // The bridge delivers the RESOLVED context after the app initializes.
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    for (let i = 0; i < 6; i++) await Promise.resolve();
    expect(bridge.toolInputCalls).toEqual([{ arguments: { region: 'emea' } }]);
    expect(bridge.toolResultCalls).toHaveLength(1);

    unmount(component);
  });

  it('an unresolvable context renders the honest placeholder and mounts NO iframe', async () => {
    // The MISS. `null` is the adapter's mapping of the Runtime's `not_found` —
    // unknown / evicted / cross-identity. Never a blank bubble, never a
    // half-mounted iframe, never a silent drop.
    const target = document.createElement('div');
    document.body.appendChild(target);

    const component = await mountRenderer(target, fakeClient(async () => null), 'tc_gone');

    const placeholder = target.querySelector('[data-testid="mcp-app-unavailable"]');
    expect(placeholder).not.toBeNull();
    expect(placeholder?.textContent).toContain('no longer available');
    // Nothing half-mounted: no iframe, and no bridge was ever constructed.
    expect(target.querySelector('iframe')).toBeNull();
    expect(captured.instances).toHaveLength(0);
    // Not silently blank either — the placeholder is announced.
    expect(placeholder?.getAttribute('role')).toBe('status');

    unmount(component);
  });

  it('an app that recorded NO correlation id mounts and simply performs no delivery', async () => {
    // Nothing was ever captured for this app, so nothing has gone missing —
    // this is NOT the miss path and must not show the placeholder.
    const target = document.createElement('div');
    document.body.appendChild(target);
    const client = fakeClient(async () => CONTEXT);
    const seen = vi.spyOn(client, 'toolContext');

    const component = await mountRenderer(target, client, undefined);

    expect(seen).not.toHaveBeenCalled();
    expect(target.querySelector('iframe')).not.toBeNull();
    expect(target.querySelector('[data-testid="mcp-app-unavailable"]')).toBeNull();
    expect(captured.instances).toHaveLength(1);

    const bridge = captured.instances[0];
    bridge.fireInitialized();
    for (let i = 0; i < 6; i++) await Promise.resolve();
    expect(bridge.toolInputCalls).toHaveLength(0);
    expect(bridge.toolResultCalls).toHaveLength(0);

    unmount(component);
  });

  it('a THROWING context read surfaces the loud error state, never a silent mount', async () => {
    // The adapter re-raises any non-`not_found` Protocol error (e.g. an
    // identity-scope rejection). That is a failure, not an eviction — it gets
    // the error state with its message, not the "no longer available" copy.
    const target = document.createElement('div');
    document.body.appendChild(target);

    const component = await mountRenderer(
      target,
      fakeClient(async () => {
        throw new Error('identity_scope_required: nope');
      }),
      'tc_9f2c1a',
    );

    const error = target.querySelector('[data-state="error"]');
    expect(error).not.toBeNull();
    expect(error?.textContent).toContain('identity_scope_required');
    expect(target.querySelector('iframe')).toBeNull();
    expect(target.querySelector('[data-testid="mcp-app-unavailable"]')).toBeNull();
    expect(captured.instances).toHaveLength(0);

    unmount(component);
  });

  it('resolved context: prop-identity churn does not rebuild or close the bridge', async () => {
    // The D-342 lifecycle guard, run through the path this phase ADDED. The
    // sibling theme-lifecycle spec proves prop churn never re-runs the bridge
    // effect, but only for an app with a RESOLVING tool context does that churn
    // traverse the new pre-mount `toolContext` await — so the invariant is
    // asserted here too, where the new async step is live.
    const target = document.createElement('div');
    document.body.appendChild(target);
    const client = fakeClient(async () => CONTEXT);
    const { component, props } = mountRendererReactive(target, {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app: { ...APP_REF },
      serverID: 'reports',
      appHostClient: client,
    });
    for (let i = 0; i < 12; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();
    expect(captured.instances).toHaveLength(1);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    // Churn the app prop's IDENTITY with an equivalent ref (the transcript does
    // this on every re-render of the turn).
    props.app = { ...APP_REF };
    for (let i = 0; i < 12; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();

    // Exactly ONE bridge, never closed — no teardown-rebuild, no refetch storm.
    expect(captured.instances).toHaveLength(1);
    expect(bridge.closeCalls).toBe(0);
    expect(target.querySelector('iframe')).not.toBeNull();

    unmount(component);
  });
});

describe('McpAppRenderer — concurrent preloads (D-348)', () => {
  // `preload` awaits twice, so an `app` prop-identity change mid-flight starts a
  // SECOND preload while the first is still parked. Only the latest may write
  // `loadState` — which IS a tracked dependency of the bridge lifecycle effect.
  // A stale write of a terminal state would fire that effect's cleanup and
  // `host.close()` a bridge mid-`ui/initialize`: the exact teardown shape that
  // got the original MCP-Apps work reverted, reached through a DATA outcome
  // (an evicted context) rather than a theme change.
  afterEach(() => {
    captured.instances.length = 0;
    document.body.innerHTML = '';
  });

  /**
   * A client whose `readResource` parks until the caller releases it, and whose
   * `toolContext` resolves for the FIRST caller and misses for every one after.
   *
   * Divergence has to come from call ORDER, not from the ref: everything after
   * the first `await` reads the LIVE `app` prop, so both in-flight preloads
   * would otherwise ask for the same context and reach the same outcome — and
   * an interleaving where both agree cannot detect a stale write at all. With
   * this client, whichever preload gets past the document await first mounts,
   * and a second one arriving behind it would report a miss — the write that
   * must never land on a bridge the first one built.
   */
  function gatedClient() {
    const releases: Array<() => void> = [];
    const base = fakeClient(async () => CONTEXT);
    let contextCalls = 0;
    return {
      releases,
      contextCalls: () => contextCalls,
      client: {
        ...base,
        readResource(_serverID: string, uri: string) {
          return new Promise<{ resourceUri: string; mimeType: string; content: string }>(
            (resolve) => {
              releases.push(() => resolve({ resourceUri: uri, mimeType: 'text/html', content: '<p>app</p>' }));
            },
          );
        },
        async toolContext() {
          contextCalls += 1;
          return contextCalls === 1 ? CONTEXT : null;
        },
      } as MCPAppHostClient,
    };
  }

  async function settle(): Promise<void> {
    for (let i = 0; i < 12; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();
  }

  it('a stale preload never lands a miss on the bridge the current one built', async () => {
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { releases, client, contextCalls } = gatedClient();
    const { component, props } = mountRendererReactive(target, {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app: { ...APP_REF },
      serverID: 'reports',
      appHostClient: client,
    });
    await settle();
    expect(releases).toHaveLength(1); // preload #1 parked on the document read

    // Churn the prop mid-flight → preload #2 starts and parks too.
    props.app = { ...MISSING_REF };
    await settle();
    expect(releases).toHaveLength(2);

    // Release the now-STALE #1 first, then the current #2.
    releases[0]();
    await settle();
    releases[1]();
    await settle();

    // Exactly one context fetch happened — the stale preload bailed before it,
    // so it neither spent a round-trip nor produced an outcome.
    expect(contextCalls()).toBe(1);
    // The current preload mounted the app: one bridge, NEVER closed. Without
    // the in-flight guard the stale one reaches `ready` first, builds a bridge,
    // and the current one's miss then writes `unavailable` — firing the
    // lifecycle effect's cleanup and closing that bridge mid-`ui/initialize`.
    expect(captured.instances).toHaveLength(1);
    expect(captured.instances[0].closeCalls).toBe(0);
    expect(target.querySelector('iframe')).not.toBeNull();
    expect(target.querySelector('[data-testid="mcp-app-unavailable"]')).toBeNull();

    unmount(component);
  });

  it('a stale preload arriving AFTER the current one cannot tear its bridge down', async () => {
    const target = document.createElement('div');
    document.body.appendChild(target);
    const { releases, client } = gatedClient();
    const { component, props } = mountRendererReactive(target, {
      mime: 'application/vnd.harbor.mcp-app',
      src: '',
      app: { ...APP_REF },
      serverID: 'reports',
      appHostClient: client,
    });
    await settle();
    props.app = { ...MISSING_REF };
    await settle();
    expect(releases).toHaveLength(2);

    // Reverse interleaving: the CURRENT preload lands first and mounts, with
    // its bridge live; the stale one resolves afterwards.
    releases[1]();
    await settle();
    expect(captured.instances).toHaveLength(1);
    const bridge = captured.instances[0];
    bridge.fireInitialized();
    flushSync();

    releases[0]();
    await settle();

    // The stale resolution changed nothing: the app is still mounted, its
    // bridge still live and never closed.
    expect(target.querySelector('iframe')).not.toBeNull();
    expect(target.querySelector('[data-testid="mcp-app-unavailable"]')).toBeNull();
    expect(captured.instances).toHaveLength(1);
    expect(bridge.closeCalls).toBe(0);

    unmount(component);
  });
});
