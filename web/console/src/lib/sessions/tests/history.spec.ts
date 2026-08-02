// Vitest for the session-reopen reducer (D-293): reduceHistoryTurns folds the
// content-free per-turn metadata from the SAME durable bus events the live
// stream reduces, so a reopen renders header stats + TOOL CALLS badges + the
// model chip identical to the live view. These fixtures mirror the runtime's
// PascalCase wire payloads (the durable log persists the Go structs without
// json tags); a snake_case-tolerance case is included.

import { describe, it, expect } from 'vitest';
import { reduceHistoryTurns, type HistoryTurn } from '../history.js';
import type { StateEvent } from '../../protocol/state.js';
// The live projection the reopen must byte-match. Importing it here binds the
// two producers so the byte-equivalence pin fails if either drifts.
import {
	parseReasoningSteps,
	type ReasoningStep,
	type TaskDetailLike
} from '../../../routes/(console)/playground/[session_id]/answer-envelope.js';

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

	it('leave-and-return structure carries the ordered reasoning↔tool interleaving (D-298)', () => {
		// The operator's UX requirement: a reopened multi-step turn reconstructs
		// the SAME ordered reasoning-step accordion + tool badges the live view
		// showed — not one flat undifferentiated reasoning blob. hydratePastTurns
		// maps turn.reasoningSteps onto the message; MessageBubble prefers it.
		const events: StateEvent[] = [
			ev('task.started', 'r1', {}, '2026-07-10T12:00:00Z'),
			ev('planner.decision', 'r1', {
				DecisionKind: 'CallTool',
				Tool: 'weather.lookup',
				ReasoningTrace: 'First, the weather.'
			}, '2026-07-10T12:00:00Z'),
			ev('tool.invoked', 'r1', { ToolName: 'weather.lookup' }, '2026-07-10T12:00:00Z'),
			ev('tool.completed', 'r1', { ToolName: 'weather.lookup', DurationMS: 900 }, '2026-07-10T12:00:01Z'),
			ev('planner.decision', 'r1', {
				DecisionKind: 'CallTool',
				Tool: 'search.web',
				ReasoningTrace: 'Then, search the web.'
			}, '2026-07-10T12:00:01Z'),
			ev('tool.invoked', 'r1', { ToolName: 'search.web' }, '2026-07-10T12:00:01Z'),
			ev('tool.completed', 'r1', { ToolName: 'search.web', DurationMS: 500 }, '2026-07-10T12:00:02Z'),
			ev('llm.completion.chunk', 'r1', { Delta: 'Sunny, 1M results.', Kind: 'content' }, '2026-07-10T12:00:02Z'),
			ev('task.completed', 'r1', {}, '2026-07-10T12:00:03Z')
		];
		const [t] = reduceHistoryTurns(events);
		expect(t.answer).toBe('Sunny, 1M results.');
		expect(t.reasoningSteps).toEqual([
			{ index: 0, reasoning_trace: 'First, the weather.' },
			{ index: 1, reasoning_trace: 'Then, search the web.' }
		]);
		expect(t.toolCalls).toEqual([
			{ tool: 'weather.lookup', status: 'succeeded', summary: '0.9s' },
			{ tool: 'search.web', status: 'succeeded', summary: '0.5s' }
		]);
	});

	it('a run with no non-empty reasoning steps yields an empty reasoningSteps (falls back to flat text)', () => {
		// The mock/non-thinking case: the reopen shows fewer/zero reasoning
		// steps exactly when the live view did. hydratePastTurns then sets
		// reasoningSteps undefined and MessageBubble falls back to reasoningText.
		const turns = reduceHistoryTurns([
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'a', ReasoningTrace: '' }),
			ev('tool.completed', 'r1', { ToolName: 'a', DurationMS: 10 })
		]);
		expect(turnOf(turns, 'r1').reasoningSteps).toEqual([]);
	});
});

// ------------------------------------------------------------------------
// Structured reasoning-step rehydration (D-298).
//
// The reducer reconstructs the ordered reasoningSteps[] from the durable
// `planner.decision` stream. The `index` semantics MUST match the live
// enricher's `i`-over-`traj.Steps`: the runloop appends a `traj.Step` ONLY
// for step-appending decisions (CallTool / CallParallel / SpawnTask /
// AwaitTask), while `Finish` / `RequestPause` append NO step. So the fold
// advances the per-run ordinal ONLY on step-appending kinds, emits only
// when the trace is non-empty, and NEVER folds a Finish/RequestPause.
// ------------------------------------------------------------------------

