// Harbor Console — Observability page reactive state controller (HA-65,
// Phase 247 minimal Console consumer). Svelte 5 runes mode (D-092).
//
// This module owns the page's reactive state; the `.svelte` view reads it
// and calls its actions, never touching the Protocol client directly
// (CONVENTIONS.md §6). It composes ONLY the shipped surface:
//   - `client.observability.query` — the ONE bounded administrative rollup
//     query. No raw event/history scan, no live cursor redesign, no second
//     administrative surface (Phase 247 non-goals).
//
// Authority posture (Phase 247 / the wire contract): the verified caller
// identity is read from the request context server-side; the request body
// NEVER supplies tenant/user/session identity for widening. This consumer
// therefore exposes NO tenant/user/session filter inputs — an ordinary
// caller's query is forced to its own verified triple, and a widened
// (admin|console:fleet) fan-in is gated + audited server-side. The only
// filter axis this consumer exposes is the closed `models` axis (the one
// non-identity filter the contract honors for every caller).
//
// Honesty contract (Phase 247 acceptance criteria):
//   - The query window is MANDATORY and UTC-grid-aligned to the bucket;
//     every request is built through `alignWindow` / `presetWindow`, never
//     from a raw operator instant.
//   - The freshness block (state / watermark / retention coverage) is
//     surfaced on every response and never hidden: current / catching_up /
//     unavailable are distinct, and a gap or unavailable projection never
//     reads as an empty result.
//   - Measure values are rendered exactly (BigInt integer arithmetic in
//     `derive.ts`); a row that does not carry a requested measure renders
//     "—", never an ambiguous zero.
//
// Fields that an async load assigns AFTER first render are `$state` so the
// reactive re-read fires (the Events-page D-180 lesson).

import {
	resolveConnection,
	hasScope,
	type RuntimeConnection
} from '$lib/connection.js';
import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
import { ProtocolError, isUnknownMethod } from '$lib/protocol/errors.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type {
	ObservabilityBucket,
	ObservabilityDimension,
	ObservabilityQualityBlock,
	ObservabilityQueryRequest,
	ObservabilityQueryResponse,
	ObservabilityQueryRow,
	ObservabilitySort
} from '../protocol/observability.js';
import {
	DEFAULT_OBSERVABILITY_LIMIT,
	DEFAULT_OBSERVABILITY_MEASURES,
	alignWindow,
	parseModelFilter,
	presetWindow,
	toUtcIso,
	type AlignedWindow
} from './derive.js';

/** A page-friendly `{code, message}` projection of a thrown error. */
export type PageError = { code: string; message: string };

/** The wire's closed set of freshness states, as a read-only tuple. */
export const OBSERVABILITY_STATES = ['current', 'catching_up', 'unavailable'] as const;

export class ObservabilityPageState {
	/* ---- connection + client (CONVENTIONS.md §6) ------------------- */
	connection = $state<RuntimeConnection | null>(null);
	/** True when the connection carries the elevated admin|console:fleet
	 * claim — surfaced only as an informational note; the consumer never
	 * exposes widening inputs (the body never widens). */
	canWiden = $state(false);
	/** Phase 83r disconnected predicate. */
	disconnected = $derived(this.connection === null);

	/* ---- page-level async state (the four-state contract) ---------- */
	status = $state<PageStatus>('loading');
	pageError = $state<PageError | null>(null);
	/** The `unknown_method` info banner (D-164) — the Runtime answered
	 * but does not mount `observability.query`. */
	info = $state<{ headline: string; detail: string } | null>(null);

	/* ---- query controls (closed wire sets) ------------------------ */
	bucket = $state<ObservabilityBucket>('hour');
	/** The active preset id ('' when the operator edited the window). */
	presetId = $state('24h');
	/** The ALIGNED query window (epoch ms + wire strings). */
	fromMs = $state(0);
	toMs = $state(0);
	/** Raw operator window edges (local wall time), aligned on query. */
	fromRaw = $state<string>('');
	toRaw = $state<string>('');
	/** Closed dimension subset to group by (empty = one row per bucket). */
	groupBy = $state<ObservabilityDimension[]>([]);
	/** The raw comma-separated model filter input. */
	modelFilterText = $state('');
	/** The selected closed measures (non-empty). */
	selectedMeasures = $state<string[]>([...DEFAULT_OBSERVABILITY_MEASURES]);
	/** The closed sort key (defaults to bucket ascending). */
	sort = $state<ObservabilitySort>('bucket_asc');
	/** The measure a measure sort keys on (a member of selectedMeasures). */
	sortMeasure = $state<string | null>(null);
	/** The page size (1..MaxRowsPerQuery, mandatory). */
	limit = $state(DEFAULT_OBSERVABILITY_LIMIT);

