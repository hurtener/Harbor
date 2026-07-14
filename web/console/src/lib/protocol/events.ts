/**
 * Events wire types — the `events.*` Protocol shapes the Console Events
 * page consumes (Phase 73g, D-125).
 *
 * # Wire types only — the client lives in `harbor.ts`
 *
 * This module is the wire-type surface only: the request / response /
 * frame shapes the `EventsNamespace` methods (in `harbor.ts`) consume
 * and return. They mirror `internal/protocol/types/events.go` and the
 * SSE `wireEvent` projection in
 * `internal/protocol/transports/stream/frame.go` field-for-field — the
 * Go side is the single source (D-002 / D-093). When
 * `cmd/harbor-gen-protocol-ts` (D-093) ships, these absorb into the
 * generated `protocol.ts`.
 *
 * # No new Protocol method (Phase 73g)
 *
 * Phase 73g ships NO new Protocol method. `events.subscribe`
 * (`GET /v1/events` SSE — Phase 72) and `events.aggregate`
 * (`POST /v1/events/aggregate` — Phase 72a) are already shipped; the
 * truncated-payload `Open artifact` link routes through the shipped
 * `artifacts.get_ref` (Phase 73l). This page is a pure UI consumer of
 * that surface.
 *
 * # Heavy payloads flow by reference (D-026)
 *
 * The event filter operates on event HEADER fields only — type +
 * identity + timestamp. Heavy payload bytes are NEVER inlined: a
 * payload exceeding the heavy-content threshold carries an
 * {@link EventArtifactRef} the page resolves via `artifacts.get_ref`.
 *
 * # events.list (D-294) reuses the flat StateEvent row
 *
 * The `events.list` historical-window read returns the SAME flat
 * {@link StateEvent} projection `state.history` carries — imported from
 * `./state`, not re-declared (no new row shape).
 */

import type { StateEvent } from './state';

/* ------------------------------------------------------------------ */
/* EventFilter — mirrors types.EventFilter                             */
/* ------------------------------------------------------------------ */

/**
 * The canonical wire predicate every `events.*` method consumes —
 * `events.subscribe` for live subscriptions and `events.aggregate` for
 * time-bucketed count series. Mirrors `types.EventFilter`.
 *
 * Identity is mandatory. Empty `user_ids` / `session_ids` is
 * interpreted as "the caller's own identity tuple"; a `tenant_ids` set
 * with more than one entry, or a single tenant other than the caller's,
 * requires the `auth.ScopeAdmin` OR `auth.ScopeConsoleFleet` claim
 * (D-079 — there is NO dedicated `events.crosstenant` scope).
 */
export interface EventFilter {
	/** Narrows to events whose `type` is in the set. Empty matches all. */
	event_types?: string[];
	/** Tenant axis — >1 entry (or a foreign tenant) needs an admin scope. */
	tenant_ids?: string[];
	/** User axis. Empty = the caller's own user. */
	user_ids?: string[];
	/** Session axis. Empty = the caller's own session. */
	session_ids?: string[];
	/** Run axis. Empty = unconstrained. */
	run_ids?: string[];
	/** Optional inclusive lower bound on `occurred_at` (RFC-3339 UTC). */
	since?: string;
	/** Optional exclusive upper bound on `occurred_at` (RFC-3339 UTC). */
	until?: string;
}

/* ------------------------------------------------------------------ */
/* Event — mirrors the SSE wireEvent projection                        */
/* ------------------------------------------------------------------ */

/**
 * One event as it crosses the `events.subscribe` SSE wire — the flat
 * `wireEvent` projection in `frame.go`. The `payload` is whatever the
 * event's typed payload marshalled to (already redaction-safe by
 * construction — the bus runs the redactor on Publish, D-020); a heavy
 * payload carries an {@link EventArtifactRef} instead of inline bytes
 * (D-026).
 */
export interface Event {
	/** The dotted canonical event-type name (e.g. `tool.failed`). */
	type: string;
	/** The per-bus monotonic sequence — also the SSE reconnect cursor. */
	sequence: number;
	/** The wall-clock instant the event occurred (RFC-3339 UTC). */
	occurred_at: string;
	/** The publishing tenant. */
	tenant: string;
	/** The publishing user. */
	user: string;
	/** The publishing session. */
	session: string;
	/** The publishing run, when the event is run-scoped. */
	run?: string;
	/** The typed, redaction-safe payload — or an {@link EventArtifactRef}. */
	payload?: unknown;
	/** Free-form string sidecar attributes (`source`, `severity`, …). */
	extra?: Record<string, string>;
}

