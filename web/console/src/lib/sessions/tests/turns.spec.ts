// Vitest for the durable conversation-turn projection consumer (HA-64 /
// D-425): the two-read open tail page, opaque older-page cursor paging with
// stable ordering under append, and the bounded terminal reconciliation
// read. These pin the consumer contract the Playground reopen depends on:
// no forensic event reconstruction, no silent cursor resets, and the
// operations DTO structurally unreachable on the consumer transcript path.

import { describe, expect, it } from 'vitest';

import type { ProtocolClient } from '../../protocol/client.js';
import type {
	SessionTurnRow,
	SessionTurnsGetResponse,
	SessionTurnsListResponse
} from '../../protocol/session-turns.js';
import { ProtocolError } from '../../protocol/errors.js';
import {
	classifyTurnPageError,
	loadTurnPage,
	mergeTurnPages,
	reconcileTurnRow,
	turnPageFromResponse,
	TurnPageError,
	TURN_PAGE_DEFAULT_LIMIT,
	TURN_PAGE_MAX_LIMIT
} from '../turns.js';

/** A minimal well-formed turn row the page renders. */
function turnRow(id: string, overrides: Partial<SessionTurnRow> = {}): SessionTurnRow {
	return {
		turn_id: id,
		task_id: id,
		session_id: 's1',
		sequence: 0,
		tie_breaker: id,
		status: 'complete',
		sealed: true,
		version: 1,
		last_applied_event_seq: 1,
		started_at: '2026-07-10T12:00:00Z',
		updated_at: '2026-07-10T12:00:01Z',
		finished_at: '2026-07-10T12:00:01Z',
		agent: { complete: 'complete' },
		query: { text: 'hello', complete: 'complete' },
		answer: { state: 'inline', inline: 'hi', seq: 1, complete: 'complete' },
		pause: { availability: 'unavailable' },
		usage: {
			prompt_tokens: { state: 'unavailable' },
			completion_tokens: { state: 'unavailable' },
			reasoning_tokens: { state: 'unavailable' },
			cache_read_tokens: { state: 'unavailable' },
			cache_write_tokens: { state: 'unavailable' },
			total_tokens: { state: 'unavailable' },
			cost_micro_usd: { state: 'unavailable' },
			latency_ns: { state: 'unavailable' }
		},
		reasoning: { steps: [], complete: 'unavailable', seq: 0 },
		activity: {
			rows: [],
			complete: 'complete',
			more: false,
			totals: {
				invoked: 0,
				succeeded: 0,
				failed: 0,
				cancelled: 0,
				retried: 0,
				policy_exhausted: 0
			}
		},
		...overrides
	};
}

/** A recording fake of the typed `sessionTurns` namespace. */
function fakeTurns(
	script: {
		list?: (req: Record<string, unknown>) => Promise<SessionTurnsListResponse>;
		get?: (req: Record<string, unknown>) => Promise<SessionTurnsGetResponse>;
	} = {}
): {
	client: ProtocolClient;
	listCalls: Array<Record<string, unknown>>;
	getCalls: Array<Record<string, unknown>>;
} {
	const listCalls: Array<Record<string, unknown>> = [];
	const getCalls: Array<Record<string, unknown>> = [];
	const client = {
		sessionTurns: {
			list: async (req: Record<string, unknown>) => {
				listCalls.push(req);
				if (script.list !== undefined) return script.list(req);
				return {
					header: { session_id: 's1', snapshot_id: 1, as_of: '2026-07-10T12:00:01Z' },
					turns: [],
					order: 'newest_first',
					has_more: false,
					count_exact: true,
					live_resume_seq: 0,
					page_completeness: 'complete',
					protocol_version: '0.1.0'
				};
			},
			get: async (req: Record<string, unknown>) => {
				getCalls.push(req);
				if (script.get !== undefined) return script.get(req);
				return { session_id: 's1', protocol_version: '0.1.0' };
			}
		}
	} as unknown as ProtocolClient;
	return { client, listCalls, getCalls };
}

function pageResp(turns: SessionTurnRow[], over: Partial<SessionTurnsListResponse> = {}): SessionTurnsListResponse {
	return {
		header: { session_id: 's1', snapshot_id: 1, as_of: '2026-07-10T12:00:01Z' },
		turns,
		order: 'newest_first',
		has_more: false,
		count_exact: true,
		live_resume_seq: turns.length,
		page_completeness: 'complete',
		protocol_version: '0.1.0',
		...over
	};
}

