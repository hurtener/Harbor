// Harbor Console — durable conversation-turn projection consumer (HA-64 /
// D-425), the two-read chat-open read surface.
//
// This module is the FIRST consumer of the `sessions.turns.*` Protocol
// surface (CLAUDE.md §13 primitive-with-consumer rule): the normal session
// reopen reads ONE lifecycle-only session snapshot (Phase 245 / HA-63, done
// by the page) plus ONE `sessions.turns.list` tail page — it never
// reconstructs a conversation from forensic `state.history` windows,
// `tasks.list`, `tasks.get`, or `events.list` joins. Older pages load by
// passing the response's opaque `next_older_cursor` back verbatim (a
// snapshot/keyset cursor anchored on an immutable task/turn tie-breaker),
// which preserves stable newest-first ordering under append — a new turn
// starting while the operator pages older history produces neither
// duplicates nor omissions. Terminal reconciliation after live streaming
// uses ONE `sessions.turns.get` — never a raw-transcript refetch.
//
// Honesty contract (CLAUDE.md §13, D-425):
//   - `page_completeness` partial (retention eviction) is surfaced with its
//     `partial_reason` — never presented as a fabricated complete page.
//   - A refused older-page cursor (malformed / foreign / stale-snapshot /
//     retention-expired) surfaces as a distinct typed `TurnPageError` —
//     never a silent reset to page one.
//   - The consumer lane is exact-session and structurally consumer-safe:
//     `reconcileTurnRow` reads ONLY the consumer `turn` field and never the
//     operations DTO (`ops_turn`) — an operations-only response is a typed
//     error, not a widened transcript read.

import type { ProtocolClient } from '../protocol/client.js';
import type {
	SessionTurnHeader,
	SessionTurnRow,
	SessionTurnsListRequest,
	SessionTurnsListResponse,
	SessionTurnsGetRequest,
	SessionTurnsGetResponse
} from '../protocol/session-turns.js';
import { ProtocolError } from '../protocol/errors.js';

/** The Protocol-mandated default page size (Go `turns.DefaultListLimit`). */
export const TURN_PAGE_DEFAULT_LIMIT = 20;
/** The Protocol-mandated maximum page size (Go `turns.MaxListLimit`). */
export const TURN_PAGE_MAX_LIMIT = 50;

/** The declared stable page order the Runtime serves. */
export const TURN_PAGE_ORDER_NEWEST_FIRST = 'newest_first';

/** Options for one {@link loadTurnPage} read. */
export interface TurnPageOptions {
	/** The effective session (must be the caller's own exact session). */
	sessionID: string;
	/** The opaque exclusive older-page cursor; empty means the newest page. */
	olderCursor?: string;
	/** Page bound; 0/omitted → {@link TURN_PAGE_DEFAULT_LIMIT}. Above the
	 *  maximum fails loud server-side, so the consumer clamps to it. */
	limit?: number;
}

/** One normalized `sessions.turns.list` page — newest-first turn rows plus
 *  the full honest paging contract. */
export interface TurnPage {
	/** The session header: session id, projection snapshot id, as-of. */
	header: SessionTurnHeader;
	/** The page's turns, newest first, effective-agent-gated. */
	turns: SessionTurnRow[];
	/** The declared stable order — always `newest_first`. */
	order: string;
	/** The opaque exclusive older-page cursor; empty when `hasMore` is false. */
	nextOlderCursor?: string;
	/** Whether older turns remain within the retained window. */
	hasMore: boolean;
	/** Exact count of older RETAINED turns when known exactly (null/omitted
	 *  otherwise — never a fabricated count). */
	remainingOlderCount?: number | null;
	/** Whether {@link remainingOlderCount} is exact. */
	countExact: boolean;
	/** The durable event-log sequence of the newest observation reflected in
	 *  this page — the exclusive live-resume cursor (subscribe from +1). */
	liveResumeSeq: number;
	/** Explicit page completeness: `complete` or `partial` (retention
	 *  eviction). Never a fabricated empty. */
	pageCompleteness: 'complete' | 'partial';
	/** Why the page is partial (`retention_eviction`); empty when complete. */
	partialReason?: string;
}

