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
import type { SessionTurnRow } from '$lib/protocol/session-turns.js';

import {
	appViewFromDiscovery,
	answerFromRow,
	appViewFromRow,
	derivedReasoningSummary,
	hydratedAgentMessage,
	turnRowMessages
} from './turn-projection.js';
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

describe('turnRowMessages — the durable projection (HA-64 / D-425)', () => {
	/** A minimal well-formed durable turn row. */
	function row(over: Partial<SessionTurnRow> = {}): SessionTurnRow {
		return {
			turn_id: 't1',
			task_id: 't1',
			session_id: 's1',
			sequence: 1,
			tie_breaker: 't1',
			status: 'complete',
			sealed: true,
			version: 1,
			last_applied_event_seq: 5,
			started_at: '2026-07-10T12:00:00Z',
			updated_at: '2026-07-10T12:00:05Z',
			finished_at: '2026-07-10T12:00:05Z',
			agent: { id: 'ag-1', name: 'analyst', binding_source: 'explicit', complete: 'complete' },
			query: { text: 'Show the dashboard', at: '2026-07-10T12:00:00Z', complete: 'complete' },
			answer: { state: 'inline', inline: 'Here is the dashboard.', seq: 5, complete: 'complete' },
			pause: { availability: 'unavailable' },
			usage: {
				prompt_tokens: { state: 'exact', value: 100 },
				completion_tokens: { state: 'exact', value: 50 },
				reasoning_tokens: { state: 'unavailable' },
				cache_read_tokens: { state: 'unavailable' },
				cache_write_tokens: { state: 'unavailable' },
				total_tokens: { state: 'exact', value: 150 },
				cost_micro_usd: { state: 'exact', value: 1234 },
				latency_ns: { state: 'estimated', value: 2_500_000_000 },
				model: 'claude-haiku'
			},
			reasoning: {
				steps: [
					{ index: 0, kind: 'tool_call' },
					{ index: 1, kind: 'spawn' }
				],
				complete: 'complete',
				seq: 5
			},
			activity: {
				rows: [
					{ position: 0, tool: 'reports_render', step_sequence: 1, status: 'succeeded', retryable: false, policy_exhausted: false, summary: '1.2s' }
				],
				complete: 'complete',
				more: false,
				totals: { invoked: 0, succeeded: 1, failed: 0, cancelled: 0, retried: 0, policy_exhausted: 0 }
			},
			apps: [
				{
					effective_agent_id: 'agent-reports',
					server_id: 'reports',
					resource_uri: 'ui://reports/dashboard.html',
					display_mode: 'inline',
					raw_html_trusted: false,
					tool_call_id: 'tc_9f2c1a',
					tool_name: 'reports_render',
					availability: 'available',
					complete: 'complete'
				}
			],
			...over
		};
	}

	it('projects the query + answer onto user/agent bubbles with taskID + timestamps', () => {
		const rendered = turnRowMessages(row());
		expect(rendered.user?.role).toBe('user');
		expect(rendered.user?.text).toBe('Show the dashboard');
		expect(rendered.user?.taskID).toBe('t1');
		expect(rendered.agent?.role).toBe('agent');
		expect(rendered.agent?.text).toBe('Here is the dashboard.');
		expect(rendered.agent?.taskID).toBe('t1');
		expect(rendered.agent?.pending).toBe(false);
	});

	it('renders a running/paused turn as a pending bubble (the mutable snapshot)', () => {
		const running = turnRowMessages(row({ status: 'running', sealed: false }));
		expect(running.agent?.pending).toBe(true);
		expect(running.status).toBe('running');
		const paused = turnRowMessages(
			row({
				status: 'paused',
				sealed: false,
				pause: { class: 'hitl_approval', reason: 'approve the spend', lifecycle: 'active', availability: 'complete' }
			})
		);
		expect(paused.agent?.pending).toBe(true);
		expect(paused.paused).toBe(true);
		expect(paused.pauseReason).toBe('approve the spend');
	});

	it('renders an artifact_ref answer by reference — never inlines heavy bytes (D-026)', () => {
		const rendered = turnRowMessages(
			row({
				answer: {
					state: 'artifact_ref',
					ref: { id: 'art-1', mime_type: 'text/markdown', size_bytes: 4096, filename: 'answer.md', sha256: 'abc' },
					seq: 5,
					complete: 'complete'
				}
			})
		);
		expect(rendered.agent?.text).toBe('');
		expect(rendered.agent?.artifacts).toEqual([
			{ id: 'art-1', mime: 'text/markdown', filename: 'answer.md', sizeBytes: 4096 }
		]);
	});

	it('captions an evicted / unavailable answer honestly — never "(no answer recorded)"', () => {
		expect(turnRowMessages(row({ answer: { state: 'evicted', seq: 5, complete: 'unavailable' } })).agent?.text).toBe(
			'(answer evicted — no longer retained)'
		);
		expect(turnRowMessages(row({ answer: { state: 'unavailable', seq: 0 } })).agent?.text).toBe(
			'(answer unavailable)'
		);
		// A definite EMPTY answer renders as an empty bubble, not the failure caption.
		expect(turnRowMessages(row({ answer: { state: 'empty', seq: 5, complete: 'complete' } })).agent?.text).toBe('');
	});

	it('renders per-measure usage availability — never a fabricated zero', () => {
		const exact = turnRowMessages(row()).usage;
		expect(exact.tokens).toBe(150);
		expect(exact.costUSD).toBeCloseTo(0.001234, 6);
		expect(exact.promptTokens).toBe(100);
		expect(exact.outputTokens).toBe(50);
		expect(exact.latencyMs).toBe(2500);
		expect(exact.model).toBe('claude-haiku');

		// An unavailable measure is OMITTED — the bubble meta carries no token/
		// cost fields rather than a believable zero.
		const unavailable = turnRowMessages(
			row({
				usage: {
					prompt_tokens: { state: 'unavailable' },
					completion_tokens: { state: 'unavailable' },
					reasoning_tokens: { state: 'unavailable' },
					cache_read_tokens: { state: 'unavailable' },
					cache_write_tokens: { state: 'unavailable' },
					total_tokens: { state: 'unavailable' },
					cost_micro_usd: { state: 'unavailable' },
					latency_ns: { state: 'unavailable' }
				}
			})
		);
		expect(unavailable.usage.tokens).toBeUndefined();
		expect(unavailable.usage.costUSD).toBeUndefined();
		expect(unavailable.agent?.meta?.tokens).toBeUndefined();
		expect(unavailable.agent?.meta?.costUSD).toBeUndefined();
	});

	it('renders the DERIVED reasoning summary — kinds + honest partial overflow, never raw thinking', () => {
		const rendered = turnRowMessages(row());
		expect(rendered.agent?.reasoningText).toBe(
			'Derived reasoning · 2 steps (tool_call, spawn)'
		);
		expect(rendered.reasoningPartial).toBe(false);
		// The wire structurally carries NO raw provider thinking.
		expect(JSON.stringify(rendered.agent?.reasoningText ?? '')).not.toContain('thinking');

		const partial = turnRowMessages(
			row({
				reasoning: {
					steps: [{ index: 0, kind: 'tool_call' }],
					complete: 'partial',
					dropped: 9,
					seq: 5
				}
			})
		);
		expect(partial.reasoningPartial).toBe(true);
		expect(partial.reasoningDropped).toBe(9);
		expect(partial.agent?.reasoningText).toContain('9 older steps not retained');
	});

	it('maps the bounded activity window to content-free tool rows and reports overflow honestly', () => {
		const rendered = turnRowMessages(row());
		expect(rendered.agent?.toolCalls).toEqual([
			{ tool: 'reports_render', status: 'succeeded', summary: '1.2s' }
		]);
		expect(rendered.activityOverflow).toEqual({ more: false, dropped: 0 });

		const overflow = turnRowMessages(
			row({
				activity: {
					rows: [{ position: 3, tool: 'z_last', step_sequence: 9, status: 'invoked', retryable: false, policy_exhausted: false, summary: '' }],
					complete: 'partial',
					more: true,
					dropped: 3,
					totals: { invoked: 1, succeeded: 1, failed: 0, cancelled: 0, retried: 0, policy_exhausted: 0 }
				}
			})
		);
		expect(overflow.activityOverflow).toEqual({ more: true, dropped: 3 });
		expect(overflow.agent?.toolCalls).toHaveLength(1);
	});

	it('carries agent binding + attachment availability honestly', () => {
		const rendered = turnRowMessages(
			row({
				inputs: [
					{ id: 'in-1', filename: 'data.csv', mime_type: 'text/csv', availability: 'complete' },
					{ id: 'in-2', filename: 'gone.bin', mime_type: 'application/octet-stream', availability: 'unavailable' }
				],
				outputs: [{ id: 'out-1', filename: 'out.json', mime_type: 'application/json', availability: 'complete' }]
			})
		);
		expect(rendered.agentBinding).toEqual({
			id: 'ag-1',
			name: 'analyst',
			bindingSource: 'explicit',
			complete: 'complete'
		});
		// Complete attachments render by reference; the unavailable one is
		// counted, never silently dropped and never rendered as complete.
		expect(rendered.user?.artifacts).toEqual([
			{ id: 'in-1', mime: 'text/csv', filename: 'data.csv', sizeBytes: undefined }
		]);
		expect(rendered.agent?.artifacts).toEqual([
			{ id: 'out-1', mime: 'application/json', filename: 'out.json', sizeBytes: undefined }
		]);
		expect(rendered.attachmentsUnavailable).toBe(1);
	});

	it('maps durable App refs to the view WITHOUT any render admission (binding)', () => {
		const rendered = turnRowMessages(row());
		expect(rendered.apps).toHaveLength(1);
		expect(rendered.apps[0].view).toEqual({
			agentId: 'agent-reports',
			resourceUri: 'ui://reports/dashboard.html',
			displayMode: 'inline',
			rawHtmlTrusted: false,
			toolCallId: 'tc_9f2c1a',
			toolName: 'reports_render'
		});
		// The HA-56 render admission is NEVER carried on the durable path.
		expect(rendered.apps[0].view.binding).toBeUndefined();
		expect((rendered.agent?.app as { binding?: string } | undefined)?.binding).toBeUndefined();
		expect(rendered.agent?.serverID).toBe('reports');
		expect(rendered.apps[0].availability).toBe('available');
	});

	it('keeps an unavailable App ref honest — availability surfaces, nothing phantom mounts', () => {
		const rendered = turnRowMessages(
			row({
				apps: [
					{
						server_id: 'reports',
						resource_uri: 'ui://reports/dashboard.html',
						display_mode: '',
						raw_html_trusted: false,
						availability: 'unavailable',
						complete: 'unavailable'
					}
				]
			})
		);
		expect(rendered.apps[0].availability).toBe('unavailable');
		// The renderer lane shows the honest "no longer available" path; the
		// projection itself carries only metadata + availability.
		expect(rendered.agent?.serverID).toBe('reports');
		expect((rendered.agent?.app as { binding?: string } | undefined)?.binding).toBeUndefined();
	});

	it('appViewFromRow never invents a binding for any durable ref', () => {
		const view = appViewFromRow({
			server_id: 's',
			resource_uri: 'ui://s/x.html',
			raw_html_trusted: false,
			display_mode: 'fullscreen',
			availability: 'available'
		});
		expect(view.binding).toBeUndefined();
		expect(view.resourceUri).toBe('ui://s/x.html');
		expect(view.displayMode).toBe('fullscreen');
	});

	it('derivedReasoningSummary is undefined for an unavailable/empty reasoning component', () => {
		expect(derivedReasoningSummary({ steps: [], complete: 'unavailable', seq: 0 })).toBeUndefined();
		expect(derivedReasoningSummary({ steps: [], complete: 'complete', seq: 0 })).toBeUndefined();
	});

	it('answerFromRow renders the closed union without fabricating an empty', () => {
		expect(answerFromRow({ state: 'inline', inline: 'yes', seq: 1, complete: 'complete' })).toEqual({ text: 'yes' });
		expect(answerFromRow({ state: 'artifact_ref', ref: { id: 'r', sha256: '' }, seq: 1, complete: 'complete' }).ref?.id).toBe('r');
		expect(answerFromRow({ state: 'empty', seq: 1, complete: 'complete' })).toEqual({ text: '' });
		expect(answerFromRow({ state: 'evicted', seq: 1, complete: 'unavailable' }).text).toContain('evicted');
		expect(answerFromRow({ state: 'unavailable', seq: 0 })).toEqual({ text: '(answer unavailable)' });
	});
});