	/* ---- response state ------------------------------------------- */
	resp = $state<ObservabilityQueryResponse | null>(null);
	/** The cursor that produced each fetched page — index 0 is "" (the
	 * first page); next pushes, prev pops. Deterministic cursor paging,
	 * never a live-cursor redesign. */
	cursorStack = $state<string[]>(['']);
	pageIndex = $state(1);

	#client: ProtocolClient | null = null;
	/** Guards against out-of-order responses (a stale slow response is
	 * discarded, never applied over a newer one). */
	#requestSeq = 0;

	/* ================================================================ */
	/* Derived projections                                               */
	/* ================================================================ */

	/** The last response's mandatory freshness block, or null before the
	 * first successful response. */
	get quality(): ObservabilityQualityBlock | null {
		return this.resp?.quality ?? null;
	}

	/** The current page's rows. */
	get rows(): ObservabilityQueryRow[] {
		return this.resp?.rows ?? [];
	}

	/** The opaque cursor for the next page ('' = this is the last page). */
	get nextCursor(): string {
		return this.resp?.next_cursor ?? '';
	}

	get hasNextPage(): boolean {
		return this.nextCursor !== '';
	}

	/** The effective model filter (the closed `models` axis). */
	get modelFilter(): string[] {
		return parseModelFilter(this.modelFilterText);
	}

	/** The effective sort_measure — a member of the selected measures
	 * whenever a measure sort is active (falls back to the first selected
	 * measure; never a closed-measure violation). */
	get effectiveSortMeasure(): string | null {
		if (this.sort !== 'measure_asc' && this.sort !== 'measure_desc') return null;
		if (this.sortMeasure !== null && this.selectedMeasures.includes(this.sortMeasure)) {
			return this.sortMeasure;
		}
		return this.selectedMeasures[0] ?? null;
	}

	/** The label of the measure the current measure sort keys on, or null
	 * when no measure sort is active. */
	get sortMeasureLabel(): string | null {
		const m = this.effectiveSortMeasure;
		return m === null ? null : m;
	}

	/**
	 * The status the `<PageState>` boundary renders. `loading` / `error` /
	 * `info` / `disconnected` pass through; the ready/empty split is
	 * derived LIVE from the row count (Events-page D-180 pattern). The
	 * freshness banner (state / coverage) is rendered alongside — an
	 * empty page with a `gap` or `unavailable` stamp never reads as a
	 * plain "no data" result.
	 */
	get displayStatus(): PageStatus {
		if (
			this.status === 'loading' ||
			this.status === 'error' ||
			this.status === 'info' ||
			this.status === 'disconnected'
		) {
			return this.status;
		}
		return this.rows.length > 0 ? 'ready' : 'empty';
	}

	/* ================================================================ */
	/* Boot + loading                                                    */
	/* ================================================================ */

	/**
	 * Resolves the connection and loads page 1 of the default preset
	 * window. `injected` is an optional in-page client the harness
	 * supplies (read inside the page's onMount closure — never captured
	 * at construction).
	 */
	load(injected?: ProtocolClient): void {
		const connection = resolveConnection();
		this.connection = connection;
		if (connection === null) {
			this.#client = null;
			this.status = 'disconnected';
			return;
		}
		this.#client = injected ?? new HarborClient({ connection });
		this.canWiden = hasScope(connection, 'admin') || hasScope(connection, 'console:fleet');
		const preset = presetWindow(this.presetId, this.bucket, Date.now());
		this.applyAligned(preset, this.presetId);
		void this.loadPage(this.cursorStack[0], 1);
	}

