/**
 * HarborClient + ProtocolError tests (D-121).
 *
 * The unified Protocol client is exercised with an injected `fetchImpl`
 * stub so it is verified without a live Runtime: namespace dispatch, the
 * request shape (route, identity body, bearer + X-Harbor-* headers), and
 * the uniform `(code, message, status)` error mapping onto the single
 * `ProtocolError` — proving status is never dropped (CONVENTIONS.md §6).
 */
import { describe, expect, it, vi } from 'vitest';
import { HarborClient, ProtocolError } from '../harbor.js';
import type { RuntimeConnection } from '../../connection.js';

const CONNECTION: RuntimeConnection = {
	baseURL: 'http://127.0.0.1:18080',
	token: 'dummy-bearer-token',
	identity: { tenant: 't1', user: 'u1', session: 's1' },
	scopes: ['admin']
};

function okResponse(body: unknown): Response {
	return new Response(JSON.stringify(body), {
		status: 200,
		headers: { 'Content-Type': 'application/json' }
	});
}

function errorResponse(status: number, code: string, message: string): Response {
	return new Response(JSON.stringify({ code, message }), {
		status,
		headers: { 'Content-Type': 'application/json' }
	});
}

describe('HarborClient namespace dispatch', () => {
	it('routes tools.list to POST /v1/tools/list with the identity body', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ tools: [] }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.tools.list({ search: 'x' }, 2, 25);

		expect(fetchImpl).toHaveBeenCalledOnce();
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/tools/list');
		expect(init.method).toBe('POST');
		const body = JSON.parse(init.body as string);
		expect(body.identity).toEqual(CONNECTION.identity);
		expect(body.page).toBe(2);
		expect(body.page_size).toBe(25);
		const headers = init.headers as Record<string, string>;
		expect(headers['Authorization']).toBe('Bearer dummy-bearer-token');
		expect(headers['X-Harbor-Tenant']).toBe('t1');
	});

	it('routes memory.health to POST /v1/memory/health', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ aggregate: {} }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.memory.health();
		const [url] = fetchImpl.mock.calls[0] as unknown as [string];
		expect(url).toBe('http://127.0.0.1:18080/v1/memory/health');
	});

	it('routes artifacts.list onto the control surface', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ rows: [] }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.artifacts.list({ scope: CONNECTION.identity });
		const [url] = fetchImpl.mock.calls[0] as unknown as [string];
		expect(url).toBe('http://127.0.0.1:18080/v1/control/artifacts.list');
	});

	it('routes mcp.servers.list onto the control surface', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ servers: [] }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.mcp.servers.list();
		const [url] = fetchImpl.mock.calls[0] as unknown as [string];
		expect(url).toBe('http://127.0.0.1:18080/v1/control/mcp.servers.list');
	});

	// Phase 73b (D-126) — the Live Runtime page surfaces.
	it('routes topology.snapshot onto the control surface', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ engine_id: 'e', nodes: [], edges: [] }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.topology.snapshot();
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/control/topology.snapshot');
		const body = JSON.parse(init.body as string);
		expect(body.identity).toEqual(CONNECTION.identity);
	});

	it('routes tasks.list with the status-counter-strip opt-in flag', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({ rows: [], cursor: {}, aggregates: {}, status_counter_strip: {} })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.tasks.list({ include_status_counter_strip: true });
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/tasks/list');
		const body = JSON.parse(init.body as string);
		expect(body.include_status_counter_strip).toBe(true);
	});

	// Phase 73a / D-127 — the runtime + pause namespaces the Overview
	// page consumes. Both are composition over already-shipped surface.
	it('routes runtime.counters to the posture control surface', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({
				events_per_second: 1,
				tasks_running: 2,
				background_jobs_active: 0,
				mcp_connections_healthy: 3,
				sessions_active: 4,
				snapshot_at: 0
			})
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		const counters = await client.runtime.counters();
		const [url] = fetchImpl.mock.calls[0] as unknown as [string];
		expect(url).toBe('http://127.0.0.1:18080/v1/control/runtime.counters');
		expect(counters.tasks_running).toBe(2);
	});

	it('routes runtime.health to the posture control surface', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ subsystems: [] }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.runtime.health();
		const [url] = fetchImpl.mock.calls[0] as unknown as [string];
		expect(url).toBe('http://127.0.0.1:18080/v1/control/runtime.health');
	});

	it('routes pause.list to POST /v1/pause/list with the identity body', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({ snapshots: [], page: 1, page_size: 50, page_count: 0, total_rows: 0 })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.pause.list({ page: 1, page_size: 50 });
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/pause/list');
		const body = JSON.parse(init.body as string);
		expect(body.identity).toEqual(CONNECTION.identity);
		expect(body.page_size).toBe(50);
	});
});

