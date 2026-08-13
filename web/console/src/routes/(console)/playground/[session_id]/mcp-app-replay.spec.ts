// Session-reopen MCP App replay — the rehydration regression guard (D-348).
//
// A rendered `ui://` MCP App used to VANISH when the session was reopened. The
// live path attaches the decoded `mcp.app_available` ref to the agent bubble;
// the reopen path reduces the SAME durable event through `reduceHistoryTurns`
// and `hydratePastTurns` maps it onto the hydrated message. This spec proves
// leave-and-return ≡ live at the RENDER level: the same real `MessageBubble`
// mounts the same real `McpAppRenderer`, resolving the same document from the
// same server, whether the message came from the live stream or from the
// durable replay.
//
// It also pins the miss half: a replayed App whose persisted tool context can
// no longer be resolved renders the honest placeholder, never a blank bubble
// and never a half-mounted iframe (CLAUDE.md §13).

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

import MessageBubble from '$lib/chat/MessageBubble.svelte';
import type { ChatMessage, ChatProtocolClient } from '$lib/chat/types.js';
import type { MCPAppHostClient, MCPAppToolContext } from '$lib/chat/renderers/app-bridge-host.js';
import { reduceHistoryTurns, type HistoryTurn } from '$lib/sessions/history.js';
import type { StateEvent } from '$lib/protocol/state.js';

import { appViewFromDiscovery, hydratedAgentMessage } from './turn-projection.js';
import { decodeAppAvailable } from './wire-events.js';

/** The runtime's `AppAvailablePayload`, PascalCase as the durable log persists it. */
const APP_PAYLOAD: Record<string, unknown> = {
  AgentID: 'agent-reports',
  ServerID: 'reports',
  ToolCallID: 'tc_9f2c1a',
  ToolName: 'reports_render',
  ResourceURI: 'ui://reports/dashboard.html',
  DisplayMode: 'inline',
  RawHTMLTrusted: false
  ,Binding: 'must-not-replay'
};

const CONTEXT: MCPAppToolContext = {
  tool: 'reports_render',
  input: { content: { region: 'emea' } },
  result: { content: { revenue: 42 } },
  isError: false
};

let seq = 0;
function ev(type: string, run: string, payload: unknown, at = '2026-07-10T12:00:00Z'): StateEvent {
  return { type, sequence: ++seq, occurred_at: at, tenant: 't', user: 'u', session: 's', run, payload };
}

/** The durable window a reopen loads for an App-bearing turn. */
function appTurnWindow(): StateEvent[] {
  return [
    ev('task.started', 'r1', {}, '2026-07-10T12:00:00Z'),
    ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'reports_render' }),
    ev('tool.invoked', 'r1', { ToolName: 'reports_render' }),
    ev('mcp.app_available', 'r1', APP_PAYLOAD),
    ev('tool.completed', 'r1', { ToolName: 'reports_render', DurationMS: 1200 }),
    ev('llm.completion.chunk', 'r1', { Delta: 'Here is the dashboard.', Kind: 'content' }),
    ev('task.completed', 'r1', {}, '2026-07-10T12:00:03Z')
  ];
}

/**
 * The LIVE message: the bubble the page holds once the SSE
 * `mcp.app_available` frame has been decoded and projected onto it — driven
 * through the REAL decoder and the REAL projection, not a re-implementation.
 */
function liveMessage(): ChatMessage {
  const decoded = decodeAppAvailable(
    JSON.stringify({ type: 'mcp.app_available', run: 'r1', payload: APP_PAYLOAD })
  );
  if (decoded === null) throw new Error('live decoder rejected the frame');
  const { app, serverID } = appViewFromDiscovery(decoded);
  return {
    id: 'live-r1-a',
    role: 'agent',
    text: 'Here is the dashboard.',
    at: '2026-07-10T12:00:00Z',
    taskID: 'r1',
    app,
    serverID
  };
}

/**
 * The REPLAYED message: the REAL projection `hydratePastTurns` applies to a
 * reduced turn. Driving the shipped function (not a copy) is what makes this
 * spec guard the integration point — deleting the App fields from the mapping
 * fails here.
 */
function replayedMessage(turn: HistoryTurn): ChatMessage {
  const m = hydratedAgentMessage(turn, { at: turn.at, durationMs: 0, toolCalls: [] });
  if (m === null) throw new Error('the turn projected to no agent message');
  if (m.app?.binding) throw new Error('replay unexpectedly restored callback authority');
  return m;
}

