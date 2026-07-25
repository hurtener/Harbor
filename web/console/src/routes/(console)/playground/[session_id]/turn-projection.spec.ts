// Specs for the two turn projections (D-348).
//
// These cover the INTEGRATION POINT the replay actually needs: the field
// mapping that carries a reduced `HistoryTurn`'s App onto the reopened bubble,
// and its render gate. While that mapping was inline in `+page.svelte` it was
// unreachable by any spec — deleting the App fields left the whole feature
// inert with every test still green, and reverting the gate re-introduced the
// exact "an App-only turn vanishes" bug the phase closes. Both mutations now
// fail here.

import { describe, expect, it } from 'vitest';

import type { ChatToolCall } from '$lib/chat/types.js';
import type { HistoryTurn } from '$lib/sessions/history.js';

import { appViewFromDiscovery, hydratedAgentMessage } from './turn-projection.js';
import { decodeAppAvailable } from './wire-events.js';

/** A reduced turn with every field at its empty default. */
function turn(over: Partial<HistoryTurn> = {}): HistoryTurn {
	return {
		runID: 'r1',
		answer: '',
		reasoning: '',
		at: '2026-07-10T12:00:00Z',
		terminal: false,
		tokens: 0,
		promptTokens: 0,
		outputTokens: 0,
		costUSD: 0,
		model: '',
		durationMs: 0,
		toolCalls: [],
		reasoningSteps: [],
		...over
	};
}

const NO_CTX = { at: '2026-07-10T12:00:00Z', durationMs: 0, toolCalls: [] as ChatToolCall[] };

const APP = {
	resourceUri: 'ui://reports/dashboard.html',
	displayMode: 'inline' as const,
	rawHtmlTrusted: false,
	toolCallId: 'tc_9f2c1a',
	toolName: 'build_dashboard'
};

describe('hydratedAgentMessage — the reopened bubble field mapping (D-348)', () => {
	it('carries the App ref AND its server onto the message', () => {
		// THE integration point: without both fields MessageBubble mounts no
		// renderer and the whole replay is inert.
		const m = hydratedAgentMessage(turn({ answer: 'Rendered.', app: APP, serverID: 'reports' }), NO_CTX);
		expect(m).not.toBeNull();
		expect(m?.app).toEqual(APP);
		expect(m?.serverID).toBe('reports');
	});

	it('renders an App-only turn (no answer, not terminal) — the vanishing bug', () => {
		// The App IS the turn's output; the model-facing text is deliberately
		// terse. A gate of `answer || terminal` drops this turn entirely, which is
		// exactly what made a reopened App disappear.
		const m = hydratedAgentMessage(turn({ app: APP, serverID: 'reports' }), NO_CTX);
		expect(m).not.toBeNull();
		expect(m?.app).toEqual(APP);
	});

	it('suppresses the "(no answer recorded)" caption when the turn carries an App', () => {
		// The live view showed no text there either — captioning a perfectly good
		// App with "no answer recorded" reads as a failure that did not happen.
		const m = hydratedAgentMessage(turn({ app: APP, serverID: 'reports' }), NO_CTX);
		expect(m?.text).toBe('');
	});

	it('still captions an answerless TERMINAL turn that declared no App', () => {
		const m = hydratedAgentMessage(turn({ terminal: true }), NO_CTX);
		expect(m?.text).toBe('(no answer recorded)');
	});

	it('renders nothing for a turn with no answer, no terminal event, and no App', () => {
		expect(hydratedAgentMessage(turn(), NO_CTX)).toBeNull();
	});

	it('leaves the sibling fields intact (stats, reasoning, tool rows)', () => {
		const rows: ChatToolCall[] = [
			{ tool: 'reports_render', status: 'succeeded', summary: '1.2s', runID: 'r1' }
		];
		const m = hydratedAgentMessage(
			turn({
				answer: 'Rendered.',
				reasoning: 'thinking',
				reasoningSteps: [{ index: 0, reasoning_trace: 'thinking' }],
				tokens: 300,
				costUSD: 0.002,
				app: APP,
				serverID: 'reports'
			}),
			{ at: '2026-07-10T12:00:01Z', durationMs: 4000, toolCalls: rows }
		);
		expect(m?.text).toBe('Rendered.');
		expect(m?.at).toBe('2026-07-10T12:00:01Z');
		expect(m?.reasoningText).toBe('thinking');
		expect(m?.reasoningSteps).toEqual([{ index: 0, reasoning_trace: 'thinking' }]);
		expect(m?.toolCalls).toEqual(rows);
		expect(m?.meta).toEqual({ elapsedMs: 4000, tokens: 300, costUSD: 0.002 });
		expect(m?.taskID).toBe('r1');
		expect(m?.role).toBe('agent');
	});

	it('omits the App fields entirely for a turn that declared none', () => {
		const m = hydratedAgentMessage(turn({ answer: 'Plain.' }), NO_CTX);
		expect(m?.app).toBeUndefined();
		expect(m?.serverID).toBeUndefined();
	});
});

describe('appViewFromDiscovery — the live bubble projection (D-348)', () => {
	function decode(payload: Record<string, unknown>) {
		const ev = decodeAppAvailable(JSON.stringify({ type: 'mcp.app_available', run: 'r1', payload }));
		if (ev === null) throw new Error('decoder rejected the frame');
		return appViewFromDiscovery(ev);
	}

	it('projects the decoded discovery onto the bubble ref + server', () => {
		const got = decode({
			ServerID: 'reports',
			ToolCallID: 'tc_9f2c1a',
			ToolName: 'build_dashboard',
			ResourceURI: 'ui://reports/dashboard.html',
			DisplayMode: 'pip',
			RawHTMLTrusted: true
		});
		expect(got.serverID).toBe('reports');
		expect(got.app).toEqual({
			resourceUri: 'ui://reports/dashboard.html',
			displayMode: 'pip',
			rawHtmlTrusted: true,
			toolCallId: 'tc_9f2c1a',
			toolName: 'build_dashboard'
		});
	});

	it('normalises an unknown display-mode hint to "" (the renderer default)', () => {
		expect(
			decode({
				ServerID: 'reports',
				ResourceURI: 'ui://reports/x.html',
				DisplayMode: 'hologram'
			}).app.displayMode
		).toBe('');
	});
});
