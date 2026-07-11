// Playground SSE wire-event decoder grammar (Phase 108 follow-up).
//
// The fixtures below are REAL frames captured from a live `harbor dev`
// runtime (`GET /v1/events`) — the PascalCase-nested payload shape the
// decoders must read. The first streaming cut read top-level snake_case
// fields and dropped every chunk; these specs pin the wire shape so that
// regression cannot recur.

import { describe, it, expect } from 'vitest';
import {
	decodeChunk,
	decodeCost,
	decodeLifecycle,
	decodeBudget,
	decodePlannerDecision,
	decodeToolLifecycle,
	decodeIntervention,
	decodeInterventionClear,
	decodeAppAvailable
} from './wire-events.js';

const chunkFrame = JSON.stringify({
	type: 'llm.completion.chunk',
	sequence: 161,
	run: '01KSTH74S20BDDP1BK6ZSGABJG',
	payload: {
		SafePayload: null,
		Identity: { TenantID: 'dev', UserID: 'dev', SessionID: 'dev', RunID: '01KSTH74S20BDDP1BK6ZSGABJG' },
		TaskID: '01KSTH74S20BDDP1BK6ZSGABJG',
		RunID: '01KSTH74S20BDDP1BK6ZSGABJG',
		Delta: ' **Downloa',
		Done: false,
		Kind: 'content'
	}
});

const costFrame = JSON.stringify({
	type: 'llm.cost.recorded',
	run: '01KSTH74S20BDDP1BK6ZSGABJG',
	payload: {
		Model: 'anthropic/claude-haiku-4.5',
		Cost: { InputTokensCost: 0, OutputTokensCost: 0, TotalCost: 0.004359, Currency: 'USD' },
		Usage: { PromptTokens: 4139, CompletionTokens: 44, ReasoningTokens: 0, TotalTokens: 4183, LatencyMS: 2320 },
		ContextWindowTokens: 200000
	}
});

const completedFrame = JSON.stringify({
	type: 'task.completed',
	payload: { TaskID: '01KSTH74S20BDDP1BK6ZSGABJG' }
});

const budgetFrame = JSON.stringify({
	type: 'governance.budget_exceeded',
	payload: { Model: 'x', TotalCost: 0.09, Ceiling: 0.1, Currency: 'USD' }
});

describe('decodeChunk', () => {
	it('reads the PascalCase nested payload (the streaming-bug regression)', () => {
		const c = decodeChunk(chunkFrame);
		expect(c).not.toBeNull();
		expect(c!.taskID).toBe('01KSTH74S20BDDP1BK6ZSGABJG');
		expect(c!.delta).toBe(' **Downloa');
		expect(c!.done).toBe(false);
		expect(c!.kind).toBe('content');
	});

	it('classifies reasoning chunks distinctly from content', () => {
		const r = decodeChunk(JSON.stringify({ payload: { TaskID: 't1', Delta: 'hmm', Kind: 'reasoning' } }));
		expect(r!.kind).toBe('reasoning');
	});

	it('falls back to the frame run id when payload TaskID is absent', () => {
		const c = decodeChunk(JSON.stringify({ run: 'r9', payload: { Delta: 'x' } }));
		expect(c!.taskID).toBe('r9');
	});

	it('returns null on malformed JSON', () => {
		expect(decodeChunk('not json')).toBeNull();
	});
});

describe('decodeCost', () => {
	it('reads Usage + Cost from the nested payload', () => {
		const c = decodeCost(costFrame);
		expect(c).not.toBeNull();
		expect(c!.model).toBe('anthropic/claude-haiku-4.5');
		expect(c!.totalTokens).toBe(4183);
		expect(c!.promptTokens).toBe(4139);
		expect(c!.outputTokens).toBe(44);
		expect(c!.usd).toBeCloseTo(0.004359, 6);
		expect(c!.contextWindow).toBe(200000);
	});
});

describe('decodeLifecycle', () => {
	it('decodes a terminal completed event', () => {
		const l = decodeLifecycle(completedFrame);
		expect(l).toEqual({ taskID: '01KSTH74S20BDDP1BK6ZSGABJG', kind: 'completed' });
	});

	it('ignores non-terminal types', () => {
		expect(decodeLifecycle(JSON.stringify({ type: 'task.started', payload: { TaskID: 't' } }))).toBeNull();
	});
});