/**
 * A heavy-payload reference. When an event's payload exceeds the
 * heavy-content threshold (RFC §6.5 / D-026) the runtime emits the
 * payload as an `artifact_ref`-shaped object rather than inline bytes;
 * the Events page renders a `Truncated` badge + an `Open artifact` link
 * that resolves the bytes via `artifacts.get_ref`. The page NEVER
 * inlines heavy bytes into a Svelte component.
 */
export interface EventArtifactRef {
	/** Discriminator the page checks via {@link isEventArtifactRef}. */
	artifact_ref: {
		/** The artifact id the `artifacts.get_ref` resolver keys on. */
		id: string;
		/** The artifact's MIME type, when known. */
		mime?: string;
		/** The artifact's byte size, when known. */
		size?: number;
	};
}

/**
 * Narrows an event payload to an {@link EventArtifactRef}. A `true`
 * result means the page MUST route through `artifacts.get_ref` — it
 * must never read bytes off the payload directly (D-026).
 */
export function isEventArtifactRef(payload: unknown): payload is EventArtifactRef {
	return (
		typeof payload === 'object' &&
		payload !== null &&
		'artifact_ref' in payload &&
		typeof (payload as { artifact_ref: unknown }).artifact_ref === 'object' &&
		(payload as { artifact_ref: unknown }).artifact_ref !== null &&
		typeof (payload as EventArtifactRef).artifact_ref.id === 'string'
	);
}

/* ------------------------------------------------------------------ */
/* events.aggregate — mirrors types.EventAggregate{Request,Response}   */
/* ------------------------------------------------------------------ */

/**
 * One time-bucketed count series — one stripe of the per-event-type
 * stacked-area sparkline. Mirrors `types.EventBucket`. `bucket_start`
 * (inclusive) and `bucket_end` (exclusive) are RFC-3339 UTC; `counts`
 * is keyed by event-type string. Empty buckets are present (empty
 * `counts`) so the rendering client scans a contiguous time axis.
 */
export interface EventBucket {
	/** The bucket's lower bound (inclusive, RFC-3339 UTC). */
	bucket_start: string;
	/** The bucket's upper bound (exclusive, RFC-3339 UTC). */
	bucket_end: string;
	/** event-type → count of events in this bucket. */
	counts: Record<string, number>;
}

/**
 * The wire request for `events.aggregate`. Mirrors
 * `types.EventAggregateRequest`. `window` / `bucket` are Go
 * `time.Duration` nanosecond integers — `bucket` must evenly divide
 * `window` or the runtime rejects with `invalid_request`.
 */
export interface EventAggregateRequest {
	/** The event predicate the aggregator counts under. */
	filter: EventFilter;
	/** The inclusive lookback span, in nanoseconds (Go `time.Duration`). */
	window: number;
	/** The per-bucket width, in nanoseconds — must divide `window`. */
	bucket: number;
	/**
	 * OPTIONAL origin of the bucket grid (RFC-3339 UTC). Absent ⇒ the
	 * clock-anchored default (buckets run `[now-window, now)`, a
	 * `bucket_start` is NOT addressable twice). Set ⇒ boundaries are
	 * floored onto the fixed grid `anchor + k·bucket`, so two calls at
	 * two instants with the same `anchor` + `window` + `bucket` share
	 * bucket coordinates — a `bucket_start` is cacheable/re-requestable
	 * (mirrors `types.EventAggregateRequest.Anchor`, D-306). Passing the
	 * Unix epoch ({@link EPOCH_ANCHOR}) yields a globally-shared grid.
	 * Grid-edge note: with an anchor the LAST bucket's `bucket_end` is
	 * the grid boundary covering now — generally just AFTER now, never
	 * treat it as "up to this instant."
	 */
	anchor?: string;
}

/**
 * The Unix-epoch grid origin (`1970-01-01T00:00:00Z`). Passing it as
 * {@link EventAggregateRequest.anchor} floors every bucket boundary onto
 * `epoch + k·bucket`, giving a globally-shared, addressable grid the
 * Events page can cache and stitch cold windows against (D-306).
 */
export const EPOCH_ANCHOR = new Date(0).toISOString();