	/**
	 * Re-run page 1 with the current controls. The window is RE-ALIGNED
	 * from the operator's raw edges so a hand-edited window is still
	 * grid-aligned before it reaches the wire (the wire rejects unaligned
	 * edges loudly).
	 */
	refresh(): void {
		if (this.#client === null) {
			this.status = this.disconnected ? 'disconnected' : 'loading';
			return;
		}
		this.applyAligned(
			alignWindow(this.#parseRawMs(this.fromRaw), this.#parseRawMs(this.toRaw), this.bucket),
			''
		);
		this.cursorStack = [''];
		this.pageIndex = 1;
		void this.loadPage('', 1);
	}

	/** Parse a datetime-local input string to epoch ms (NaN when empty). */
	#parseRawMs(raw: string): number {
		const ms = Date.parse(raw);
		return Number.isNaN(ms) ? 0 : ms;
	}

	/** Apply an aligned window (from a preset or the alignment choke
	 * point) and mirror it into the raw operator inputs (local wall
	 * time, so the input shows the same instant the query runs). */
	applyAligned(window: AlignedWindow, presetId: string): void {
		this.fromMs = window.fromMs;
		this.toMs = window.toMs;
		this.fromRaw = toLocalInputValue(window.fromMs);
		this.toRaw = toLocalInputValue(window.toMs);
		this.presetId = presetId;
	}

	/* ---- control actions (each re-runs page 1 from the first page —
	     the cursor is bound to the full query shape, so any control
	     change invalidates the cursor stack) ------------------------ */