describe('decodePlannerDecision', () => {
	const callToolFrame = JSON.stringify({
		type: 'planner.decision',
		run: '01KSTW-TASK',
		payload: { DecisionKind: 'CallTool', Tool: 'youtube_get_metadata', ReasoningChars: 0 }
	});

	it('decodes a CallTool decision (tool name + kind) using the run fallback', () => {
		const d = decodePlannerDecision(callToolFrame);
		expect(d).toEqual({ taskID: '01KSTW-TASK', decisionKind: 'CallTool', tool: 'youtube_get_metadata' });
	});

	it('ignores non-planner.decision frames', () => {
		expect(decodePlannerDecision(completedFrame)).toBeNull();
	});
});

describe('decodeToolLifecycle', () => {
	it('decodes tool.invoked (run fallback for the task id, empty summary)', () => {
		const f = JSON.stringify({
			type: 'tool.invoked',
			run: 'RUN-T',
			payload: { ToolName: 'youtube_get_metadata', Transport: 'mcp', StartedAt: '2026-05-30T03:55:22Z' }
		});
		expect(decodeToolLifecycle(f)).toEqual({
			taskID: 'RUN-T',
			tool: 'youtube_get_metadata',
			kind: 'invoked',
			summary: ''
		});
	});

	it('decodes tool.completed with a duration summary', () => {
		const f = JSON.stringify({
			type: 'tool.completed',
			run: 'RUN-T',
			payload: { ToolName: 'youtube_get_metadata', Attempts: 1, DurationMS: 2320 }
		});
		expect(decodeToolLifecycle(f)).toEqual({
			taskID: 'RUN-T',
			tool: 'youtube_get_metadata',
			kind: 'completed',
			summary: '2.3s'
		});
	});

	it('decodes tool.failed with the class + message summary (the timeout case)', () => {
		const f = JSON.stringify({
			type: 'tool.failed',
			run: 'RUN-T',
			payload: {
				ToolName: 'youtube_get_metadata',
				Attempts: 4,
				ErrorClass: 'timeout',
				ErrorMessage: 'context deadline exceeded'
			}
		});
		expect(decodeToolLifecycle(f)).toEqual({
			taskID: 'RUN-T',
			tool: 'youtube_get_metadata',
			kind: 'failed',
			summary: 'timeout: context deadline exceeded'
		});
	});

	it('returns null without a tool name or for unrelated frames', () => {
		expect(decodeToolLifecycle(JSON.stringify({ type: 'tool.invoked', run: 'r', payload: {} }))).toBeNull();
		expect(
			decodeToolLifecycle(JSON.stringify({ type: 'planner.decision', run: 'r', payload: { Tool: 'x' } }))
		).toBeNull();
	});

	// The R2 latent-live-bug pin (D-293 b3 / §17.6): tool.* payloads carry NO
	// TaskID, so attribution rides the envelope `run`. BEFORE R2 the runtime
	// stamped an EMPTY envelope run on every tool.* frame → `taskIDOf` yielded
	// '' → the frame was silently dropped (dead status badges). AFTER R2 the
	// frame carries the full run quadruple, so it decodes and attributes.
	it('is attribution-dead without a run (the pre-R2 empty-envelope shape)', () => {
		const f = JSON.stringify({ type: 'tool.completed', payload: { ToolName: 'youtube_get_metadata', DurationMS: 1200 } });
		expect(decodeToolLifecycle(f)).toBeNull();
	});

	it('attributes on the run envelope alone once R2 populates it (no payload.TaskID)', () => {
		const f = JSON.stringify({
			type: 'tool.completed',
			run: 'RUN-Q',
			payload: { ToolName: 'youtube_get_metadata', DurationMS: 1200 }
		});
		expect(decodeToolLifecycle(f)).toEqual({
			taskID: 'RUN-Q',
			tool: 'youtube_get_metadata',
			kind: 'completed',
			summary: '1.2s'
		});
	});
});