/** The merged result of paging newest-first pages. */
export interface LoadedTurnPages {
	/** Every loaded page, newest-first page order. */
	pages: TurnPage[];
	/** The flattened turn rows, newest-first, deduped by `turn_id`. */
	turns: SessionTurnRow[];
	/** Whether older turns remain beyond the last loaded page. */
	hasMore: boolean;
	/** The total number of deduped turns loaded. */
	totalTurns: number;
	/** True when ANY loaded page was partial (retention eviction). */
	truncated: boolean;
	/** The first partial page's reason (empty when not truncated). */
	partialReason?: string;
}

/** The distinct typed outcomes of a refused turn-page read. */
export type TurnPageErrorKind =
	/** Malformed / forged / foreign-session / stale-snapshot / retention-
	 *  expired cursor — the wire surfaces these as `invalid_request` (the
	 *  handler maps the domain's umbrella `ErrInvalidCursor`), and the
	 *  consumer names them honestly without guessing which binding failed. */
	| 'invalid_cursor'
	/** The session (or a foreign read) answered typed not-found. */
	| 'not_found'
	/** The runtime predates the `sessions.turns.*` surface. */
	| 'unknown_method'
	/** Anything else — a real transport / runtime failure. */
	| 'transport';

/** The typed refusal of a turn-page read. Distinct from a bare
 *  `ProtocolError` so a consumer can render an honest cursor-expiry / gap /
 *  unavailable state instead of an empty or silently-reset page. */
export class TurnPageError extends Error {
	readonly kind: TurnPageErrorKind;
	/** The underlying canonical Protocol code, when there was one. */
	readonly code?: string;

	constructor(kind: TurnPageErrorKind, message: string, code?: string) {
		super(message);
		this.name = 'TurnPageError';
		this.kind = kind;
		this.code = code;
	}
}

/** Classifies a thrown error into the typed turn-page refusal set. Accepts
 *  the canonical `ProtocolError` AND the defensive plain `{code, message}`
 *  shape (some legacy call sites pre-unwrap — the same tolerance
 *  `isUnknownMethod` in `$lib/protocol/errors.js` has). */
export function classifyTurnPageError(err: unknown): TurnPageError {
	if (err instanceof TurnPageError) return err;
	if (err instanceof ProtocolError || (err !== null && typeof err === 'object' && 'code' in err)) {
		const code =
			err instanceof ProtocolError
				? err.code
				: typeof (err as { code: unknown }).code === 'string'
					? ((err as { code: string }).code as string)
					: 'runtime_error';
		const message = err instanceof Error ? err.message : String(err);
		if (code === 'unknown_method') {
			return new TurnPageError('unknown_method', message, code);
		}
		if (code === 'not_found') {
			return new TurnPageError('not_found', message, code);
		}
		if (code === 'invalid_request' || code === 'invalid_cursor') {
			// The handler maps the domain's cursor sentinels (foreign /
			// stale-snapshot / retention-expired / forged) to this umbrella
			// code — the honest consumer label is "the cursor was refused".
			return new TurnPageError('invalid_cursor', message, code);
		}
		return new TurnPageError('transport', message, code);
	}
	return new TurnPageError(
		'transport',
		err instanceof Error ? err.message : String(err),
		'runtime_error'
	);
}

/** Normalizes one `sessions.turns.list` wire response into a {@link TurnPage}. */
export function turnPageFromResponse(resp: SessionTurnsListResponse): TurnPage {
	const complete = resp.page_completeness === 'complete';
	return {
		header: resp.header,
		turns: resp.turns ?? [],
		order: resp.order || TURN_PAGE_ORDER_NEWEST_FIRST,
		...(resp.next_older_cursor !== undefined && resp.next_older_cursor !== ''
			? { nextOlderCursor: resp.next_older_cursor }
			: {}),
		hasMore: resp.has_more === true,
		remainingOlderCount: resp.remaining_older_count ?? null,
		countExact: resp.count_exact === true,
		liveResumeSeq: resp.live_resume_seq ?? 0,
		pageCompleteness: complete ? 'complete' : 'partial',
		...(resp.partial_reason !== undefined && resp.partial_reason !== ''
			? { partialReason: resp.partial_reason }
			: {})
	};
}