describe('loadTurnPage — the two-read open tail page (HA-64 / D-425)', () => {
	it('requests the newest page with the session id and the bounded default limit', async () => {
		const { client, listCalls } = fakeTurns();
		const page = await loadTurnPage(client, { sessionID: 's1' });
		expect(listCalls).toEqual([{ session_id: 's1', limit: TURN_PAGE_DEFAULT_LIMIT }]);
		expect(page.order).toBe('newest_first');
		expect(page.hasMore).toBe(false);
		expect(page.pageCompleteness).toBe('complete');
	});

	it('clamps an over-max limit to the Protocol maximum (never asks for a loud 400)', async () => {
		const { client, listCalls } = fakeTurns();
		await loadTurnPage(client, { sessionID: 's1', limit: 500 });
		expect(listCalls[0]).toEqual({ session_id: 's1', limit: TURN_PAGE_MAX_LIMIT });
	});

	it('passes the opaque older-page cursor back verbatim (keyset/snapshot paging)', async () => {
		const { client, listCalls } = fakeTurns();
		await loadTurnPage(client, { sessionID: 's1', olderCursor: 'snap-7/seq-42/turn-x', limit: 10 });
		expect(listCalls[0]).toEqual({
			session_id: 's1',
			limit: 10,
			older_cursor: 'snap-7/seq-42/turn-x'
		});
	});

	it('surfaces the honest page-completeness contract (partial + reason)', async () => {
		const { client } = fakeTurns({
			list: async () =>
				pageResp([turnRow('t1')], {
					has_more: false,
					page_completeness: 'partial',
					partial_reason: 'retention_eviction',
					remaining_older_count: 3,
					count_exact: true
				})
		});
		const page = await loadTurnPage(client, { sessionID: 's1' });
		expect(page.pageCompleteness).toBe('partial');
		expect(page.partialReason).toBe('retention_eviction');
		expect(page.remainingOlderCount).toBe(3);
		expect(page.countExact).toBe(true);
	});

	it('never fabricates a remaining count the store does not know exactly', async () => {
		const { client } = fakeTurns({
			list: async () =>
				pageResp([turnRow('t1')], {
					remaining_older_count: undefined,
					count_exact: false
				})
		});
		const page = await loadTurnPage(client, { sessionID: 's1' });
		expect(page.remainingOlderCount).toBeNull();
		expect(page.countExact).toBe(false);
	});

	it('classifies a refused cursor as the typed invalid_cursor outcome — never a silent reset', async () => {
		const { client } = fakeTurns({
			list: async () => {
				throw new ProtocolError('invalid_request', 'older cursor refused', 400);
			}
		});
		await expect(loadTurnPage(client, { sessionID: 's1', olderCursor: 'stale' })).rejects.toMatchObject({
			kind: 'invalid_cursor',
			code: 'invalid_request'
		});
	});

	it('classifies typed not_found (foreign/erased session) and unknown_method distinctly', async () => {
		const nf = classifyTurnPageError(new ProtocolError('not_found', 'no such session', 404));
		expect(nf.kind).toBe('not_found');
		expect(nf.code).toBe('not_found');
		const um = classifyTurnPageError(new ProtocolError('unknown_method', 'no surface', 501));
		expect(um.kind).toBe('unknown_method');
		expect(um.code).toBe('unknown_method');
		const tr = classifyTurnPageError(new ProtocolError('runtime_error', 'boom', 500));
		expect(tr.kind).toBe('transport');
		expect(classifyTurnPageError(new TurnPageError('invalid_cursor', 'x'))).toBeInstanceOf(
			TurnPageError
		);
	});
});

