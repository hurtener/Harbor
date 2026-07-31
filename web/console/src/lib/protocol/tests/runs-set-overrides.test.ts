/**
 * Regression: `runs.set_overrides` carries ONLY the real override fields
 * (v1.13 checkpoint F1 / W1).
 *
 * A `top_p` field leaked from the Controls card into the composed
 * override set. The Playground adapter mapped it to `payload.top_p`, the
 * typed client forwarded it, and the runtime's `runs.set_overrides`
 * decoder (`DisallowUnknownFields()`, `runs_handler.go`) rejected the
 * WHOLE request with 400 — so reasoning-effort / temperature / max-tokens
 * / system-prompt overrides ALL silently failed together.
 *
 * The fix removed `top_p` end-to-end and typed `setOverrides` against the
 * named `RunOverrides` wire interface so a phantom key is a compile
 * error. This test is the runtime guard: the fake fetch mirrors the
 * runtime's `DisallowUnknownFields()` contract (any key outside the wire
 * shape → 400), then asserts the four real fields round-trip cleanly and
 * that no phantom key reaches the wire.
 */
import { describe, expect, it, vi } from 'vitest';
import { HarborClient } from '../harbor.js';
import type { RunOverrides } from '../runs.js';
import type { RuntimeConnection } from '../../connection.js';

const CONNECTION: RuntimeConnection = {
	baseURL: 'http://127.0.0.1:18080',
	token: 'dummy-bearer-token',
	identity: { tenant: 't1', user: 'u1', session: 's1' },
	scopes: ['admin']
};

/** The keys `RunOverrides` (internal/protocol/types/runs.go) accepts. */
const ALLOWED_OVERRIDE_KEYS = new Set([
	'session_id',
	'reasoning_effort',
	'temperature',
	'max_tokens',
	'system_prompt_override',
	'extra_instructions',
	'model'
]);

/**
 * A fetch stub that mirrors the runtime's `DisallowUnknownFields()`
 * decoder for `runs.set_overrides`: any override key outside the wire
 * shape → 400 (the exact failure mode the `top_p` leak produced). A clean
 * body returns the shipped `{applied_at, protocol_version}` response.
 */
function strictSetOverridesFetch() {
	return vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
		const body = JSON.parse((init?.body ?? '{}') as string) as {
			overrides?: Record<string, unknown>;
		};
		const overrides = body.overrides ?? {};
		const phantom = Object.keys(overrides).filter((k) => !ALLOWED_OVERRIDE_KEYS.has(k));
		if (phantom.length > 0) {
			return new Response(
				JSON.stringify({
					code: 'invalid_request',
					message: `json: unknown field "${phantom[0]}"`
				}),
				{ status: 400, headers: { 'Content-Type': 'application/json' } }
			);
		}
		return new Response(
			JSON.stringify({
				applied_at: '2026-07-10T00:00:00Z',
				protocol_version: '1.0.0'
			}),
			{ status: 200, headers: { 'Content-Type': 'application/json' } }
		);
	});
}

describe('runs.set_overrides wire round-trip', () => {
	it('carries all four real override fields and no phantom key', async () => {
		const fetchImpl = strictSetOverridesFetch();
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });

		const overrides: RunOverrides = {
			session_id: 's1',
			reasoning_effort: 'high',
			temperature: 0.7,
			max_tokens: 4096,
			system_prompt_override: 'You are terse.'
		};

		// The round-trip SUCCEEDS — the strict decoder does not 400.
		await expect(client.runs.setOverrides(overrides)).resolves.toBeDefined();

		expect(fetchImpl).toHaveBeenCalledOnce();
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('http://127.0.0.1:18080/v1/runs/set_overrides');
		const sent = JSON.parse(init.body as string).overrides as Record<string, unknown>;

		// Exactly the four real fields plus the mandatory session id — and
		// crucially NO `top_p` (or any other phantom key).
		expect(new Set(Object.keys(sent))).toEqual(
			new Set(['session_id', 'reasoning_effort', 'temperature', 'max_tokens', 'system_prompt_override'])
		);
		expect(sent).not.toHaveProperty('top_p');
	});

	it('carries extra_instructions alongside system_prompt_override', async () => {
		// The additive block and the whole-spine replace are NOT mutually
		// exclusive: the base prompt is replaced AND the additive guidance
		// still appends. Both must survive the strict decoder in one request.
		const fetchImpl = strictSetOverridesFetch();
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });

		const overrides: RunOverrides = {
			session_id: 's1',
			system_prompt_override: 'You are terse.',
			extra_instructions: 'Cite every source.'
		};
		await expect(client.runs.setOverrides(overrides)).resolves.toBeDefined();

		const [, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
		const sent = JSON.parse(init.body as string).overrides as Record<string, unknown>;
		expect(sent.extra_instructions).toBe('Cite every source.');
		expect(sent.system_prompt_override).toBe('You are terse.');
	});

	it('a phantom override key would 400 the whole request (the guarded regression)', async () => {
		const fetchImpl = strictSetOverridesFetch();
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });

		// Cast past the compile-time guard to prove the runtime contract the
		// typed `RunOverrides` shape protects against: a stray key fails the
		// WHOLE request, not just itself.
		const bogus = { session_id: 's1', top_p: 0.9 } as unknown as RunOverrides;
		await expect(client.runs.setOverrides(bogus)).rejects.toThrow(/unknown field/);
	});
});