/** The wire response for `events.aggregate`. Mirrors `types.EventAggregateResponse`. */
export interface EventAggregateResponse {
	/** The per-bucket count series, oldest-first. */
	buckets: EventBucket[];
	/**
	 * True when the counts are PARTIAL — the aggregation could not observe
	 * every matching event in the window (a best-effort ring evicted older
	 * events below the window, or a durable log held more matching events than
	 * the single-read aggregation bound). Set uniformly across drivers; an
	 * over-wide window is DATA (partial buckets flagged truncated), never a
	 * request error. Absent/false ⇒ the counts are complete for the window.
	 */
	truncated?: boolean;
	/** The Protocol version the Runtime answered under. */
	protocol_version: string;
}

/* ------------------------------------------------------------------ */
/* events.list — mirrors types.EventsList{Request,Response}            */
/* ------------------------------------------------------------------ */

/** Default + maximum page size for `events.list`, mirroring the Go bounds. */
export const DEFAULT_EVENTS_LIST_LIMIT = 50;
export const MAX_EVENTS_LIST_LIMIT = 200;

/**
 * The wire request for `events.list` — the durable, time-ranged,
 * cross-session raw-event read. Mirrors `types.EventsListRequest`. Reuses
 * {@link EventFilter} (identity axes + `since`/`until` bounds + event-type
 * selector) plus a tail-first, sequence-based paging cursor mirroring
 * `state.history`. The `identity` triple is folded server-side; a
 * cross-tenant (fleet) filter requires the verified `admin` or
 * `console:fleet` claim (D-079). `cursor` zero ⇒ from the newest retained
 * events; pass the prior response's `next_cursor` back to scroll a page
 * older.
 */
export interface EventsListRequest {
	/** The event predicate (identity sets + since/until + types). */
	filter: EventFilter;
	/** Exclusive upper sequence bound; 0 (or omitted) ⇒ from the tail. */
	cursor?: number;
	/** Page size K (clamped to {@link MAX_EVENTS_LIST_LIMIT}; 0 ⇒ default). */
	limit?: number;
}

/**
 * The wire response for `events.list`. Mirrors
 * `types.EventsListResponse`. Rows are the SAME flat {@link StateEvent}
 * projection `state.history` and the SSE stream carry — no new row shape.
 * `next_cursor` is the value to pass back as `cursor` to scroll one page
 * older (0 at the retained head); `truncated` is the honest retention-gap
 * signal (a best-effort ring evicted older events).
 */
export interface EventsListResponse {
	/** The page's events, oldest-first within the window. */
	events: StateEvent[];
	/** Lowest sequence in this page — the scroll-up cursor (0 at the head). */
	next_cursor: number;
	/** Whether older matching events remain before this page. */
	has_more: boolean;
	/** True when a wrapped best-effort ring may have evicted older rows. */
	truncated?: boolean;
}

/* ------------------------------------------------------------------ */
/* Console-local view vocabulary                                       */
/* ------------------------------------------------------------------ */

/**
 * The active sparkline / subscription window. Console-local UI state —
 * the runtime is window-agnostic; the page derives `since` / `window` /
 * `bucket` from the selected `TimeWindow`. Mirrors page-events.md §12.
 */
export type TimeWindow = '5m' | '1h' | '24h' | '7d';

/** Nanoseconds per millisecond — the Go `time.Duration` unit bridge. */
const NS_PER_MS = 1_000_000;

/**
 * The window → (span, bucket) table page-events.md §12 pins:
 * 5 min: 30 s buckets; 1 h: 1 min; 24 h: 5 min; 7 d: 1 h. Spans and
 * buckets are expressed in nanoseconds so they feed
 * {@link EventAggregateRequest} directly.
 */
export const WINDOW_SPEC: Record<TimeWindow, { windowNs: number; bucketNs: number; label: string }> = {
	'5m': { windowNs: 5 * 60_000 * NS_PER_MS, bucketNs: 30_000 * NS_PER_MS, label: 'Last 5 min' },
	'1h': { windowNs: 60 * 60_000 * NS_PER_MS, bucketNs: 60_000 * NS_PER_MS, label: 'Last 1 h' },
	'24h': { windowNs: 24 * 60 * 60_000 * NS_PER_MS, bucketNs: 5 * 60_000 * NS_PER_MS, label: 'Last 24 h' },
	'7d': { windowNs: 7 * 24 * 60 * 60_000 * NS_PER_MS, bucketNs: 60 * 60_000 * NS_PER_MS, label: 'Last 7 d' }
};