describe('HarborClient events namespace (Phase 73g / D-125)', () => {
	it('routes events.aggregate to POST /v1/events/aggregate', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ buckets: [], protocol_version: '1.0' }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.events.aggregate({ filter: {}, window: 3_600_000_000_000, bucket: 60_000_000_000 });
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/events/aggregate');
		const body = JSON.parse(init.body as string);
		expect(body.identity).toEqual(CONNECTION.identity);
		expect(body.window).toBe(3_600_000_000_000);
	});

	it('builds the events.subscribe SSE URL with type + admin + access_token', () => {
		const client = new HarborClient({ connection: CONNECTION });
		const url = new URL(
			client.events.subscribeURL({ eventTypes: ['tool.failed', 'planner.error'], admin: true })
		);
		expect(url.pathname).toBe('/v1/events');
		expect(url.searchParams.getAll('type')).toEqual(['tool.failed', 'planner.error']);
		expect(url.searchParams.get('admin')).toBe('1');
		// EventSource cannot set an Authorization header — token rides as a param.
		expect(url.searchParams.get('access_token')).toBe('dummy-bearer-token');
	});

	it('omits the admin param for a triple-scoped subscription', () => {
		const client = new HarborClient({ connection: CONNECTION });
		const url = new URL(client.events.subscribeURL());
		expect(url.searchParams.has('admin')).toBe(false);
	});
});

describe('HarborClient error mapping', () => {
	it('raises a ProtocolError carrying code, message AND status', async () => {
		const fetchImpl = vi.fn(async () =>
			errorResponse(403, 'scope_mismatch', 'admin scope required')
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await expect(client.flows.run({ flow_id: 'f1', inputs: {} })).rejects.toThrowError(
			ProtocolError
		);
		try {
			await client.flows.run({ flow_id: 'f1', inputs: {} });
			expect.unreachable('expected a ProtocolError');
		} catch (err) {
			expect(err).toBeInstanceOf(ProtocolError);
			const pe = err as ProtocolError;
			expect(pe.code).toBe('scope_mismatch');
			expect(pe.message).toBe('admin scope required');
			expect(pe.status).toBe(403); // status is never dropped (D-121).
		}
	});

	it('never silently degrades on a non-JSON error body', async () => {
		const fetchImpl = vi.fn(
			async () => new Response('upstream exploded', { status: 502 })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await expect(client.tools.get('t1')).rejects.toThrowError(ProtocolError);
		try {
			await client.tools.get('t1');
		} catch (err) {
			const pe = err as ProtocolError;
			expect(pe.status).toBe(502);
			expect(pe.message).toContain('upstream exploded');
		}
	});
});

// Phase 108b — the app-shell ⌘K global-search launcher consumes the
// shipped `search.query` method. These tests pin the route + body shape
// and verify the decoder reads a CAPTURED REAL `search.query` frame
// (PAGE-POLISH-PROCEDURE §3.2/§3.3 — verify against real data, snake_case
// RPC body). The frame below was captured live (2026-05-30) from the
// validation runtime after seeding a youtube task.
describe('search.query namespace (Phase 108b)', () => {
	it('routes search.query onto the control surface with the request body', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({
				rows: [],
				page: 1,
				page_size: 8,
				page_count: 1,
				total_count: 0,
				has_more: false
			})
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.search.query({ query: 'youtube', page_size: 8 });

		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/control/search.query');
		const body = JSON.parse(init.body as string);
		expect(body.identity).toEqual(CONNECTION.identity);
		expect(body.query).toBe('youtube');
		expect(body.page_size).toBe(8);
	});

	it('decodes a captured real search.query frame (task + heavy-artifact ref)', async () => {
		// Verbatim from the live runtime — a task row (inline preview) and an
		// artifact row whose 686KB JSON preview ships by-reference (D-026).
		const realFrame = {
			rows: [
				{
					index: 'tasks',
					id: '01KSX5GR8Z2X16TJJ3XEJSH229',
					tenant_id: 'dev',
					user_id: 'dev',
					session_id: 'dev',
					occurred_at: '2026-05-30T19:25:00Z',
					preview: 'task 01KSX5GR8Z2X16TJJ3XEJSH229 status=complete kind=foreground'
				},
				{
					index: 'artifacts',
					id: 'default_e5aa9d3aa0f6',
					tenant_id: 'dev',
					user_id: 'dev',
					session_id: 'dev',
					ref: {
						id: 'default_e5aa9d3aa0f6',
						mime_type: 'application/json',
						size_bytes: 686923,
						filename: 'tool-result-youtube_get_metadata.json'
					}
				}
			],
			page: 1,
			page_size: 8,
			page_count: 1,
			total_count: 2,
			has_more: false,
			protocol_version: '0.1.0'
		};
		const fetchImpl = vi.fn(async () => okResponse(realFrame));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		const resp = await client.search.query({ query: 'youtube' });

		expect(resp.total_count).toBe(2);
		expect(resp.rows).toHaveLength(2);
		expect(resp.rows[0].index).toBe('tasks');
		expect(resp.rows[0].preview).toContain('status=complete');
		// Heavy-content row carries a `ref`, not an inline preview (D-026).
		expect(resp.rows[1].index).toBe('artifacts');
		expect(resp.rows[1].preview).toBeUndefined();
		expect(resp.rows[1].ref?.mime_type).toBe('application/json');
		expect(resp.rows[1].ref?.filename).toBe('tool-result-youtube_get_metadata.json');
	});
});
