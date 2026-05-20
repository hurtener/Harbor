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
