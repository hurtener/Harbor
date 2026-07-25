// Vitest for MCP App replay in session-history hydration (D-348).
//
// A rendered `ui://` MCP App used to VANISH on reopen: `reduceHistoryTurns`
// did not fold `mcp.app_available` and `HistoryTurn` had nowhere to put it, so
// a reopened transcript degraded to the deliberately-terse model-facing tool
// text (the rich payload lives in `structuredContent`, out of model context by
// design) and read as a broken or empty turn.
//
// The reducer now reconstructs the SAME `MCPAppRefView` the LIVE discovery
// path builds, from the SAME durable event, so the renderer that mounts a live
// App mounts a replayed one unchanged. These fixtures mirror the runtime's
// PascalCase wire payload (`AppAvailablePayload`, persisted as the Go struct);
// a snake_case-tolerance case is included.

import { describe, it, expect } from 'vitest';
import { loadSessionHistory, reduceHistoryTurns, type HistoryTurn } from '../history.js';
import type { ProtocolClient } from '../../protocol/client.js';
import type { StateEvent } from '../../protocol/state.js';
// The REAL live-path producers — the `mcp.app_available` decoder and the
// projection the Playground page applies to the bubble. Importing BOTH (rather
// than re-implementing the projection here) is what makes the equivalence pin
// below a real gate: a one-sided change to the live projection — a dropped
// `toolCallId`, a widened known-mode set — fails it.
import { decodeAppAvailable } from '../../../routes/(console)/playground/[session_id]/wire-events.js';
import {
	appViewFromDiscovery,
	type AppAttachment
} from '../../../routes/(console)/playground/[session_id]/turn-projection.js';

let seq = 0;
function ev(type: string, run: string, payload: unknown, at = '2026-07-10T12:00:00Z'): StateEvent {
	return { type, sequence: ++seq, occurred_at: at, tenant: 't', user: 'u', session: 's', run, payload };
}

function turnOf(turns: HistoryTurn[], runID: string): HistoryTurn {
	const t = turns.find((x) => x.runID === runID);
	if (t === undefined) throw new Error(`no turn ${runID}`);
	return t;
}

/** The runtime's `AppAvailablePayload`, PascalCase as the durable log persists it. */
function appPayload(over: Record<string, unknown> = {}): Record<string, unknown> {
	return {
		ServerID: 'reports',
		ToolCallID: 'tc_9f2c1a',
		ToolName: 'reports_render',
		ResourceURI: 'ui://reports/dashboard.html',
		DisplayMode: 'inline',
		RawHTMLTrusted: false,
		...over
	};
}

/**
 * Drive the REAL live path end to end for one payload: the SSE frame through
 * the real decoder, then through the real projection the page applies to the
 * bubble. No re-implementation — both halves are the shipped functions.
 */
function livePathAppView(payload: Record<string, unknown>): AppAttachment {
	const frame = JSON.stringify({ type: 'mcp.app_available', run: 'r1', payload });
	const decoded = decodeAppAvailable(frame);
	if (decoded === null) throw new Error('live decoder rejected the frame');
	return appViewFromDiscovery(decoded);
}

