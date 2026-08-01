/**
 * Regression (D-374): the six Console methods whose Protocol request types
 * declare NO `identity` field must not fold one into the request body.
 *
 * `HarborTransport.request` folds the identity triple into every request body
 * by default — BELOW the typed client surface, so the caller's own typing can
 * never see it. Six request types scope by something other than `identity`:
 * the five `artifacts.*` types scope by `scope`, and `SearchRequest` scopes by
 * `filter`. Now that the Runtime's control transport decodes strictly, a
 * folded `identity` on any of them is refused `unknown field "identity"`
 * (HTTP 400). Four `artifacts-page` Playwright specs caught the artifacts
 * five; `search.query` has NO e2e coverage and would have failed in production
 * instead.
 *
 * This is the behavioural half of the guard. The static half lives in
 * `scripts/check-protocol-ts-lockstep.mjs` (check (e), in `npm run lint`) and
 * asserts the call site PASSES `omitBodyIdentity`; this asserts the flag
 * actually suppresses the fold on the wire. A test that only checked the
 * source would go green against a transport that ignored the option.
 *
 * The sibling `set-posture.test.ts` covers the seventh such method,
 * `governance.set_posture`, which is where this class was first found.
 */
import { describe, expect, it, vi } from 'vitest';
import { HarborClient } from '../harbor.js';
import type { RuntimeConnection } from '../../connection.js';

const CONNECTION: RuntimeConnection = {
	baseURL: 'http://127.0.0.1:18080',
	token: 'dummy-bearer-token',
	identity: { tenant: 't1', user: 'u1', session: 's1' },
	scopes: ['admin']
};

/** A fetch stub returning an empty 200 JSON envelope. */
function okFetch() {
	return vi.fn(
		async () =>
			new Response(JSON.stringify({}), {
				status: 200,
				headers: { 'Content-Type': 'application/json' }
			})
	);
}

const SCOPE = { tenant: 't1', user: 'u1', session: 's1' };

describe('identity-less request types do not receive the body identity fold (D-374)', () => {
	for (const [name, invoke] of [
		['artifacts.list', (c: HarborClient) => c.artifacts.list({ scope: SCOPE })],
		[
			'artifacts.put',
			(c: HarborClient) => c.artifacts.put({ scope: SCOPE, bytes: '', opts: {} })
		],
		['artifacts.get', (c: HarborClient) => c.artifacts.get({ scope: SCOPE, id: 'a1' })],
		['artifacts.get_ref', (c: HarborClient) => c.artifacts.getRef({ scope: SCOPE, id: 'a1' })],
		['artifacts.delete', (c: HarborClient) => c.artifacts.delete({ scope: SCOPE, id: 'a1' })],
		['search.query', (c: HarborClient) => c.search.query({ query: 'x' })]
	] as const) {
		it(`${name} sends no identity member, and identity still rides the headers`, async () => {
			const fetchImpl = okFetch();
			const client = new HarborClient({ connection: CONNECTION, fetchImpl });
			await invoke(client);

			const [, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
			const body = JSON.parse((init.body ?? '{}') as string) as Record<string, unknown>;
			expect(body).not.toHaveProperty('identity');

			// The identity is not lost — dropping the fold must not drop the
			// scope the handler's `resolveIdentity` reads.
			const headers = init.headers as Record<string, string>;
			expect(headers['X-Harbor-Tenant']).toBe('t1');
			expect(headers['X-Harbor-User']).toBe('u1');
		});
	}
});