/**
 * A modelled trajectory step — mirrors the runtime `trajectory.Step`'s
 * reasoning slot. Derives BOTH the durable event window and the live
 * enricher projection from the SAME source of truth, so the byte-equivalence
 * pin cannot silently agree with a mis-built fixture.
 */
interface TrajStep {
	/** The step-appending decision kind that produced this step. */
	kind: 'CallTool' | 'CallParallel' | 'SpawnTask' | 'AwaitTask';
	/** The provider-side reasoning captured on the step (may be empty). */
	reasoning: string;
}

/**
 * The live enricher projection for a modelled trajectory: `{index: i,
 * reasoning_trace}` for each step with non-empty reasoning, `i` being the
 * 0-based position in the FULL step sequence (enricher.go:61-67).
 */
function enricherProjection(steps: TrajStep[]): ReasoningStep[] {
	const out: ReasoningStep[] = [];
	steps.forEach((s, i) => {
		if (s.reasoning.length > 0) out.push({ index: i, reasoning_trace: s.reasoning });
	});
	return out;
}

describe('reduceHistoryTurns — reasoning-step reconstruction (D-298)', () => {
	it('folds ordered steps ONLY for step-appending decisions, in sequence order', () => {
		const turns = reduceHistoryTurns([
			ev('planner.decision', 'r1', {
				DecisionKind: 'CallTool',
				Tool: 'weather.lookup',
				ReasoningTrace: 'I should check the weather first.'
			}),
			ev('tool.invoked', 'r1', { ToolName: 'weather.lookup' }),
			ev('tool.completed', 'r1', { ToolName: 'weather.lookup', DurationMS: 800 }),
			ev('planner.decision', 'r1', {
				DecisionKind: 'CallTool',
				Tool: 'search.web',
				ReasoningTrace: 'Now search the web for context.'
			}),
			ev('tool.invoked', 'r1', { ToolName: 'search.web' }),
			ev('tool.completed', 'r1', { ToolName: 'search.web', DurationMS: 400 })
		]);
		const t = turnOf(turns, 'r1');
		expect(t.reasoningSteps).toEqual([
			{ index: 0, reasoning_trace: 'I should check the weather first.' },
			{ index: 1, reasoning_trace: 'Now search the web for context.' }
		]);
	});

	it('advances the ordinal without emitting on an empty-reasoning step-appending decision', () => {
		const turns = reduceHistoryTurns([
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'a', ReasoningTrace: 'think A' }),
			// Step 1 is step-appending with EMPTY reasoning — the enricher's `i`
			// still advances, so the next emitted step is index 2.
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'b', ReasoningTrace: '' }),
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'c', ReasoningTrace: 'think C' })
		]);
		expect(turnOf(turns, 'r1').reasoningSteps).toEqual([
			{ index: 0, reasoning_trace: 'think A' },
			{ index: 2, reasoning_trace: 'think C' }
		]);
	});

	it('tolerates snake_case reasoning_trace / decision_kind keys', () => {
		const turns = reduceHistoryTurns([
			ev('planner.decision', 'r1', { decision_kind: 'CallTool', tool: 'a', reasoning_trace: 'snake think' })
		]);
		expect(turnOf(turns, 'r1').reasoningSteps).toEqual([{ index: 0, reasoning_trace: 'snake think' }]);
	});
});