/** A fake injected Protocol surface (D-173) recording what it was asked for. */
function fakeHostClient(
  toolContext: () => Promise<MCPAppToolContext | null> = async () => CONTEXT
): MCPAppHostClient & { reads: Array<[string, string, string | undefined]>; contexts: Array<[string, string]> } {
  const reads: Array<[string, string, string | undefined]> = [];
  const contexts: Array<[string, string]> = [];
  return {
    reads,
    contexts,
    async readResource(serverID, resourceURI, agentID) {
      reads.push([serverID, resourceURI, agentID]);
      return { resourceUri: resourceURI, mimeType: 'text/html', content: '<p>dashboard</p>' };
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
    async toolContext(serverID, toolCallID) {
      contexts.push([serverID, toolCallID]);
      return toolContext();
    },
    async fetchArtifactText() {
      return '{}';
    }
  };
}

const noopChatClient = {} as unknown as ChatProtocolClient;

const mounts: Array<ReturnType<typeof mount>> = [];
const targets: HTMLElement[] = [];

/** Mount a bubble and drain the renderer's async preload + lifecycle effects. */
async function renderBubble(message: ChatMessage, client: MCPAppHostClient): Promise<HTMLElement> {
  const target = document.createElement('div');
  document.body.appendChild(target);
  targets.push(target);
  // `mount`'s typed overload demands props matching the concrete component;
  // this harness erases that at the call boundary (the shape mirrors the
  // sibling discovery spec's helper).
  const props: Record<string, unknown> = {
    message,
    client: noopChatClient,
    appHostClient: client,
    availableDisplayModes: ['inline', 'fullscreen', 'pip']
  };
  mounts.push(
    mount(MessageBubble as Parameters<typeof mount>[0], {
      target,
      props: props as Record<string, never>
    })
  );
  for (let i = 0; i < 12; i++) {
    flushSync();
    await Promise.resolve();
  }
  flushSync();
  return target;
}

afterEach(() => {
  for (const m of mounts.splice(0)) unmount(m);
  for (const t of targets.splice(0)) t.remove();
});

describe('session-reopen MCP App replay (D-348)', () => {
  it('a reopened turn renders the App equivalently to the live view', async () => {
    const turn = reduceHistoryTurns(appTurnWindow())[0];
    expect(turn.app, 'the reducer folded the App onto the turn').toBeDefined();

    const liveClient = fakeHostClient();
    const replayClient = fakeHostClient();
    const liveRoot = await renderBubble(liveMessage(), liveClient);
    const replayRoot = await renderBubble(replayedMessage(turn), replayClient);

    // Both mounted the REAL renderer inside the bubble's app slot.
    for (const root of [liveRoot, replayRoot]) {
      expect(root.querySelector("[data-renderer-source='mcp-app']")).not.toBeNull();
      expect(root.querySelector("[data-testid='chat-mcp-app']")).not.toBeNull();
      expect(root.querySelector("[data-testid='mcp-app-unavailable']")).toBeNull();
    }

    // Same document, resolved from the same server, through the injected client.
    expect(replayClient.reads).toEqual(liveClient.reads);
    expect(replayClient.reads).toEqual([['reports', 'ui://reports/dashboard.html', 'agent-reports']]);

    // Same PERSISTED tool context, read by the same deterministic id — no new
    // storage and no caller-controlled identifier on the replay path.
    expect(replayClient.contexts).toEqual(liveClient.contexts);
    expect(replayClient.contexts).toEqual([['reports', 'tc_9f2c1a']]);

    // Same iframe posture (sandbox tokens + trust flag) — a replayed App is
    // never granted more than a live one.
    const liveFrame = liveRoot.querySelector('iframe');
    const replayFrame = replayRoot.querySelector('iframe');
    expect(liveFrame).not.toBeNull();
    expect(replayFrame).not.toBeNull();
    expect(replayFrame?.getAttribute('sandbox')).toBe(liveFrame?.getAttribute('sandbox'));
    expect(replayFrame?.getAttribute('sandbox') ?? '').not.toContain('allow-same-origin');
    expect(replayFrame?.getAttribute('data-trusted')).toBe(liveFrame?.getAttribute('data-trusted'));
    expect(replayFrame?.getAttribute('srcdoc')).toBe(liveFrame?.getAttribute('srcdoc'));

    // Same negotiated display mode.
    expect(
      replayRoot.querySelector("[data-renderer-source='mcp-app']")?.getAttribute('data-display-mode')
    ).toBe(
      liveRoot.querySelector("[data-renderer-source='mcp-app']")?.getAttribute('data-display-mode')
    );
  });

  it('a replayed App whose persisted context is gone renders the honest placeholder', async () => {
    // The miss the reopen path is most likely to hit: the discovery replays
    // fine, but the record behind its deterministic id is unknown / evicted /
    // another identity's. The turn must say so — never a blank bubble, never a
    // half-mounted iframe whose data silently never arrives.
    const turn = reduceHistoryTurns(appTurnWindow())[0];
    const client = fakeHostClient(async () => null);
    const root = await renderBubble(replayedMessage(turn), client);

    const placeholder = root.querySelector("[data-testid='mcp-app-unavailable']");
    expect(placeholder).not.toBeNull();
    expect(placeholder?.textContent).toContain('no longer available');
    expect(root.querySelector('iframe')).toBeNull();
    // The rest of the turn is untouched — the answer text still renders.
    expect(root.textContent).toContain('Here is the dashboard.');
  });

  it('a turn with no App reopens with no app slot at all (no phantom placeholder)', async () => {
    const turn = reduceHistoryTurns([
      ev('task.started', 'r2', {}, '2026-07-10T12:00:00Z'),
      ev('llm.completion.chunk', 'r2', { Delta: 'Plain answer.', Kind: 'content' }),
      ev('task.completed', 'r2', {}, '2026-07-10T12:00:01Z')
    ])[0];
    expect(turn.app).toBeUndefined();

    const root = await renderBubble(replayedMessage(turn), fakeHostClient());
    expect(root.querySelector("[data-testid='chat-mcp-app']")).toBeNull();
    expect(root.querySelector("[data-testid='mcp-app-unavailable']")).toBeNull();
    expect(root.textContent).toContain('Plain answer.');
  });
});
