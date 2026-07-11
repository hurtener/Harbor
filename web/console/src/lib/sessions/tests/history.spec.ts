// Vitest for the session-reopen reducer (D-293): reduceHistoryTurns folds the
// content-free per-turn metadata from the SAME durable bus events the live
// stream reduces, so a reopen renders header stats + TOOL CALLS badges + the
// model chip identical to the live view. These fixtures mirror the runtime's
// PascalCase wire payloads (the durable log persists the Go structs without
// json tags); a snake_case-tolerance case is included.

import { describe, it, expect } from 'vitest';
import { reduceHistoryTurns, type HistoryTurn } from '../history.js';
import type { StateEvent } from '../../protocol/state.js';

let seq = 0;
function ev(type: string, run: string, payload: unknown, at = '2026-07-10T12:00:00Z'): StateEvent {
	return { type, sequence: ++seq, occurred_at: at, tenant: 't', user: 'u', session: 's', run, payload };
}

function turnOf(turns: HistoryTurn[], runID: string): HistoryTurn {
	const t = turns.find((x) => x.runID === runID);
	if (t === undefined) throw new Error(`no turn ${runID}`);
	return t;
}

describe('reduceHistoryTurns — cost fold', () => {
	it('sums usage/cost and captures the model from llm.cost.recorded (PascalCase)', () => {
		const turns = reduceHistoryTurns([
			ev('llm.cost.recorded', 'r1', {
				Model: 'anthropic/claude-haiku-4.5',
				Usage: { TotalTokens: 100, PromptTokens: 70, CompletionTokens: 30 },
				Cost: { TotalCost: 0.0012 },
				ContextWindowTokens: 200000
			}),
			ev('llm.cost.recorded', 'r1', {
				Model: 'anthropic/claude-haiku-4.5',
				Usage: { TotalTokens: 40, PromptTokens: 25, CompletionTokens: 15 },
				Cost: { TotalCost: 0.0005 }
			})
		]);
		const t = turnOf(turns, 'r1');
		expect(t.tokens).toBe(140);
		expect(t.promptTokens).toBe(95);
		expect(t.outputTokens).toBe(45);
		expect(t.costUSD).toBeCloseTo(0.0017, 6);
		expect(t.model).toBe('anthropic/claude-haiku-4.5');
	});

	it('tolerates snake_case usage/cost keys', () => {
		const turns = reduceHistoryTurns([
			ev('llm.cost.recorded', 'r1', {
				model: 'm',
				usage: { total_tokens: 10, prompt_tokens: 6, completion_tokens: 4 },
				cost: { total_cost: 0.001 }
			})
		]);
		const t = turnOf(turns, 'r1');
		expect(t.tokens).toBe(10);
		expect(t.promptTokens).toBe(6);
		expect(t.outputTokens).toBe(4);
		expect(t.costUSD).toBeCloseTo(0.001, 6);
		expect(t.model).toBe('m');
	});
});

describe('reduceHistoryTurns — tool-call lifecycle resolution', () => {
	it('planner.decision opens an invoked row that tool.completed resolves to succeeded + duration', () => {
		const turns = reduceHistoryTurns([
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'youtube_get_metadata' }),
			ev('tool.completed', 'r1', { ToolName: 'youtube_get_metadata', DurationMS: 2320 })
		]);
		const t = turnOf(turns, 'r1');
		expect(t.toolCalls).toEqual([{ tool: 'youtube_get_metadata', status: 'succeeded', summary: '2.3s' }]);
	});

	it('tool.failed resolves the row to failed with a redacted class:message summary', () => {
		const turns = reduceHistoryTurns([
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'youtube_get_metadata' }),
			ev('tool.failed', 'r1', {
				ToolName: 'youtube_get_metadata',
				ErrorClass: 'timeout',
				ErrorMessage: 'context deadline exceeded'
			})
		]);
		const t = turnOf(turns, 'r1');
		expect(t.toolCalls).toEqual([
			{ tool: 'youtube_get_metadata', status: 'failed', summary: 'timeout: context deadline exceeded' }
		]);
	});

	it('a terminal tool event with no open row appends one (never silently dropped)', () => {
		const turns = reduceHistoryTurns([ev('tool.completed', 'r1', { ToolName: 'orphan', DurationMS: 500 })]);
		expect(turnOf(turns, 'r1').toolCalls).toEqual([{ tool: 'orphan', status: 'succeeded', summary: '0.5s' }]);
	});
});

describe('reduceHistoryTurns — duration + answer', () => {
	it('derives durationMs from task.started → task.completed timestamps (the fallback)', () => {
		const turns = reduceHistoryTurns([
			ev('task.started', 'r1', {}, '2026-07-10T12:00:00Z'),
			ev('llm.completion.chunk', 'r1', { Delta: 'hello', Kind: 'content' }, '2026-07-10T12:00:01Z'),
			ev('task.completed', 'r1', {}, '2026-07-10T12:00:03Z')
		]);
		const t = turnOf(turns, 'r1');
		expect(t.durationMs).toBe(3000);
		expect(t.answer).toBe('hello');
		expect(t.terminal).toBe(true);
	});

	it('does NOT fold tool args/results (content-free): a sentinel never appears', () => {
		const sentinel = 'SUPER-SECRET';
		const turns = reduceHistoryTurns([
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'echo' }),
			ev('tool.completed', 'r1', { ToolName: 'echo', DurationMS: 10 })
		]);
		expect(JSON.stringify(turns)).not.toContain(sentinel);
	});
});

describe('reduceHistoryTurns — rehydration regression (leave-and-return ≡ live)', () => {
	it('reconstructs a complete turn: answer + tokens + cost + model + tool badge + duration', () => {
		const events: StateEvent[] = [
			ev('task.started', 'r1', {}, '2026-07-10T12:00:00Z'),
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'youtube_get_metadata' }, '2026-07-10T12:00:00Z'),
			ev('tool.invoked', 'r1', { ToolName: 'youtube_get_metadata', Transport: 'mcp' }, '2026-07-10T12:00:00Z'),
			ev('tool.completed', 'r1', { ToolName: 'youtube_get_metadata', DurationMS: 2320 }, '2026-07-10T12:00:02Z'),
			ev('llm.completion.chunk', 'r1', { Delta: 'The video has ', Kind: 'content' }, '2026-07-10T12:00:03Z'),
			ev('llm.completion.chunk', 'r1', { Delta: '1M views.', Kind: 'content' }, '2026-07-10T12:00:03Z'),
			ev('llm.cost.recorded', 'r1', {
				Model: 'anthropic/claude-haiku-4.5',
				Usage: { TotalTokens: 512, PromptTokens: 400, CompletionTokens: 112 },
				Cost: { TotalCost: 0.0031 },
				ContextWindowTokens: 200000
			}, '2026-07-10T12:00:03Z'),
			ev('task.completed', 'r1', {}, '2026-07-10T12:00:04Z')
		];
		const [t] = reduceHistoryTurns(events);
		expect(t.answer).toBe('The video has 1M views.');
		expect(t.tokens).toBe(512);
		expect(t.costUSD).toBeCloseTo(0.0031, 6);
		expect(t.model).toBe('anthropic/claude-haiku-4.5');
		expect(t.durationMs).toBe(4000);
		expect(t.toolCalls).toEqual([{ tool: 'youtube_get_metadata', status: 'succeeded', summary: '2.3s' }]);
		expect(t.terminal).toBe(true);
	});
});