describe('reduceHistoryTurns — byte-equivalence vs parseReasoningSteps (D-298, §17.8)', () => {
	// The mandated fixture: a reasoning-bearing Finish AND a mid-run
	// RequestPause (both carrying non-empty reasoning), plus an empty-reasoning
	// CallTool step. A reducer using the OLD every-decision rule phantom-adds
	// the Finish/RequestPause steps and shifts the post-pause index — this
	// fixture is the guard that catches it.
	const trajectory: TrajStep[] = [
		{ kind: 'CallTool', reasoning: 'Check the weather.' },
		{ kind: 'CallTool', reasoning: '' }, // empty → advances i, no emit
		{ kind: 'CallParallel', reasoning: 'Fan out two lookups.' }
	];

	it('reconstructs reasoningSteps byte-identical to the enricher projection', () => {
		const events: StateEvent[] = [
			ev('task.started', 'r1', {}),
			// Step 0: CallTool with reasoning.
			ev('planner.decision', 'r1', {
				DecisionKind: 'CallTool',
				Tool: 'weather.lookup',
				ReasoningTrace: 'Check the weather.'
			}),
			ev('tool.invoked', 'r1', { ToolName: 'weather.lookup' }),
			ev('tool.completed', 'r1', { ToolName: 'weather.lookup', DurationMS: 800 }),
			// Step 1: CallTool with EMPTY reasoning (advances i, no emit).
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'search.web', ReasoningTrace: '' }),
			ev('tool.invoked', 'r1', { ToolName: 'search.web' }),
			ev('tool.completed', 'r1', { ToolName: 'search.web', DurationMS: 400 }),
			// A mid-run RequestPause carrying reasoning — NO traj.Step, must NOT
			// emit and must NOT shift the following index.
			ev('planner.decision', 'r1', {
				DecisionKind: 'RequestPause',
				ReasoningTrace: 'Pausing for operator approval.'
			}),
			// Step 2: CallParallel with reasoning — index MUST be 2 (unshifted).
			ev('planner.decision', 'r1', {
				DecisionKind: 'CallParallel',
				ReasoningTrace: 'Fan out two lookups.'
			}),
			// A reasoning-bearing Finish — NO traj.Step, must NOT emit.
			ev('planner.decision', 'r1', {
				DecisionKind: 'Finish',
				ReasoningTrace: 'The answer is ready; summarising now.'
			}),
			ev('task.completed', 'r1', {})
		];

		const detail: TaskDetailLike = { trajectory: { steps: enricherProjection(trajectory) } };
		const live = parseReasoningSteps(detail);
		const reopened = turnOf(reduceHistoryTurns(events), 'r1').reasoningSteps;

		// The two producers agree — same indices, same traces, same order.
		expect(reopened).toEqual(live);
		expect(reopened).toEqual([
			{ index: 0, reasoning_trace: 'Check the weather.' },
			{ index: 2, reasoning_trace: 'Fan out two lookups.' }
		]);
		// Neither the Finish nor the RequestPause reasoning leaks a step.
		const traces = reopened.map((s) => s.reasoning_trace);
		expect(traces).not.toContain('Pausing for operator approval.');
		expect(traces).not.toContain('The answer is ready; summarising now.');
		// The post-RequestPause step keeps its un-shifted index.
		expect(reopened[1].index).toBe(2);
	});

	it('preserves newline-bearing reasoning bytes between live projection and durable reopen', () => {
		const reasoning = '**Preparing to send email**\n\nI need to compose';
		const expectedBytes = [
			42, 42, 80, 114, 101, 112, 97, 114, 105, 110, 103, 32, 116, 111, 32, 115, 101,
			110, 100, 32, 101, 109, 97, 105, 108, 42, 42, 10, 10, 73, 32, 110, 101, 101, 100,
			32, 116, 111, 32, 99, 111, 109, 112, 111, 115, 101
		];
		const live = parseReasoningSteps({
			trajectory: { steps: [{ index: 0, reasoning_trace: reasoning }] }
		});
		const reopened = turnOf(
			reduceHistoryTurns([
				ev('planner.decision', 'r1', {
					DecisionKind: 'CallTool',
					Tool: 'email.send',
					ReasoningTrace: reasoning
				})
			]),
			'r1'
		).reasoningSteps;

		// This is a data-path assertion, not a presentation workaround: both
		// producers must retain the two literal LF bytes before rendering.
		expect(reopened).toEqual(live);
		expect(live[0].reasoning_trace).toBe(reasoning);
		expect(reopened[0].reasoning_trace).toBe(reasoning);
		expect(Array.from(new TextEncoder().encode(live[0].reasoning_trace))).toEqual(expectedBytes);
		expect(Array.from(new TextEncoder().encode(reopened[0].reasoning_trace))).toEqual(expectedBytes);
	});
});

