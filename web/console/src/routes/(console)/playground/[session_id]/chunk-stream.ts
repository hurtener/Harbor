// Phase 107 (V1.3) — chunk-stream helpers for the Playground.
//
// Pure functions for applying `llm.completion.chunk` SSE events to
// the in-flight chat-bubble state. Extracted so the state machinery
// can be unit-tested without rendering Svelte.
//
// Contract: chunk events carry `{task_id, delta, done, kind}` on a
// flat payload. A Done=true event is the stream terminator; the bubble's
// `streaming` flag flips false on Done=true OR on `task.completed`,
// whichever fires first. Consumer that missed every chunk still sees
// the full answer at terminal time (the Phase 106 fetch reconciles).

export interface ChunkPayload {
	task_id?: string;
	delta?: string;
	done?: boolean;
	kind?: string;
}

/**
 * applyChunk appends `delta` onto the matching agent bubble's text and
 * sets `streaming: true`. Returns the updated array. Messages with no
 * `taskID` or a mismatched `taskID` are passed through unchanged.
 */
export function applyChunk<T extends { taskID?: string; role: string; text: string; streaming?: boolean }>(
	messages: T[],
	taskID: string,
	delta: string
): T[] {
	return messages.map((m) =>
		m.taskID === taskID && m.role === 'agent'
			? { ...m, text: m.text + delta, streaming: true }
			: m
	);
}

/**
 * finalizeStream clears the `streaming` flag on the matching agent
 * bubble. Safe to call even after `applyChunk` — a bubble that was
 * never streaming is a no-op.
 */
export function finalizeStream<T extends { taskID?: string; role: string; streaming?: boolean }>(
	messages: T[],
	taskID: string
): T[] {
	return messages.map((m) =>
		m.taskID === taskID && m.role === 'agent'
			? { ...m, streaming: false }
			: m
	);
}