describe('mergeTurnPages — stable ordering under append (D-425)', () => {
	it('flattens newest-first pages newest-first with no duplicate page-boundary rows', () => {
		const newest = turnPageFromResponse(
			pageResp([turnRow('t4'), turnRow('t3')], { has_more: true, next_older_cursor: 'cur-1' })
		);
		const older = turnPageFromResponse(pageResp([turnRow('t2'), turnRow('t1')]));
		const merged = mergeTurnPages([newest, older]);
		expect(merged.turns.map((r) => r.turn_id)).toEqual(['t4', 't3', 't2', 't1']);
		expect(merged.totalTurns).toBe(4);
		expect(merged.hasMore).toBe(false);
		expect(merged.truncated).toBe(false);
	});

	it('keeps a row exactly once when a new turn appends while paging older history', () => {
		// The newest page was re-read AFTER the append: t5 joined the tail.
		const newest = turnPageFromResponse(
			pageResp([turnRow('t5'), turnRow('t4'), turnRow('t3')], { has_more: true })
		);
		// The older page was minted BEFORE the append — it must not contain t5.
		const older = turnPageFromResponse(pageResp([turnRow('t2'), turnRow('t1')]));
		const merged = mergeTurnPages([newest, older]);
		expect(merged.turns.map((r) => r.turn_id)).toEqual(['t5', 't4', 't3', 't2', 't1']);
	});

	it('dedupes a boundary row that appears in two pages (no skip, no duplicate)', () => {
		const newest = turnPageFromResponse(
			pageResp([turnRow('t4'), turnRow('t3')], { has_more: true })
		);
		const older = turnPageFromResponse(pageResp([turnRow('t3'), turnRow('t2')]));
		const merged = mergeTurnPages([newest, older]);
		expect(merged.turns.map((r) => r.turn_id)).toEqual(['t4', 't3', 't2']);
	});

	it('propagates retention truncation from any partial page', () => {
		const newest = turnPageFromResponse(
			pageResp([turnRow('t4')], { has_more: true, page_completeness: 'partial', partial_reason: 'retention_eviction' })
		);
		const older = turnPageFromResponse(pageResp([turnRow('t3')]));
		const merged = mergeTurnPages([newest, older]);
		expect(merged.truncated).toBe(true);
		expect(merged.partialReason).toBe('retention_eviction');
	});
});

describe('reconcileTurnRow — the ONE bounded terminal reconciliation read', () => {
	it('issues one sessions.turns.get on the consumer lane with no projection', async () => {
		const { client, getCalls } = fakeTurns({
			get: async () => ({ session_id: 's1', turn: turnRow('t9'), protocol_version: '0.1.0' })
		});
		const row = await reconcileTurnRow(client, 's1', 't9');
		expect(getCalls).toEqual([{ session_id: 's1', task_id: 't9' }]);
		expect(row.turn_id).toBe('t9');
	});

	it('NEVER lets the operations DTO reach a consumer transcript (ops-only → typed error)', async () => {
		// A structurally distinct `ops_turn` (no query/answer/reasoning/App
		// URI) must not be served as a consumer turn — it would be a widened
		// read leaking onto the transcript path.
		const { client, getCalls } = fakeTurns({
			get: async () => ({
				session_id: 's1',
				ops_turn: {
					turn_id: 't9',
					task_id: 't9',
					session_id: 's1',
					sequence: 1,
					tie_breaker: 't9',
					status: 'complete',
					sealed: true,
					version: 1,
					started_at: '2026-07-10T12:00:00Z',
					updated_at: '2026-07-10T12:00:01Z',
					finished_at: '2026-07-10T12:00:01Z',
					usage: {
						prompt_tokens: { state: 'unavailable' },
						completion_tokens: { state: 'unavailable' },
						reasoning_tokens: { state: 'unavailable' },
						cache_read_tokens: { state: 'unavailable' },
						cache_write_tokens: { state: 'unavailable' },
						total_tokens: { state: 'unavailable' },
						cost_micro_usd: { state: 'unavailable' },
						latency_ns: { state: 'unavailable' }
					},
					activity: {
						rows: [],
						complete: 'complete',
						more: false,
						totals: {
							invoked: 0,
							succeeded: 0,
							failed: 0,
							cancelled: 0,
							retried: 0,
							policy_exhausted: 0
						}
					},
					reasoning_steps: 0,
					inputs: 0,
					outputs: 0,
					pause: { availability: 'unavailable' },
					last_applied_event_seq: 1
				},
				protocol_version: '0.1.0'
			})
		});
		await expect(reconcileTurnRow(client, 's1', 't9')).rejects.toMatchObject({
			kind: 'not_found'
		});
		expect(getCalls).toEqual([{ session_id: 's1', task_id: 't9' }]);
	});

	it('classifies a not_found get as the typed refusal', async () => {
		const { client } = fakeTurns({
			get: async () => {
				throw new ProtocolError('not_found', 'turn not found', 404);
			}
		});
		await expect(reconcileTurnRow(client, 's1', 'missing')).rejects.toMatchObject({
			kind: 'not_found'
		});
	});
});
