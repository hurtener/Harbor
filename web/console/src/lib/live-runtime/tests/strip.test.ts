/**
 * Live Runtime — `strip.ts` unit tests (Phase 73b / D-126).
 *
 * Pins the pure live-delta reducer that keeps the header status-counter
 * strip live from the `task.*` SSE event stream: every transition
 * (started / spawned / completed / failed / paused), the
 * non-task-event no-op, and the never-negative clamp.
 */
import { describe, expect, it } from 'vitest';
import { EMPTY_STRIP, applyTaskEvent } from '../strip.js';

describe('live-runtime strip: applyTaskEvent', () => {
	it('task.started and task.spawned increment running', () => {
		expect(applyTaskEvent(EMPTY_STRIP, 'task.started').running).toBe(1);
		expect(applyTaskEvent(EMPTY_STRIP, 'task.spawned').running).toBe(1);
	});

	it('task.completed shifts a running task to completed', () => {
		const after = applyTaskEvent({ ...EMPTY_STRIP, running: 2 }, 'task.completed');
		expect(after.running).toBe(1);
		expect(after.completed).toBe(1);
	});

	it('task.failed shifts a running task to failed', () => {
		const after = applyTaskEvent({ ...EMPTY_STRIP, running: 3 }, 'task.failed');
		expect(after.running).toBe(2);
		expect(after.failed).toBe(1);
	});

	it('task.paused shifts a running task to paused', () => {
		const after = applyTaskEvent({ ...EMPTY_STRIP, running: 1 }, 'task.paused');
		expect(after.running).toBe(0);
		expect(after.paused).toBe(1);
	});

	it('a non-task event returns the strip unchanged', () => {
		const base = { ...EMPTY_STRIP, running: 4, completed: 2 };
		expect(applyTaskEvent(base, 'tool.invoked')).toEqual(base);
		expect(applyTaskEvent(base, 'planner.step')).toEqual(base);
	});

	it('a completed/failed/paused shift off an empty running clamps at 0', () => {
		expect(applyTaskEvent(EMPTY_STRIP, 'task.completed').running).toBe(0);
		expect(applyTaskEvent(EMPTY_STRIP, 'task.failed').running).toBe(0);
		expect(applyTaskEvent(EMPTY_STRIP, 'task.paused').running).toBe(0);
	});

	it('the reducer is pure — it does not mutate the input strip', () => {
		const base = { ...EMPTY_STRIP, running: 1 };
		const snapshot = { ...base };
		applyTaskEvent(base, 'task.completed');
		expect(base).toEqual(snapshot);
	});
});
