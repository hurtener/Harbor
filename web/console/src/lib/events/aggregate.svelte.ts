// Events page — the `events.aggregate` sparkline-data controller
// (Phase 73g / D-125 — Svelte 5 runes mode, D-092).
//
// `EventsAggregator` is the page-local reactive wrapper over the
// shipped `events.aggregate` Protocol method (Phase 72a). It drives the
// event-rate sparkline. It calls `client.events.aggregate(...)` through
// the unified `HarborClient` — no hand-rolled `fetch`.
//
// # Independent cursor (page-events.md §4)
//
// The sparkline's data source is `events.aggregate`; the table's is
// `events.subscribe`. The two are DELIBERATELY independent: the
// sparkline is a derived rate-over-time view, the table is the live
// stream. Pause-stream freezes the TABLE only — the sparkline keeps
// refreshing (page-events.md §12 risk note).

import type {
	EventFilter,
	EventsNamespace,
	TimeWindow
} from '$lib/protocol/harbor.js';
import { WINDOW_SPEC } from '$lib/protocol/harbor.js';
import { projectBuckets, type SparklineSeries } from './sparkline.js';
import type { ProtocolError } from '$lib/protocol/errors.js';

/** The four-state status the sparkline's nested `<PageState>` reads. */
export type AggregateStatus = 'loading' | 'error' | 'ready';

/**
 * EventsAggregator owns the sparkline's `events.aggregate` data. It
 * exposes the projected {@link SparklineSeries} as a rune; the page
 * re-fetches on a window or filter change. A fetch failure surfaces in
 * `error` — the sparkline renders a nested error state, never silently
 * empty (CLAUDE.md §13).
 */
export class EventsAggregator {
	/** The projected stacked-area series the sparkline renders. */
	series = $state<SparklineSeries>({ columns: [], eventTypes: [], peak: 0 });
	/** The fetch status. */
	status = $state<AggregateStatus>('loading');
	/** The thrown error — populated only in the `error` status. */
	error = $state<{ code: string; message: string } | null>(null);
	/** The active window — selects the `(span, bucket)` pair. */
	window = $state<TimeWindow>('1h');

	readonly #ns: EventsNamespace;
	#filter: EventFilter = {};

	constructor(ns: EventsNamespace) {
		this.#ns = ns;
	}

	/** Sets the active window and re-fetches. */
	setWindow(w: TimeWindow): void {
		this.window = w;
		void this.refresh();
	}

	/** Sets the event predicate and re-fetches. */
	setFilter(f: EventFilter): void {
		this.#filter = f;
		void this.refresh();
	}

	/**
	 * Fetches `events.aggregate` for the current window + filter and
	 * projects the response into the rendered sparkline series.
	 */
	async refresh(): Promise<void> {
		this.status = 'loading';
		this.error = null;
		const spec = WINDOW_SPEC[this.window];
		try {
			const resp = await this.#ns.aggregate({
				filter: this.#filter,
				window: spec.windowNs,
				bucket: spec.bucketNs
			});
			this.series = projectBuckets(resp.buckets);
			this.status = 'ready';
		} catch (e) {
			this.series = { columns: [], eventTypes: [], peak: 0 };
			this.error = describeAggregateError(e);
			this.status = 'error';
		}
	}
}

/** Renders an aggregate-fetch error into a page-friendly `{code, message}`. */
export function describeAggregateError(e: unknown): { code: string; message: string } {
	const pe = e as Partial<ProtocolError>;
	if (typeof pe?.code === 'string' && typeof pe?.message === 'string') {
		return { code: pe.code, message: pe.message };
	}
	if (e instanceof Error) {
		return { code: 'runtime_error', message: e.message };
	}
	return { code: 'runtime_error', message: 'Unknown error' };
}
