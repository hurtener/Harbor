/**
 * Settings-page Protocol-client tests (Phase 73m / D-129).
 *
 * The `PostureNamespace` + `AuthNamespace` are exercised with an
 * injected `fetchImpl` stub so they are verified without a live
 * Runtime: route dispatch, the identity body, and the uniform
 * `(code, message, status)` error mapping onto the single
 * `ProtocolError`. The Settings page is a pure CONSUMER of the 72f /
 * 72g posture surfaces; the ONE net-new method is `auth.rotate_token`.
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

describe('Settings posture namespace', () => {
	it('routes runtime.info onto the control surface', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ instance_id: 'i1' }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.posture.info();
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/control/runtime.info');
		expect(init.method).toBe('POST');
		const body = JSON.parse(init.body as string);
		expect(body.identity).toEqual(CONNECTION.identity);
	});

	it('routes runtime.drivers / governance.posture / llm.posture onto the control surface', async () => {
		const fetchImpl = vi.fn(async () => okResponse({}));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.posture.drivers();
		await client.posture.governance();
		await client.posture.llm();
		const urls = fetchImpl.mock.calls.map((c) => (c as unknown as [string])[0]);
		expect(urls).toEqual([
			'http://127.0.0.1:18080/v1/control/runtime.drivers',
			'http://127.0.0.1:18080/v1/control/governance.posture',
			'http://127.0.0.1:18080/v1/control/llm.posture'
		]);
	});
});

describe('Settings auth namespace', () => {
	it('routes auth.rotate_token to POST /v1/auth/rotate_token', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({ new_token: 'rotated', expires_at: '2026-06-01T00:00:00Z' })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		const resp = await client.auth.rotateToken<{ new_token: string }>();
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/auth/rotate_token');
		expect(init.method).toBe('POST');
		expect(resp.new_token).toBe('rotated');
	});

	it('maps a 403 from auth.rotate_token onto a ProtocolError with status preserved', async () => {
		const fetchImpl = vi.fn(
			async () =>
				new Response(
					JSON.stringify({
						code: 'identity_scope_required',
						message: 'admin scope required'
					}),
					{ status: 403, headers: { 'Content-Type': 'application/json' } }
				)
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		try {
			await client.auth.rotateToken();
			expect.unreachable('expected a ProtocolError');
		} catch (err) {
			expect(err).toBeInstanceOf(ProtocolError);
			const pe = err as ProtocolError;
			expect(pe.code).toBe('identity_scope_required');
			expect(pe.status).toBe(403); // status is never dropped (CONVENTIONS.md §6).
		}
	});
});