	/** Apply a window preset (re-aligned to the current bucket grid). */
	applyPreset(id: string): void {
		if (this.#client === null) return;
		const window = presetWindow(id, this.bucket, Date.now());
		this.applyAligned(window, id);
		this.cursorStack = [''];
		this.pageIndex = 1;
		void this.loadPage('', 1);
	}

	/** Switch the bucket size and re-align the current window onto the
	 * new grid (floor start / ceil end — never silently rounded). */
	setBucket(bucket: ObservabilityBucket): void {
		if (this.#client === null || bucket === this.bucket) return;
		this.bucket = bucket;
		const window = alignWindow(this.fromMs, this.toMs, bucket);
		this.applyAligned(window, '');
		this.cursorStack = [''];
		this.pageIndex = 1;
		void this.loadPage('', 1);
	}

	/** Toggle one closed dimension in the group-by set (the query shape
	 * changes → restart from the first page). */
	toggleGroupBy(dim: ObservabilityDimension): void {
		if (this.#client === null) return;
		const next = this.groupBy.includes(dim)
			? this.groupBy.filter((d) => d !== dim)
			: [...this.groupBy, dim];
		this.groupBy = next;
		this.cursorStack = [''];
		this.pageIndex = 1;
		void this.loadPage('', 1);
	}

	/** Update the raw model-filter text (applied on the next query). */
	setModelFilterText(text: string): void {
		this.modelFilterText = text;
	}

	/** Apply the model filter (restart from the first page). */
	applyModelFilter(): void {
		if (this.#client === null) return;
		this.cursorStack = [''];
		this.pageIndex = 1;
		void this.loadPage('', 1);
	}

	/** Toggle one closed measure in the selection. The selection is never
	 * emptied (the wire requires a non-empty measures set); removing the
	 * sort measure resets it to null so a measure sort always keys on a
	 * member of the selection. */
	toggleMeasure(key: string): void {
		if (this.#client === null) return;
		const has = this.selectedMeasures.includes(key);
		if (has && this.selectedMeasures.length === 1) return;
		this.selectedMeasures = has
			? this.selectedMeasures.filter((m) => m !== key)
			: [...this.selectedMeasures, key];
		if (this.sortMeasure === key && has) {
			this.sortMeasure = null;
		}
		this.cursorStack = [''];
		this.pageIndex = 1;
		void this.loadPage('', 1);
	}

	/** Select the closed sort key. */
	setSort(sort: ObservabilitySort): void {
		if (this.#client === null || sort === this.sort) return;
		this.sort = sort;
		this.cursorStack = [''];
		this.pageIndex = 1;
		void this.loadPage('', 1);
	}

	/** Select the measure a measure sort keys on. */
	setSortMeasure(key: string): void {
		if (this.#client === null || key === this.effectiveSortMeasure) return;
		this.sortMeasure = key;
		this.cursorStack = [''];
		this.pageIndex = 1;
		void this.loadPage('', 1);
	}

	/** Change the page size (restart from the first page). */
	setLimit(limit: number): void {
		if (this.#client === null || limit === this.limit) return;
		this.limit = limit;
		this.cursorStack = [''];
		this.pageIndex = 1;
		void this.loadPage('', 1);
	}

	/* ---- cursor paging (deterministic, forward + backward) -------- */

	/** Fetch the next page via the response's opaque cursor. */
	nextPage(): void {
		const cursor = this.nextCursor;
		if (this.#client === null || cursor === '') return;
		this.cursorStack.push(cursor);
		this.pageIndex += 1;
		void this.loadPage(cursor, this.pageIndex);
	}

	/** Return to the previous page via the cursor stack. */
	prevPage(): void {
		if (this.#client === null || this.pageIndex <= 1) return;
		this.cursorStack.pop();
		this.pageIndex -= 1;
		void this.loadPage(this.cursorStack[this.pageIndex - 1] ?? '', this.pageIndex);
	}

	/* ================================================================ */
	/* Query execution                                                   */
	/* ================================================================ */

	/** The wire request for the current controls and one cursor. */
	buildRequest(cursor: string): ObservabilityQueryRequest {
		const models = this.modelFilter;
		const sortMeasure = this.effectiveSortMeasure;
		const req: ObservabilityQueryRequest = {
			from: toUtcIso(this.fromMs),
			to: toUtcIso(this.toMs),
			bucket: this.bucket,
			measures: [...this.selectedMeasures],
			limit: this.limit,
			...(this.groupBy.length > 0 ? { group_by: [...this.groupBy] } : {}),
			...(models.length > 0 ? { filters: { models } } : {}),
			...(this.sort !== 'bucket_asc' ? { sort: this.sort } : {}),
			...(sortMeasure !== null ? { sort_measure: sortMeasure } : {}),
			...(cursor !== '' ? { cursor } : {})
		};
		return req;
	}

	/** Run one page of `observability.query`. A stale (out-of-order)
	 * response is discarded; a thrown ProtocolError routes to the Error
	 * state; the `unknown_method` shape routes to the Info state (D-164)
	 * — a Runtime that does not mount the surface is not an error. */
	async loadPage(cursor: string, index: number): Promise<void> {
		const client = this.#client;
		if (client === null) {
			this.status = this.disconnected ? 'disconnected' : 'loading';
			return;
		}
		const seq = ++this.#requestSeq;
		this.status = 'loading';
		this.pageError = null;
		this.info = null;
		try {
			const resp = await client.observability.query(this.buildRequest(cursor));
			if (seq !== this.#requestSeq) return;
			this.resp = resp;
			if (index !== this.pageIndex) {
				this.pageIndex = index;
			}
			// ready/empty is derived live via `displayStatus`; set a
			// non-terminal status here so it flips out of `loading`.
			this.status = 'ready';
		} catch (err) {
			if (seq !== this.#requestSeq) return;
			if (isUnknownMethod(err)) {
				this.info = {
					headline: 'Observability query unavailable',
					detail:
						'This Runtime does not mount observability.query — the rollup projection surface is not part of its shape.'
				};
				this.status = 'info';
				return;
			}
			// The Error state suppresses any stale view — drop last-good data.
			this.resp = null;
			this.pageError = this.#toError(err);
			this.status = 'error';
		}
	}

	#toError(err: unknown): PageError {
		if (err instanceof ProtocolError) {
			return { code: err.code, message: err.message };
		}
		return { code: 'runtime_error', message: err instanceof Error ? err.message : 'unknown error' };
	}
}

/** Render an epoch-ms instant as a `datetime-local` input value (local
 * wall time, minute precision). */
export function toLocalInputValue(ms: number): string {
	const d = new Date(ms);
	if (Number.isNaN(d.getTime())) return '';
	const yyyy = d.getFullYear();
	const mm = String(d.getMonth() + 1).padStart(2, '0');
	const dd = String(d.getDate()).padStart(2, '0');
	const hh = String(d.getHours()).padStart(2, '0');
	const mi = String(d.getMinutes()).padStart(2, '0');
	return `${yyyy}-${mm}-${dd}T${hh}:${mi}`;
}
