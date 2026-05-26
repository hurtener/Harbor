/**
 * Phase 106 (V1.2) — answer-envelope parsing unit tests.
 *
 * Pins the Playground's answer-extraction contract. The shipped bug
 * (`detail.task.result_inline` instead of `detail.result_inline`) would
 * have surfaced here: a test that hands a wire-shape TaskDetail to the
 * parser fails when the parser walks the wrong path.
 */
import { describe, expect, it } from 'vitest';
import { parseAnswerFromDetail, normalizeLifecycleType } from './answer-envelope.js';

describe('parseAnswerFromDetail — answer envelope', () => {
	it('returns the answer when result_inline parses cleanly', () => {
		const envelope = JSON.stringify({
			answer: 'Hello, world.',
			finish_reason: 'goal',
			tool_calls_seen: 0
		});
		expect(parseAnswerFromDetail({ result_inline: envelope })).toBe('Hello, world.');
	});

	it('returns the empty string when result_inline is absent', () => {
		expect(parseAnswerFromDetail({})).toBe('');
		expect(parseAnswerFromDetail(null)).toBe('');
		expect(parseAnswerFromDetail(undefined)).toBe('');
	});

	it('returns the empty string when result_inline is an empty string', () => {
		expect(parseAnswerFromDetail({ result_inline: '' })).toBe('');
	});

	it('returns the explicit fallback when result_inline is not valid JSON', () => {
		expect(parseAnswerFromDetail({ result_inline: 'not-json' })).toBe(
			'(failed to parse answer payload)'
		);
	});

	it('returns empty when envelope is valid JSON but has no answer field', () => {
		expect(parseAnswerFromDetail({ result_inline: '{}' })).toBe('');
	});

	it('returns empty when the answer field is non-string (defends against future-shape drift)', () => {
		expect(parseAnswerFromDetail({ result_inline: '{"answer": 42}' })).toBe('');
		expect(parseAnswerFromDetail({ result_inline: '{"answer": null}' })).toBe('');
		expect(parseAnswerFromDetail({ result_inline: '{"answer": {"k": "v"}}' })).toBe('');
	});

	it('pins the wire shape — result_inline is at the TOP LEVEL of TaskDetail, NOT inside task', () => {
		// A previous implementation read `detail.task.result_inline`.
		// That path does not exist on the wire shape (only TaskDetail
		// carries result_inline; TaskRow does not). This test fails if
		// a future regression nests it again.
		const wrongShape = {
			task: { result_inline: '{"answer":"this should not be read"}' }
		} as { task: { result_inline: string } };
		// parseAnswerFromDetail ignores the nested field — it only reads
		// the top-level one.
		expect(parseAnswerFromDetail(wrongShape as unknown as { result_inline?: string })).toBe('');
	});
});

describe('normalizeLifecycleType — SSE delivery channel normalization', () => {
	it("strips the 'task.' prefix from named-event types", () => {
		expect(normalizeLifecycleType('task.completed')).toBe('completed');
		expect(normalizeLifecycleType('task.failed')).toBe('failed');
		expect(normalizeLifecycleType('task.cancelled')).toBe('cancelled');
	});

	it('passes through suffixes that are already normalised', () => {
		expect(normalizeLifecycleType('completed')).toBe('completed');
		expect(normalizeLifecycleType('failed')).toBe('failed');
	});

	it('only strips a leading "task." (not "task." in the middle)', () => {
		expect(normalizeLifecycleType('foo.task.completed')).toBe('foo.task.completed');
	});
});