describe('reduceHistoryTurns — reasoning-step page-window-boundary safety (D-298)', () => {
	it('reconstructs order-stably across a two-page merge with no dup/reorder', () => {
		// loadSessionHistory merges pages oldest-first into one flat stream; the
		// reducer folds that stream. Model a run whose steps straddle a window
		// seam: the reducer must not duplicate or reorder them.
		const olderPage: StateEvent[] = [
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'a', ReasoningTrace: 'step zero' }),
			ev('tool.completed', 'r1', { ToolName: 'a', DurationMS: 100 })
		];
		const newerPage: StateEvent[] = [
			ev('planner.decision', 'r1', { DecisionKind: 'CallTool', Tool: 'b', ReasoningTrace: 'step one' }),
			ev('tool.completed', 'r1', { ToolName: 'b', DurationMS: 100 })
		];
		const merged = [...olderPage, ...newerPage]; // oldest-first, as loadSessionHistory returns
		const steps = turnOf(reduceHistoryTurns(merged), 'r1').reasoningSteps;
		expect(steps).toEqual([
			{ index: 0, reasoning_trace: 'step zero' },
			{ index: 1, reasoning_trace: 'step one' }
		]);
	});
});

describe('reduceHistoryTurns — reasoning-step per-run ordinal isolation (D-298)', () => {
	// The plan named "a window with 2 tool-calling runs" as THE failure fixture:
	// the per-run stepOrdinal Map must be keyed by runID, so each run's
	// reasoningSteps restart at index 0. A shared (non-keyed) ordinal would
	// carry run A's count into run B — this pins that it does not.
	it('two tool-calling runs in one window each restart reasoningSteps at index 0', () => {
		const turns = reduceHistoryTurns([
			// Run A — two step-appending decisions.
			ev('planner.decision', 'runA', { DecisionKind: 'CallTool', Tool: 'a1', ReasoningTrace: 'A step 0' }),
			ev('tool.invoked', 'runA', { ToolName: 'a1' }),
			ev('tool.completed', 'runA', { ToolName: 'a1', DurationMS: 100 }),
			ev('planner.decision', 'runA', { DecisionKind: 'CallTool', Tool: 'a2', ReasoningTrace: 'A step 1' }),
			ev('tool.invoked', 'runA', { ToolName: 'a2' }),
			ev('tool.completed', 'runA', { ToolName: 'a2', DurationMS: 100 }),
			ev('task.completed', 'runA', {}),
			// Run B — a later run in the SAME window; its ordinal MUST restart at 0.
			ev('planner.decision', 'runB', { DecisionKind: 'CallTool', Tool: 'b1', ReasoningTrace: 'B step 0' }),
			ev('tool.invoked', 'runB', { ToolName: 'b1' }),
			ev('tool.completed', 'runB', { ToolName: 'b1', DurationMS: 100 }),
			ev('task.completed', 'runB', {})
		]);
		expect(turnOf(turns, 'runA').reasoningSteps).toEqual([
			{ index: 0, reasoning_trace: 'A step 0' },
			{ index: 1, reasoning_trace: 'A step 1' }
		]);
		// The pin: run B restarts at index 0 — the ordinal did NOT leak from run A.
		expect(turnOf(turns, 'runB').reasoningSteps).toEqual([{ index: 0, reasoning_trace: 'B step 0' }]);
	});

	it('two runs whose events INTERLEAVE by sequence keep independent per-run ordinals', () => {
		// The harder shape: the two runs' events are woven together in the
		// window (as concurrent runs land on the one bus). The ordinal is keyed
		// by runID, so interleaving must not cross-contaminate either index line.
		const turns = reduceHistoryTurns([
			ev('planner.decision', 'runA', { DecisionKind: 'CallTool', Tool: 'a1', ReasoningTrace: 'A step 0' }),
			ev('planner.decision', 'runB', { DecisionKind: 'CallTool', Tool: 'b1', ReasoningTrace: 'B step 0' }),
			ev('planner.decision', 'runA', { DecisionKind: 'CallParallel', ReasoningTrace: 'A step 1' }),
			ev('planner.decision', 'runB', { DecisionKind: 'CallTool', Tool: 'b2', ReasoningTrace: 'B step 1' }),
			// A reasoning-bearing Finish on each run — neither appends a step.
			ev('planner.decision', 'runA', { DecisionKind: 'Finish', ReasoningTrace: 'A done' }),
			ev('planner.decision', 'runB', { DecisionKind: 'Finish', ReasoningTrace: 'B done' })
		]);
		expect(turnOf(turns, 'runA').reasoningSteps).toEqual([
			{ index: 0, reasoning_trace: 'A step 0' },
			{ index: 1, reasoning_trace: 'A step 1' }
		]);
		expect(turnOf(turns, 'runB').reasoningSteps).toEqual([
			{ index: 0, reasoning_trace: 'B step 0' },
			{ index: 1, reasoning_trace: 'B step 1' }
		]);
	});
});
