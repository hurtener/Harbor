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

import type { EventFilter, TimeWindow } from '$lib/protocol/events.js';
import { EPOCH_ANCHOR, WINDOW_SPEC } from '$lib/protocol/events.js';
import type { EventsNamespace } from '$lib/protocol/client.js';
import { projectBuckets, type SparklineSeries } from './sparkline.js';
import { projectTenantAttribution, type TenantAttribution } from './attribution.js';
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
	/**
	 * The per-tenant attribution breakdown, or null when the read was not
	 * admin-widened / attribution was not requested (nothing to attribute).
	 * Populated only on a cross-tenant (fleet) read that opted in — it lets
	 * the operator independently verify the tenant boundary the runtime
	 * enforced (the defence-in-depth an aggregate otherwise lacks).
	 */
	attribution = $state<TenantAttribution | null>(null);
	/** The fetch status. */
	status = $state<AggregateStatus>('loading');
	/** The thrown error — populated only in the `error` status. */
	error = $state<{ code: string; message: string } | null>(null);
	/** The active window — selects the `(span, bucket)` pair. */
	window = $state<TimeWindow>('1h');

	readonly #ns: EventsNamespace;
	#filter: EventFilter = {};
	/**
	 * Whether to request per-tenant attribution. Set by the page ONLY when
	 * the active read is cross-tenant (widened); the runtime honours it only
	 * under a verified admin/console:fleet scope and ignores it otherwise, so
	 * this is a display opt-in, never an authority escalation.
	 */
	#byTenant = false;

	constructor(ns: EventsNamespace) {
		this.#ns = ns;
	}

	/** Sets the active window and re-fetches. */
	setWindow(w: TimeWindow): void {
		this.window = w;
		void this.refresh();
	}

	/**
	 * Sets the event predicate and re-fetches. `byTenant` opts in to
	 * per-tenant attribution — the page passes `true` on a cross-tenant
	 * (fleet) read so the operator can verify the tenant breakdown; the
	 * runtime ignores it on a non-widened read (fail-closed).
	 */
	setFilter(f: EventFilter, byTenant = false): void {
		this.#filter = f;
		this.#byTenant = byTenant;
		void this.refresh();
	}

	/** The entitled tenants the operator asked for — the aggregate filter's
	 * `tenant_ids`, used to flag any returned tenant outside the set. */
	get #entitled(): readonly string[] {
		return this.#filter.tenant_ids ?? [];
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
				bucket: spec.bucketNs,
				// Floor the grid onto the globally-shared epoch grid so every
				// bucket_start is an addressable, cacheable coordinate the page
				// can stitch cold windows against and re-request by coordinate
				// on the next poll instead of re-deriving the whole series
				// (D-306). Absent this, two polls a few seconds apart return two
				// different boundary sets and nothing caches.
				anchor: EPOCH_ANCHOR,
				// Opt in to per-tenant attribution on a cross-tenant read. The
				// runtime returns counts_by_tenant ONLY under a verified
				// admin/console:fleet scope; on any other read the flag is
				// ignored (fail-closed) and the projection below is null.
				by_tenant: this.#byTenant
			});
			this.series = projectBuckets(resp.buckets);
			this.attribution = projectTenantAttribution(resp.buckets, this.#entitled);
			this.status = 'ready';
		} catch (e) {
			this.series = { columns: [], eventTypes: [], peak: 0 };
			this.attribution = null;
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
