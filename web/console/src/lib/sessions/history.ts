// Harbor Console — session-reopen windowed hydration helper (D-254).
//
// This is the FIRST consumer of the `state.history` Protocol surface
// (CLAUDE.md §13 primitive-with-consumer rule): a tail-first, windowed,
// scroll-up-by-cursor reader of a session's durable event stream, plus a
// pure client-side reduction of the loaded events into reopen turns. The
// Playground session-reopen hydration drives this instead of its former
// full-load `tasks.list` + N×`tasks.get` reconstruction.
//
// The reduction stays client-side (the surface returns flat events, never
// pre-reduced turns): the agent answer + reasoning + tool calls for each
// run are reconstructed from the `llm.completion.chunk` / `planner.*` /
// `tool.*` events the runtime already publishes onto the durable log —
// the same events the live SSE stream reduces. The user query text is NOT
// carried in the durable event payloads (the task lifecycle payloads omit
// it), so the caller folds it in from a single catalog lookup.

import type { ProtocolClient } from '../protocol/client.js';
import type { StateEvent, StateHistoryResponse } from '../protocol/state.js';
import { DEFAULT_STATE_HISTORY_LIMIT } from '../protocol/state.js';

/** Options for {@link loadSessionHistory}. */
export interface LoadHistoryOptions {
	/** Per-page window size (clamped server-side to the max). */
	pageLimit?: number;
	/**
	 * The maximum number of windows to scroll up through. A reopen loads a
	 * bounded recent slice, not the entire (possibly enormous) history; the
	 * operator scrolls further on demand. Defaults to 8 pages.
	 */
	maxPages?: number;
}

/** The merged result of a windowed history load. */
export interface LoadedHistory {
	/** Every loaded event, oldest-first across all scrolled windows. */
	events: StateEvent[];
	/** The session's retained head/tail sequence (from the first page). */
	headSequence: number;
	tailSequence: number;
	/** True when older events remain below the loaded window (page budget hit). */
	hasMore: boolean;
	/** True when the durable log trimmed events older than the retained head. */
	truncated: boolean;
}

/**
 * Loads a session's recent durable event history TAIL-FIRST through the
 * typed `state.history` surface, scrolling up by `next_cursor` until the
 * retained head is reached or the page budget is spent. Returns the merged
 * events oldest-first.
 *
 * No hand-rolled `fetch` — every request goes through the injected typed
 * `ProtocolClient` (CLAUDE.md §4.5 rule 5, §13).
 */
export async function loadSessionHistory(
	client: ProtocolClient,
	sessionID: string,
	opts: LoadHistoryOptions = {}
): Promise<LoadedHistory> {
	const pageLimit = opts.pageLimit ?? DEFAULT_STATE_HISTORY_LIMIT;
	const maxPages = opts.maxPages ?? 8;

	const pages: StateEvent[][] = [];
	let before = 0; // 0 ⇒ from the tail.
	let head = 0;
	let tail = 0;
	let hasMore = false;
	let truncated = false;

	for (let page = 0; page < maxPages; page++) {
		const resp: StateHistoryResponse = await client.state.history({
			session_id: sessionID,
			before,
			limit: pageLimit
		});
		if (page === 0) {
			head = resp.head_sequence;
			tail = resp.tail_sequence;
		}
		truncated = truncated || resp.truncated === true;
		// Each page is oldest-first WITHIN the window; collect pages
		// newest-window-first, then flatten oldest-first below.
		pages.push(resp.events);
		hasMore = resp.has_more;
		if (!resp.has_more || resp.next_cursor === 0) {
			hasMore = false;
			break;
		}
		before = resp.next_cursor;
	}

	// pages[0] is the newest window, pages[n] the oldest scrolled window;
	// each is internally oldest-first. Reverse the page order and
	// concatenate so the merged stream is oldest-first end-to-end.
	const events: StateEvent[] = [];
	for (let i = pages.length - 1; i >= 0; i--) {
		events.push(...pages[i]);
	}

	return { events, headSequence: head, tailSequence: tail, hasMore, truncated };
}

/** One reopened conversation turn reduced from the event window. */
export interface HistoryTurn {
	/** The run (task) id the turn belongs to. */
	runID: string;
	/** The reconstructed agent answer text (concatenated content deltas). */
	answer: string;
	/** The reconstructed reasoning text (concatenated reasoning deltas), if any. */
	reasoning: string;
	/** The earliest event instant in the run (RFC-3339 UTC). */
	at: string;
	/** Whether the run reached a terminal lifecycle event. */
	terminal: boolean;
}

/** Reads the first present string field across PascalCase / snake_case keys. */
function readString(obj: Record<string, unknown>, keys: string[]): string {
	for (const k of keys) {
		const v = obj[k];
		if (typeof v === 'string') return v;
	}
	return '';
}

/** The run id an event belongs to — the wire `run` field, or a payload TaskID. */
function eventRunID(ev: StateEvent): string {
	if (ev.run) return ev.run;
	if (ev.payload !== null && typeof ev.payload === 'object') {
		return readString(ev.payload as Record<string, unknown>, ['TaskID', 'task_id']);
	}
	return '';
}

/**
 * `reduceHistoryTurns` folds a window of durable events into per-run turns,
 * reconstructing each run's answer + reasoning from its
 * `llm.completion.chunk` deltas (the same channel the live stream reduces).
 * Returns the turns oldest-first by first-seen event. The durable event
 * payloads are the redacted Go structs (PascalCase keys); the reader
 * tolerates both casings.
 */
export function reduceHistoryTurns(events: readonly StateEvent[]): HistoryTurn[] {
	const order: string[] = [];
	const byRun = new Map<string, HistoryTurn>();

	const turnFor = (runID: string, at: string): HistoryTurn => {
		let t = byRun.get(runID);
		if (t === undefined) {
			t = { runID, answer: '', reasoning: '', at, terminal: false };
			byRun.set(runID, t);
			order.push(runID);
		}
		return t;
	};

	for (const ev of events) {
		const runID = eventRunID(ev);
		if (runID === '') continue;
		const turn = turnFor(runID, ev.occurred_at);

		if (ev.type === 'llm.completion.chunk' && ev.payload !== null && typeof ev.payload === 'object') {
			const p = ev.payload as Record<string, unknown>;
			const delta = readString(p, ['Delta', 'delta']);
			const kind = readString(p, ['Kind', 'kind']);
			if (kind === 'reasoning') {
				turn.reasoning += delta;
			} else {
				turn.answer += delta;
			}
		}
		if (ev.type === 'task.completed' || ev.type === 'task.failed' || ev.type === 'task.cancelled') {
			turn.terminal = true;
		}
	}

	return order.map((id) => byRun.get(id)!).filter((t): t is HistoryTurn => t !== undefined);
}
