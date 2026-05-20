/**
 * HarborClient `memory.*` namespace tests (D-121).
 *
 * The Memory-page refactor (CONVENTIONS.md §6) deleted the legacy
 * hand-authored `protocol-memory.ts` client; the `memory.*` surface now
 * hangs off the unified `HarborClient`. This spec replaces the deleted
 * `protocol-memory.spec.ts`: it pins the wire shape (route, identity
 * body, bearer header) and the typed responses for `memory.list` /
 * `memory.get` / `memory.health`, plus the uniform `ProtocolError`
 * mapping that proves status is never dropped — all against an injected
 * `fetchImpl`, with no live Runtime.
 */
import { describe, expect, it, vi } from 'vitest';
import { HarborClient, ProtocolError } from '../harbor.js';
import type {
	MemoryGetResponse,
	MemoryHealthResponse,
	MemoryListResponse
} from '../memory-types.js';
import type { RuntimeConnection } from '../../connection.js';

const CONNECTION: RuntimeConnection = {
	baseURL: 'http://runtime.test',
	token: 'tok-abc',
	identity: { tenant: 't1', user: 'u1', session: 's1' },
	scopes: []
};

function okResponse(body: unknown): Response {
	return new Response(JSON.stringify(body), {
		status: 200,
		headers: { 'Content-Type': 'application/json' }
	});
}

describe('HarborClient memory namespace', () => {
	it('memory.list POSTs to /v1/memory/list with the identity body + bearer', async () => {
		const listResp: MemoryListResponse = {
			items: [],
			page: 1,
			page_size: 50,
			page_count: 0,
			total_rows: 0,
			aggregates: {
				total: 0,
				expiring_in_1h: 0,
				identity_rejected_24h: 0,
				recovery_dropped_24h: 0
			},
			protocol_version: '1.0.0'
		};
		const fetchImpl = vi.fn(async () => okResponse(listResp));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });

		const got = await client.memory.list({ filter: { scopes: ['session'] } });
		expect(got.protocol_version).toBe('1.0.0');

		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://runtime.test/v1/memory/list');
		expect(init.method).toBe('POST');
		const body = JSON.parse(init.body as string);
		expect(body.identity).toEqual(CONNECTION.identity);
		expect(body.filter.scopes).toEqual(['session']);
		const headers = init.headers as Record<string, string>;
		expect(headers['Authorization']).toBe('Bearer tok-abc');
		expect(headers['X-Harbor-Tenant']).toBe('t1');
	});

	it('memory.get round-trips a light-value detail', async () => {
		const getResp: MemoryGetResponse = {
			detail: {
				item: {
					key: 'mem_abc',
					strategy: 'truncation',
					scope: 'session',
					identity: { tenant: 't', user: 'u', session: 's' },
					created_at: '2026-05-20T00:00:00Z',
					last_updated_at: '2026-05-20T00:00:00Z',
					size_bytes: 42,
					driver: 'inmem'
				},
				value: '{"x":1}',
				metadata: {}
			},
			protocol_version: '1.0.0'
		};
		const fetchImpl = vi.fn(async () => okResponse(getResp));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });

		const got = await client.memory.get('mem_abc');
		expect(got.detail.item.key).toBe('mem_abc');
		expect(got.detail.value).toBe('{"x":1}');
		expect(got.detail.value_artifact).toBeUndefined();
		const [url] = fetchImpl.mock.calls[0] as unknown as [string];
		expect(url).toBe('http://runtime.test/v1/memory/get');
	});

	it('memory.health round-trips the aggregate counters', async () => {
		const healthResp: MemoryHealthResponse = {
			aggregate: {
				total: 7,
				expiring_in_1h: 1,
				identity_rejected_24h: 2,
				recovery_dropped_24h: 0,
				driver_by_scope: { session: 'inmem' }
			},
			protocol_version: '1.0.0'
		};
		const fetchImpl = vi.fn(async () => okResponse(healthResp));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });

		const got = await client.memory.health();
		expect(got.aggregate.total).toBe(7);
		expect(got.aggregate.driver_by_scope.session).toBe('inmem');
	});

	it('raises a ProtocolError carrying code + status on a non-2xx', async () => {
		const fetchImpl = vi.fn(
			async () =>
				new Response(
					JSON.stringify({
						code: 'identity_scope_required',
						message: 'cross-tenant memory.list requires a verified admin scope claim'
					}),
					{ status: 403 }
				)
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });

		await expect(
			client.memory.list({ filter: { tenant_ids: ['t-other'] } })
		).rejects.toThrowError(ProtocolError);

		try {
			await client.memory.list({ filter: { tenant_ids: ['t-other'] } });
			expect.unreachable('memory.list should have thrown');
		} catch (err) {
			const pe = err as ProtocolError;
			expect(pe.code).toBe('identity_scope_required');
			expect(pe.status).toBe(403); // status is never dropped (D-121).
		}
	});
});