describe('decodeIntervention', () => {
	it('decodes a tool.approval_requested into an Approve reason', () => {
		const f = JSON.stringify({
			type: 'tool.approval_requested',
			run: 'RUN-A',
			payload: { Tool: 'youtube_delete', PauseToken: 'tok1', Reason: 'destructive', Tags: ['write'] }
		});
		expect(decodeIntervention(f)).toEqual({
			runID: 'RUN-A',
			reason: 'Approve call to youtube_delete — destructive',
			source: 'tool.approval_requested'
		});
	});

	it('decodes a tool.auth_required into a Connect reason (SourceName preferred)', () => {
		const f = JSON.stringify({
			type: 'tool.auth_required',
			run: 'RUN-B',
			payload: { Source: 'gdrive', SourceName: 'Google Drive', AuthorizeURL: 'https://x', PauseToken: 't' }
		});
		expect(decodeIntervention(f)).toEqual({
			runID: 'RUN-B',
			reason: 'Connect Google Drive',
			source: 'tool.auth_required'
		});
	});

	it('decodes a pause.requested into the canonical pause reason', () => {
		const f = JSON.stringify({
			type: 'pause.requested',
			run: 'RUN-C',
			payload: { Token: 'tok', Reason: 'hitl_approval' }
		});
		expect(decodeIntervention(f)).toEqual({
			runID: 'RUN-C',
			reason: 'hitl_approval',
			source: 'pause.requested'
		});
	});

	it('returns null without a run id (the correlation key)', () => {
		expect(
			decodeIntervention(JSON.stringify({ type: 'pause.requested', payload: { Reason: 'x' } }))
		).toBeNull();
	});

	it('ignores unrelated frames', () => {
		expect(decodeIntervention(JSON.stringify({ type: 'task.completed', run: 'r', payload: {} }))).toBeNull();
	});
});

describe('decodeInterventionClear', () => {
	it('reads the run id from a pause.resumed frame', () => {
		expect(decodeInterventionClear(JSON.stringify({ type: 'pause.resumed', run: 'RUN-C', payload: {} }))).toBe(
			'RUN-C'
		);
	});

	it('reads the run id from tool.approved / tool.rejected / tool.auth_completed', () => {
		for (const type of ['tool.approved', 'tool.rejected', 'tool.auth_completed']) {
			expect(decodeInterventionClear(JSON.stringify({ type, run: 'RX', payload: {} }))).toBe('RX');
		}
	});

	it('ignores request frames and unrelated frames', () => {
		expect(decodeInterventionClear(JSON.stringify({ type: 'pause.requested', run: 'r', payload: {} }))).toBeNull();
		expect(decodeInterventionClear(JSON.stringify({ type: 'task.completed', run: 'r' }))).toBeNull();
	});
});

describe('decodeBudget', () => {
	it('reads ceiling + spend', () => {
		const b = decodeBudget(budgetFrame);
		expect(b).toEqual({ ceilingUSD: 0.1, totalCostUSD: 0.09 });
	});

	it('ignores other event types', () => {
		expect(decodeBudget(costFrame)).toBeNull();
	});
});

// The `mcp.app_available` frame is the inline-MCP-app discovery signal. Its
// payload (AppAvailablePayload) is PascalCase-nested like every event frame;
// the turn it belongs to is the envelope's `run` (the RunID).
const appAvailableFrame = JSON.stringify({
	type: 'mcp.app_available',
	sequence: 200,
	run: '01KSTH74S20BDDP1BK6ZSGABJG',
	payload: {
		SafePayload: null,
		Identity: { TenantID: 'dev', UserID: 'dev', SessionID: 'dev', RunID: '01KSTH74S20BDDP1BK6ZSGABJG' },
		ServerID: 'weather-server',
		ResourceURI: 'ui://weather/main.html',
		DisplayMode: 'inline',
		RawHTMLTrusted: false
	}
});

describe('decodeAppAvailable', () => {
	it('decodes the discovery frame, correlating to the run', () => {
		expect(decodeAppAvailable(appAvailableFrame)).toEqual({
			taskID: '01KSTH74S20BDDP1BK6ZSGABJG',
			serverID: 'weather-server',
			resourceUri: 'ui://weather/main.html',
			displayMode: 'inline',
			rawHtmlTrusted: false
		});
	});

	it('drops a frame missing the server id or resource uri (cannot mount an app)', () => {
		const noServer = JSON.stringify({
			type: 'mcp.app_available',
			run: 'r',
			payload: { ResourceURI: 'ui://x', DisplayMode: 'inline' }
		});
		const noURI = JSON.stringify({
			type: 'mcp.app_available',
			run: 'r',
			payload: { ServerID: 'srv', DisplayMode: 'inline' }
		});
		expect(decodeAppAvailable(noServer)).toBeNull();
		expect(decodeAppAvailable(noURI)).toBeNull();
	});

	it('ignores other event types', () => {
		expect(decodeAppAvailable(costFrame)).toBeNull();
		expect(decodeAppAvailable(chunkFrame)).toBeNull();
	});
});
