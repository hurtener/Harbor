// Events page — the `events.subscribe` SSE subscription wrapper
// (Phase 73g / D-125 — Svelte 5 runes mode, D-092).
//
// `EventsSubscription` is the page-local reactive wrapper over the
// shipped `events.subscribe` SSE endpoint (`GET /v1/events` — Phase 60
// transport / Phase 72 method). It is NOT a hand-rolled per-page
// Protocol client: the endpoint URL is built by the `events` namespace
// on `HarborClient` (CONVENTIONS.md §6); this class owns only the
// browser-side `EventSource` lifecycle + the rolling page of events the
// table renders.
//
// # Pause-stream is a Console-local view toggle (page-events.md §12)
//
// `streamPaused` is a render gate, NOT the runtime `pause` Protocol
// method (which is task-scoped — RFC §5.2). While paused, the
// `EventSource` stays open and the underlying SSE cursor keeps
// advancing per D-029; incoming events accumulate in a buffer. `resume`
// flushes the buffer into the visible page in `sequence` order with no
// gap. No Protocol call fires on pause / resume.
//
// # Reconnect (D-029 + Phase 60 §3)
//
// The browser `EventSource` reconnects automatically and echoes the
// last `id:` line as `Last-Event-ID`; the runtime replays from that
// cursor. `EventsSubscription` tracks the highest `sequence` it has
// seen as `cursor` for the page's subscription-status rail.

import type { Event } from '$lib/protocol/harbor.js';
import type { EventsNamespace } from '$lib/protocol/harbor.js';

/** The maximum number of events the rolling page retains in memory. */
export const MAX_ROLLING_EVENTS = 500;

/** The connection state of the underlying SSE stream. */
export type StreamState = 'idle' | 'connecting' | 'open' | 'closed' | 'error';

/**
 * An `EventSource`-shaped handle. The browser `EventSource` satisfies
 * this; tests / the Playwright harness inject a deterministic stub so
 * the wrapper is exercised without a live SSE server.
 */
export interface EventSourceLike {
	onopen: ((this: unknown, ev: unknown) => void) | null;
	onerror: ((this: unknown, ev: unknown) => void) | null;
	onmessage: ((this: unknown, ev: { data: string; lastEventId?: string }) => void) | null;
	close(): void;
}

/** Builds an {@link EventSourceLike} from a URL — `globalThis.EventSource` by default. */
export type EventSourceFactory = (url: string) => EventSourceLike;

/** Subscription-open options. */
export interface OpenOptions {
	/** The event-type names to narrow the stream to (empty = all types). */
	eventTypes?: string[];
	/** Request cross-tenant fan-in (runtime gates on admin scope, D-079). */
	admin?: boolean;
}

/**
 * EventsSubscription owns the page's live event stream. Construct one
 * with the `events` namespace from `HarborClient`; call `open` to start
 * tailing and `close` on teardown. The reactive `events` / `cursor` /
 * `state` runes drive the table + subscription-status rail.
 */
export class EventsSubscription {
	/** The rolling page of received events, newest-first. `$state`-backed. */
	events = $state<Event[]>([]);
	/** The highest `sequence` seen — the reconnect / status cursor. */
	cursor = $state<number>(0);
	/** The SSE connection state. */
	state = $state<StreamState>('idle');
	/** True while the table render is frozen (Console-local — see godoc). */
	streamPaused = $state<boolean>(false);
	/** Count of events dropped by the bus since open (`bus.dropped`). */
	droppedCount = $state<number>(0);

	readonly #ns: EventsNamespace;
	readonly #factory: EventSourceFactory;
	#source: EventSourceLike | null = null;
	/** Events received while paused — flushed on `resume` (D-029). */
	#buffer: Event[] = [];

	constructor(ns: EventsNamespace, factory?: EventSourceFactory) {
		this.#ns = ns;
		this.#factory =
			factory ??
			((url: string) => new EventSource(url) as unknown as EventSourceLike);
	}

	/** Opens (or re-opens) the SSE subscription with the given filter. */
	open(opts: OpenOptions = {}): void {
		this.close();
		this.events = [];
		this.#buffer = [];
		this.cursor = 0;
		this.droppedCount = 0;
		this.state = 'connecting';
		const url = this.#ns.subscribeURL({ eventTypes: opts.eventTypes, admin: opts.admin });
		const src = this.#factory(url);
		src.onopen = () => {
			this.state = 'open';
		};
		src.onerror = () => {
			// EventSource auto-reconnects; surface the transient error
			// state without closing — the page renders a reconnecting
			// banner, never a hard Error (CONVENTIONS.md §4 — a stream
			// blip is not a page failure).
			this.state = 'error';
		};
		src.onmessage = (ev) => {
			this.#ingest(ev.data);
		};
		this.#source = src;
	}

	/** Ingests one SSE `data:` line as an {@link Event}. */
	#ingest(data: string): void {
		let parsed: Event;
		try {
			parsed = JSON.parse(data) as Event;
		} catch {
			// A malformed frame is a real bug, not a silent drop — but
			// the SSE wire is JSON-by-construction (frame.go). Skip the
			// one frame; the cursor keeps advancing on the next.
			return;
		}
		if (parsed.sequence > this.cursor) {
			this.cursor = parsed.sequence;
		}
		if (parsed.type === 'bus.dropped') {
			this.droppedCount += 1;
		}
		if (this.streamPaused) {
			this.#buffer.push(parsed);
			return;
		}
		this.#append(parsed);
	}

	/** Prepends an event to the rolling page, trimming to the cap. */
	#append(ev: Event): void {
		const next = [ev, ...this.events];
		this.events = next.length > MAX_ROLLING_EVENTS ? next.slice(0, MAX_ROLLING_EVENTS) : next;
	}

	/**
	 * Freezes the table render. Console-local — the `EventSource` stays
	 * open and the cursor keeps advancing (D-029). No Protocol call.
	 */
	pause(): void {
		this.streamPaused = true;
	}

	/**
	 * Resumes the table render, flushing every event buffered while
	 * paused in `sequence` order (oldest-buffered first) so the table
	 * reorders deterministically with no gap (D-029).
	 */
	resume(): void {
		this.streamPaused = false;
		const flush = [...this.#buffer].sort((a, b) => a.sequence - b.sequence);
		this.#buffer = [];
		for (const ev of flush) {
			this.#append(ev);
		}
	}

	/** Closes the SSE subscription and releases the `EventSource`. */
	close(): void {
		if (this.#source !== null) {
			this.#source.onopen = null;
			this.#source.onerror = null;
			this.#source.onmessage = null;
			this.#source.close();
			this.#source = null;
		}
		this.state = 'closed';
	}
}