describe('reduceHistoryTurns — MCP App replay (D-348)', () => {
	it('folds mcp.app_available into the turn as the live MCPAppRefView shape', () => {
		const t = turnOf(reduceHistoryTurns([ev('mcp.app_available', 'r1', appPayload())]), 'r1');
		expect(t.serverID).toBe('reports');
		expect(t.app).toEqual({
			resourceUri: 'ui://reports/dashboard.html',
			displayMode: 'inline',
			rawHtmlTrusted: false,
			// The deterministic content-hash key the re-mount reads the PERSISTED
			// tool context by — no new storage, no caller-controlled identifier.
			toolCallId: 'tc_9f2c1a'
		});
	});

	it('the replayed ref equals the LIVE decoder + page projection for the same payload', () => {
		// The cross-producer pin: reopen must reconstruct identically to what the
		// live SSE path attached to the bubble, or the reopened App negotiates a
		// different display mode / trust posture / correlation key.
		const payload = appPayload({ DisplayMode: 'pip', RawHTMLTrusted: true });
		const live = livePathAppView(payload);
		const t = turnOf(reduceHistoryTurns([ev('mcp.app_available', 'r1', payload)]), 'r1');
		expect(t.app).toEqual(live.app);
		expect(t.serverID).toBe(live.serverID);
	});

	it('normalises an unknown or absent display-mode hint to "" (the renderer default)', () => {
		const unknown = turnOf(
			reduceHistoryTurns([ev('mcp.app_available', 'r1', appPayload({ DisplayMode: 'hologram' }))]),
			'r1'
		);
		expect(unknown.app?.displayMode).toBe('');
		const absent = turnOf(
			reduceHistoryTurns([ev('mcp.app_available', 'r1', appPayload({ DisplayMode: '' }))]),
			'r1'
		);
		expect(absent.app?.displayMode).toBe('');
	});

	it('tolerates snake_case payload keys', () => {
		const t = turnOf(
			reduceHistoryTurns([
				ev('mcp.app_available', 'r1', {
					server_id: 'reports',
					tool_call_id: 'tc_1',
					resource_uri: 'ui://reports/x.html',
					display_mode: 'fullscreen',
					raw_html_trusted: true
				})
			]),
			'r1'
		);
		expect(t.serverID).toBe('reports');
		expect(t.app).toEqual({
			resourceUri: 'ui://reports/x.html',
			displayMode: 'fullscreen',
			rawHtmlTrusted: true,
			toolCallId: 'tc_1'
		});
	});

	it('declares NO app when the frame is missing serverID or resourceUri', () => {
		// Both are load-bearing for the mount (`readResource(serverID, uri)`), so
		// a frame missing either declares no App at all — the same guard the live
		// decoder applies. Never a half-formed ref that could only mount broken.
		const noServer = turnOf(
			reduceHistoryTurns([ev('mcp.app_available', 'r1', appPayload({ ServerID: '' }))]),
			'r1'
		);
		expect(noServer.app).toBeUndefined();
		expect(noServer.serverID).toBeUndefined();
		const noURI = turnOf(
			reduceHistoryTurns([ev('mcp.app_available', 'r1', appPayload({ ResourceURI: '' }))]),
			'r1'
		);
		expect(noURI.app).toBeUndefined();
	});

	it('preserves ordering and interleaving with the turn tool calls and answer', () => {
		// The App is discovered from a tool RESULT, so it lands between the tool
		// lifecycle events and the answer deltas. The fold must leave both intact.
		const t = turnOf(
			reduceHistoryTurns([
				ev('task.started', 'r1', {}, '2026-07-10T12:00:00Z'),
				ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'reports_render' }),
				ev('tool.invoked', 'r1', { ToolName: 'reports_render' }),
				ev('mcp.app_available', 'r1', appPayload()),
				ev('tool.completed', 'r1', { ToolName: 'reports_render', DurationMS: 1200 }),
				ev('llm.completion.chunk', 'r1', { Delta: 'Here is the dashboard.', Kind: 'content' }),
				ev('task.completed', 'r1', {}, '2026-07-10T12:00:03Z')
			]),
			'r1'
		);
		expect(t.toolCalls).toEqual([{ tool: 'reports_render', status: 'succeeded', summary: '1.2s' }]);
		expect(t.answer).toBe('Here is the dashboard.');
		expect(t.app?.resourceUri).toBe('ui://reports/dashboard.html');
		expect(t.terminal).toBe(true);
	});

	it('last-wins when a turn declares two Apps (mirrors the live reducer)', () => {
		const t = turnOf(
			reduceHistoryTurns([
				ev('mcp.app_available', 'r1', appPayload({ ResourceURI: 'ui://reports/first.html' })),
				ev('mcp.app_available', 'r1', appPayload({ ResourceURI: 'ui://reports/second.html' }))
			]),
			'r1'
		);
		expect(t.app?.resourceUri).toBe('ui://reports/second.html');
	});

	it('keeps each run App isolated across two runs in one window', () => {
		const turns = reduceHistoryTurns([
			ev('mcp.app_available', 'runA', appPayload({ ResourceURI: 'ui://reports/a.html' })),
			ev('mcp.app_available', 'runB', appPayload({ ServerID: 'charts', ResourceURI: 'ui://charts/b.html' }))
		]);
		expect(turnOf(turns, 'runA').app?.resourceUri).toBe('ui://reports/a.html');
		expect(turnOf(turns, 'runA').serverID).toBe('reports');
		expect(turnOf(turns, 'runB').app?.resourceUri).toBe('ui://charts/b.html');
		expect(turnOf(turns, 'runB').serverID).toBe('charts');
	});

	it('a page boundary neither duplicates nor reorders the App discovery', async () => {
		// Drive the REAL windowed loader rather than concatenating two arrays by
		// hand: `loadSessionHistory` walks tail-first by cursor and re-orders the
		// pages oldest-first, which is precisely the step a hand-built fixture
		// assumes instead of proving. The run's discoveries straddle the seam, so
		// only the loader's ordering makes the LATER one win.
		const olderPage: StateEvent[] = [
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'reports_render' }),
			ev('mcp.app_available', 'r1', appPayload({ ResourceURI: 'ui://reports/first.html' }))
		];
		const newerPage: StateEvent[] = [
			ev('mcp.app_available', 'r1', appPayload({ ResourceURI: 'ui://reports/second.html' })),
			ev('tool.completed', 'r1', { ToolName: 'reports_render', DurationMS: 300 })
		];
		// The surface returns the NEWEST window first and points at the older one
		// through `next_cursor`; each window is internally oldest-first.
		const pages = [
			{ events: newerPage, has_more: true, next_cursor: olderPage[0].sequence },
			{ events: olderPage, has_more: false, next_cursor: 0 }
		];
		let call = 0;
		const client = {
			state: {
				history: async () => {
					const page = pages[call++];
					return {
						events: page.events,
						head_sequence: olderPage[0].sequence,
						tail_sequence: newerPage[newerPage.length - 1].sequence,
						has_more: page.has_more,
						next_cursor: page.next_cursor,
						truncated: false,
						protocol_version: '0.1.0'
					};
				}
			}
		} as unknown as ProtocolClient;

		const loaded = await loadSessionHistory(client, 'sess-1');
		expect(call).toBe(2);
		expect(loaded.events).toHaveLength(4);
		const turns = reduceHistoryTurns(loaded.events);
		expect(turns).toHaveLength(1);
		const t = turnOf(turns, 'r1');
		expect(t.app?.resourceUri).toBe('ui://reports/second.html');
		expect(t.toolCalls).toEqual([{ tool: 'reports_render', status: 'succeeded', summary: '0.3s' }]);
	});

	it('leave-and-return: an App-bearing turn reopens with the App AND its stats', () => {
		// The acceptance centerpiece — the reopened turn carries everything the
		// live view showed: the App ref, its server, the tool badge, the model
		// chip, tokens/cost. Previously the App alone was lost and the turn read
		// as a broken/empty answer.
		const t = turnOf(
			reduceHistoryTurns([
				ev('task.started', 'r1', {}, '2026-07-10T12:00:00Z'),
				ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'reports_render' }),
				ev('tool.invoked', 'r1', { ToolName: 'reports_render' }),
				ev('mcp.app_available', 'r1', appPayload()),
				ev('tool.completed', 'r1', { ToolName: 'reports_render', DurationMS: 1200 }),
				ev('llm.completion.chunk', 'r1', { Delta: 'Rendered.', Kind: 'content' }),
				ev('llm.cost.recorded', 'r1', {
					Model: 'anthropic/claude-haiku-4.5',
					Usage: { TotalTokens: 300, PromptTokens: 220, CompletionTokens: 80 },
					Cost: { TotalCost: 0.002 }
				}),
				ev('task.completed', 'r1', {}, '2026-07-10T12:00:04Z')
			]),
			'r1'
		);
		expect(t.app).toBeDefined();
		expect(t.serverID).toBe('reports');
		expect(t.tokens).toBe(300);
		expect(t.model).toBe('anthropic/claude-haiku-4.5');
		expect(t.toolCalls).toHaveLength(1);
		expect(t.durationMs).toBe(4000);
	});

	it('folds ONLY the declared fields: an unknown payload key never reaches the turn', () => {
		// The fold reads named keys; it must never copy the payload wholesale into
		// the turn. The sentinel is PLANTED IN THE PAYLOAD (on a key the reducer
		// does not read, and inside a nested object) so the assertion can actually
		// fail — a fold that spread the payload, or a future one that carried an
		// argument/result field, would surface it in the hydrated transcript.
		const sentinel = 'SUPER-SECRET';
		const turns = reduceHistoryTurns([
			ev(
				'mcp.app_available',
				'r1',
				appPayload({
					Arguments: { apiKey: sentinel },
					Result: sentinel,
					Unknown: sentinel
				})
			)
		]);
		const serialized = JSON.stringify(turns);
		expect(serialized).not.toContain(sentinel);
		// ...and the fold still produced the App from the keys it DOES read, so
		// the assertion above is not passing merely because nothing was folded.
		expect(turnOf(turns, 'r1').app?.resourceUri).toBe('ui://reports/dashboard.html');
	});
});