/**
 * One `sessions.turns.list` read — the newest page when no cursor is
 * supplied, or one older page when the response's opaque
 * {@link TurnPage.nextOlderCursor} is passed back verbatim. A refused
 * cursor throws a typed {@link TurnPageError}.
 */
export async function loadTurnPage(
	client: ProtocolClient,
	opts: TurnPageOptions
): Promise<TurnPage> {
	let limit = opts.limit ?? TURN_PAGE_DEFAULT_LIMIT;
	if (limit < 1) limit = TURN_PAGE_DEFAULT_LIMIT;
	if (limit > TURN_PAGE_MAX_LIMIT) limit = TURN_PAGE_MAX_LIMIT;
	const req: SessionTurnsListRequest = {
		session_id: opts.sessionID,
		limit
	};
	if (opts.olderCursor !== undefined && opts.olderCursor !== '') {
		req.older_cursor = opts.olderCursor;
	}
	// The consumer lane omits `projection` entirely — the operations lane is
	// get-only and rejected server-side on the list surface; the consumer
	// never asks for it (D-425).
	try {
		const resp: SessionTurnsListResponse = await client.sessionTurns.list(req);
		return turnPageFromResponse(resp);
	} catch (err) {
		throw classifyTurnPageError(err);
	}
}

/**
 * Merges newest-first pages (the newest page first) into one flattened,
 * deduped, newest-first row list. Stable under append: a turn that starts
 * while older pages are being paged appears only in the newest page, and
 * the `turn_id` dedup keeps a page-boundary row once — no skip, no
 * duplicate.
 */
export function mergeTurnPages(pages: readonly TurnPage[]): LoadedTurnPages {
	const seen = new Set<string>();
	const turns: SessionTurnRow[] = [];
	for (const page of pages) {
		for (const row of page.turns) {
			if (seen.has(row.turn_id)) continue;
			seen.add(row.turn_id);
			turns.push(row);
		}
	}
	const last = pages.length > 0 ? pages[pages.length - 1] : undefined;
	const partial = pages.find((p) => p.pageCompleteness === 'partial');
	return {
		pages: [...pages],
		turns,
		hasMore: last?.hasMore === true,
		totalTurns: turns.length,
		truncated: partial !== undefined,
		...(partial?.partialReason !== undefined && partial.partialReason !== ''
			? { partialReason: partial.partialReason }
			: {})
	};
}

/**
 * The bounded terminal reconciliation read after live streaming: ONE
 * `sessions.turns.get` on the CONSUMER lane. The sealed row is
 * authoritative; this never refetches the raw transcript (`state.history` /
 * `events.list`). The operations DTO is structurally unreachable here —
 * only `resp.turn` is read, and an ops-only response is a typed error, so a
 * widened/operations read can never leak into a consumer transcript path.
 */
export async function reconcileTurnRow(
	client: ProtocolClient,
	sessionID: string,
	taskID: string
): Promise<SessionTurnRow> {
	const req: SessionTurnsGetRequest = {
		session_id: sessionID,
		task_id: taskID
		// Consumer lane: `projection` omitted — the operations lane requires
		// an elevated claim and is never requested from a consumer transcript.
	};
	let resp: SessionTurnsGetResponse;
	try {
		resp = await client.sessionTurns.get(req);
	} catch (err) {
		throw classifyTurnPageError(err);
	}
	if (resp.turn === undefined) {
		// An absent consumer turn (or an ops-only response — the structurally
		// distinct DTO) must never reach this transcript path.
		throw new TurnPageError(
			'not_found',
			`sessions.turns.get returned no consumer turn for task ${taskID}`,
			'not_found'
		);
	}
	return resp.turn;
}
