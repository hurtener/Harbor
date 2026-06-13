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
import type { MCPAppHostClient, MCPAppRefView } from '$lib/chat/renderers/app-bridge-host.js';
import {
  appId,
  computeRegion,
  INITIAL_LAYOUT,
  reduceLayout,
  type AppRef,
  type OpenApp
} from '$lib/components/playground/layout.js';

// A fake injected Protocol surface (D-173 — the renderer NEVER opens a direct
// MCP transport; it drives every app→host request through this). `readResource`
// records the (serverID, resourceURI) pair so the test can assert the renderer
// resolved the document from the discovered server.
function fakeHostClient(): MCPAppHostClient & { reads: Array<[string, string]> } {
  const reads: Array<[string, string]> = [];
  return {
    reads,
    async readResource(serverID, resourceURI) {
      reads.push([serverID, resourceURI]);
      return { resourceUri: resourceURI, mimeType: 'text/html', content: '<p>app body</p>' };
    },
    async callTool(tool) {
      return { tool, content: { ok: true }, isError: false };
    },
    async listResources() {
      return [];
    },
    async listTools() {
      return [];
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
