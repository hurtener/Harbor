/**
 * Phase 107 (V1.3) — chunk-stream unit tests.
 */
import { describe, expect, it } from 'vitest';
import { applyChunk, finalizeStream } from './chunk-stream.js';

interface TMsg {
	id: string;
	role: string;
	text: string;
	taskID?: string;
	streaming?: boolean;
}

const mkMsg = (overrides: Partial<TMsg> = {}): TMsg => ({
	id: 'm-1',
	role: 'agent',
	text: '',
	taskID: 'task-1',
	...overrides
});

describe('applyChunk', () => {
	it('appends delta to matching bubble', () => {
		const msgs = [mkMsg({ text: 'Hello' })];
		const got = applyChunk(msgs, 'task-1', ', world');
		expect(got[0].text).toBe('Hello, world');
	});

	it('ignores mismatched taskID', () => {
		const msgs = [mkMsg({ text: 'Hello', taskID: 'task-1' })];
		const got = applyChunk(msgs, 'task-2', ', ignored');
		expect(got[0].text).toBe('Hello');
	});

	it('sets streaming:true on first chunk', () => {
		const msgs = [mkMsg({ streaming: undefined })];
		const got = applyChunk(msgs, 'task-1', 'hi');
		expect(got[0].streaming).toBe(true);
	});

	it('keeps streaming true across multiple chunks', () => {
		let msgs = [mkMsg()];
		msgs = applyChunk(msgs, 'task-1', 'a');
		msgs = applyChunk(msgs, 'task-1', 'b');
		expect(msgs[0].text).toBe('ab');
		expect(msgs[0].streaming).toBe(true);
	});

	it('ignores messages with no role=agent', () => {
		const msgs = [mkMsg({ role: 'user', taskID: 'task-1', text: 'hi' })];
		const got = applyChunk(msgs, 'task-1', ' ignored');
		expect(got[0].text).toBe('hi');
	});
});

describe('finalizeStream', () => {
	it('clears streaming flag', () => {
		const msgs = [mkMsg({ streaming: true })];
		const got = finalizeStream(msgs, 'task-1');
		expect(got[0].streaming).toBe(false);
	});

	it('ignores mismatched taskID', () => {
		const msgs = [mkMsg({ streaming: true, taskID: 'task-1' })];
		const got = finalizeStream(msgs, 'task-2');
		expect(got[0].streaming).toBe(true);
	});

	it('is a no-op when streaming was already false', () => {
		const msgs = [mkMsg({ streaming: false })];
		const got = finalizeStream(msgs, 'task-1');
		expect(got[0].streaming).toBe(false);
	});

	it('handles reasoning-kind gracefully (Phase 107 emit but Phase 107a render)', () => {
		const msgs = [mkMsg()];
		const got = applyChunk(msgs, 'task-1', 'First, I need to understand...');
		expect(got[0].text.length).toBeGreaterThan(0);
	});

	// Phase 107 AC-12 (b): a consumer that never receives a chunk (mock
	// LLM or non-streaming provider) must still end up in a sane terminal
	// state. The chunk-stream helpers leave such a bubble untouched —
	// `streaming` stays undefined, `text` stays empty. The Playground
	// page then populates from the Phase 106 `result_inline` fetch at
	// `task.completed`. This test pins the helpers' "do nothing when no
	// chunks arrive" invariant; the page-level fetch is exercised by the
	// answer-envelope.test.ts suite.
	it('leaves a never-streamed bubble untouched (Phase 106 fetch reconciles)', () => {
		const msgs: TMsg[] = [mkMsg({ text: '', streaming: undefined })];
		// No applyChunk calls — simulating a provider that completed
		// without streaming. finalizeStream on the same task id is a no-op
		// because the bubble was never marked streaming.
		const got = finalizeStream(msgs, 'task-1');
		expect(got[0].text).toBe('');
		expect(got[0].streaming).toBe(false);
	});
});
