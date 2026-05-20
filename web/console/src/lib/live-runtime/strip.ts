/**
 * Live Runtime header status-counter strip — the pure live-delta logic
 * (Phase 73b / D-126).
 *
 * The header strip is fed by the `tasks.list` status-counter-strip
 * aggregate on initial load, then maintained LIVE from the `task.*` SSE
 * event stream (the aggregate call is the initial-load shape only — see
 * the phase-73b plan's "tasks.list aggregate cost" risk).
 *
 * `applyTaskEvent` is the pure reducer: given the current strip + one
 * event type, it returns the next strip. It lives here as pure logic so
 * a Vitest unit test exercises every transition without a DOM.
 */

/** The five-chip strip shape — mirrors `TaskListStatusCounterStrip`. */
export interface StatusCounterStrip {
	pending: number;
	running: number;
	completed: number;
	paused: number;
	failed: number;
}

/** The empty strip — the loading / no-data baseline. */
export const EMPTY_STRIP: StatusCounterStrip = {
	pending: 0,
	running: 0,
	completed: 0,
	paused: 0,
	failed: 0
};

/**
 * The pure live-delta reducer. Given the current strip + one event
 * type, returns the next strip — a `task.started` / `task.spawned`
 * increments running; `task.completed` shifts running→completed;
 * `task.failed` shifts running→failed; `task.paused` shifts
 * running→paused. Every other event type returns the strip unchanged.
 *
 * Counts never go negative — a shift off an empty `running` clamps at 0
 * (a live delta is best-effort; the authoritative recount is the next
 * `tasks.list` aggregate call).
 */
export function applyTaskEvent(strip: StatusCounterStrip, eventType: string): StatusCounterStrip {
	switch (eventType) {
		case 'task.started':
		case 'task.spawned':
			return { ...strip, running: strip.running + 1 };
		case 'task.completed':
			return {
				...strip,
				running: Math.max(0, strip.running - 1),
				completed: strip.completed + 1
			};
		case 'task.failed':
			return {
				...strip,
				running: Math.max(0, strip.running - 1),
				failed: strip.failed + 1
			};
		case 'task.paused':
			return {
				...strip,
				running: Math.max(0, strip.running - 1),
				paused: strip.paused + 1
			};
		default:
			return strip;
	}
}
