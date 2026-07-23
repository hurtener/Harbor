/**
 * Regression (HA-31): the Console's `governance.set_posture` client must NOT
 * fold the identity triple into the request body.
 *
 * `GovernanceSetPostureRequest` (internal/protocol/types/governance.go) carries
 * NO identity field, and the handler decodes it with `DisallowUnknownFields()`.
 * The shared `Transport.request` folds the identity triple into EVERY other
 * admin write's body, so a folded `identity` key here is rejected with
 * `unknown field "identity"` (HTTP 400) — 400-ing the Console's own
 * Governance-Posture edit card on every submit. The fix routes `setPosture`
 * through the transport's `omitBodyIdentity` opt-out; identity still rides the
 * `X-Harbor-*` headers, so the handler's server-side `resolveIdentity` is
 * unaffected. This test mirrors the runtime's identity-less strict decoder and
 * asserts no `identity` key reaches the wire.
 */
import { describe, expect, it, vi } from 'vitest';
import { HarborClient } from '../harbor.js';
import type { GovernanceSetPostureRequest } from '../governance.js';
import type { RuntimeConnection } from '../../connection.js';

const CONNECTION: RuntimeConnection = {
	baseURL: 'http://127.0.0.1:18080',
	token: 'dummy-bearer-token',
	identity: { tenant: 't1', user: 'u1', session: 's1' },
	scopes: ['admin']
};

/**
 * A fetch stub that mirrors the runtime's identity-LESS `set_posture` decoder
 * (`DisallowUnknownFields()`): any body key outside {default_tier,
 * identity_tiers} — crucially a folded `identity` — → 400, the exact failure
 * the shipped Console hit. A clean body returns the shipped response shape.
 */
function strictSetPostureFetch() {
	return vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
		const body = JSON.parse((init?.body ?? '{}') as string) as Record<string, unknown>;
		const allowed = new Set(['default_tier', 'identity_tiers']);
		const unknown = Object.keys(body).filter((k) => !allowed.has(k));
		if (unknown.length > 0) {
			return new Response(
				JSON.stringify({
					code: 'invalid_request',
					message: `json: unknown field "${unknown[0]}"`
				}),
				{ status: 400, headers: { 'Content-Type': 'application/json' } }
			);
		}
		return new Response(
			JSON.stringify({
				default_tier: (body.default_tier as string) ?? '',
				identity_tiers: body.identity_tiers ?? {},
				enforcement_pending_restart: false,
				protocol_version: '0.1.0'
			}),
			{ status: 200, headers: { 'Content-Type': 'application/json' } }
		);
	});
}

describe('governance.set_posture wire round-trip (HA-31)', () => {
	it('omits the body identity fold so the identity-less strict handler does not 400', async () => {
		const fetchImpl = strictSetPostureFetch();
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });

		const req: GovernanceSetPostureRequest = { default_tier: 'free', identity_tiers: {} };
		// The round-trip SUCCEEDS — no folded `identity` key reaches the decoder.
		await expect(client.governance.setPosture(req)).resolves.toBeDefined();

		expect(fetchImpl).toHaveBeenCalledOnce();
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/governance/set_posture');
		const sent = JSON.parse(init.body as string) as Record<string, unknown>;

		// The fix: the body carries ONLY the two real fields, never `identity`
		// (a folded `identity` key would 400 the whole request).
		expect(sent).not.toHaveProperty('identity');
		expect(new Set(Object.keys(sent))).toEqual(new Set(['default_tier', 'identity_tiers']));

		// Identity still rides the X-Harbor-* headers — server-side
		// resolveIdentity is unaffected by dropping the body fold.
		const headers = init.headers as Record<string, string>;
		expect(headers['X-Harbor-Tenant']).toBe('t1');
		expect(headers['X-Harbor-Session']).toBe('s1');
	});
});
