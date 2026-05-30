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
	decodeIntervention,
	decodeInterventionClear
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
