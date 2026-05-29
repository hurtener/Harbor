/**
 * Phase 105 (V1.2) — AttachToLocalCard bootstrap-fetch unit tests.
 *
 * Pins AC-9 of the Phase 105 plan: the card branches on the bootstrap
 * endpoint's response status. 200 attaches; 403/404 surface a neutral
 * info banner; everything else (including network errors) is an error
 * banner. The flow's logic is extracted to attach-to-local.ts so it can
 * be tested without rendering the Svelte component.
 */
import { describe, expect, it, vi } from 'vitest';
import { runAttachToLocal, type BootstrapEnvelope } from './attach-to-local.js';

function jsonResponse(body: unknown, status = 200): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json' }
	});
}

const validEnvelope: BootstrapEnvelope = {
	base_url: 'http://127.0.0.1:18080',
	token: 'header.payload.sig',
	identity: { tenant: 'dev', user: 'dev', session: 'dev' },
	scopes: ['admin', 'console:fleet'],
	protocol_version: '0.1.0'
};

describe('runAttachToLocal — AC-9 branches', () => {
	it('200 → attached, envelope returned verbatim', async () => {
		const fetchImpl = vi.fn(async () => jsonResponse(validEnvelope));
		const outcome = await runAttachToLocal({
			fetch: fetchImpl as unknown as typeof globalThis.fetch,
			origin: 'http://127.0.0.1:18790'
		});
		expect(outcome.kind).toBe('attached');
		if (outcome.kind === 'attached') {
			expect(outcome.envelope.token).toBe('header.payload.sig');
			expect(outcome.envelope.identity.tenant).toBe('dev');
		}
		// The fetch URL is composed from origin + bootstrap path.
		expect(fetchImpl).toHaveBeenCalledWith(
			'http://127.0.0.1:18790/v1/dev/bootstrap.json',
			expect.objectContaining({ method: 'POST', credentials: 'omit' })
		);
	});

	it('403 → info banner ("not available; use the manual form")', async () => {
		const fetchImpl = vi.fn(async () => new Response('forbidden', { status: 403 }));
		const outcome = await runAttachToLocal({
			fetch: fetchImpl as unknown as typeof globalThis.fetch,
			origin: 'http://example.com'
		});
		expect(outcome.kind).toBe('info');
		if (outcome.kind === 'info') {
			expect(outcome.message).toMatch(/not available/i);
			expect(outcome.message).toMatch(/manual form/i);
		}
	});

	it('404 → info banner ("not registered on this build")', async () => {
		const fetchImpl = vi.fn(async () => new Response('not found', { status: 404 }));
		const outcome = await runAttachToLocal({
			fetch: fetchImpl as unknown as typeof globalThis.fetch,
			origin: 'http://example.com'
		});
		expect(outcome.kind).toBe('info');
		if (outcome.kind === 'info') {
			expect(outcome.message).toMatch(/this build/i);
		}
	});

	it('non-2xx other than 403 / 404 → error banner with the status code', async () => {
		const fetchImpl = vi.fn(async () => new Response('boom', { status: 500 }));
		const outcome = await runAttachToLocal({
			fetch: fetchImpl as unknown as typeof globalThis.fetch,
			origin: 'http://example.com'
		});
		expect(outcome.kind).toBe('error');
		if (outcome.kind === 'error') {
			expect(outcome.message).toMatch(/500/);
		}
	});

	it('network error → error banner with the thrown message', async () => {
		const fetchImpl = vi.fn(async () => {
			throw new Error('connection refused');
		});
		const outcome = await runAttachToLocal({
			fetch: fetchImpl as unknown as typeof globalThis.fetch,
			origin: 'http://127.0.0.1:18790'
		});
		expect(outcome.kind).toBe('error');
		if (outcome.kind === 'error') {
			expect(outcome.message).toBe('connection refused');
		}
	});

	it('non-Error throw → fallback error message (no silent degradation)', async () => {
		const fetchImpl = vi.fn(async () => {
			throw 'unexpected-string-throw';
		});
		const outcome = await runAttachToLocal({
			fetch: fetchImpl as unknown as typeof globalThis.fetch,
			origin: 'http://127.0.0.1:18790'
		});
		expect(outcome.kind).toBe('error');
		if (outcome.kind === 'error') {
			expect(outcome.message).toMatch(/network error/i);
		}
	});
});
