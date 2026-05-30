/**
 * Phase 105 (V1.2) — ConnectedRuntimesCard form-validation unit tests.
 *
 * Pins AC-1 + AC-4 of the Phase 105 plan: a six-field form whose
 * validation function gates submit. Each test exercises one rejection
 * shape so a future implementor that breaks validation sees a precise
 * red bar.
 */
import { describe, expect, it } from 'vitest';
import { validateAddRuntimeDraft, MIN_TOKEN_LENGTH } from './add-runtime-form.js';

/** A draft that satisfies every rule. Tests start from this and mutate. */
function validDraft() {
	return {
		name: 'staging',
		url: 'https://runtime.example.com',
		token:
			'header.' +
			'a'.repeat(Math.max(0, MIN_TOKEN_LENGTH - ('header.'.length + '.sig'.length))) +
			'.sig',
		tenant: 'dev',
		user: 'dev',
		session: 'dev'
	};
}

describe('validateAddRuntimeDraft — AC-1 / AC-4 rejection shapes', () => {
	it('a fully-valid draft returns null', () => {
		expect(validateAddRuntimeDraft(validDraft())).toBeNull();
	});

	it('an empty name is rejected', () => {
		const d = validDraft();
		d.name = '   ';
		expect(validateAddRuntimeDraft(d)).toMatch(/name/i);
	});

	it('an empty URL is rejected', () => {
		const d = validDraft();
		d.url = '';
		expect(validateAddRuntimeDraft(d)).toMatch(/url/i);
	});

	it('a malformed URL is rejected', () => {
		const d = validDraft();
		d.url = 'not-a-url';
		expect(validateAddRuntimeDraft(d)).toMatch(/valid URL/);
	});

	it('an empty token is rejected', () => {
		const d = validDraft();
		d.token = '';
		expect(validateAddRuntimeDraft(d)).toMatch(/token/i);
	});

	it('a too-short token is rejected', () => {
		const d = validDraft();
		d.token = 'a.b.c';
		expect(validateAddRuntimeDraft(d)).toMatch(/too short/i);
	});

	it('a token without three base64url segments is rejected', () => {
		const d = validDraft();
		// Long enough to pass the length check but not three-segment shaped.
		d.token = 'a'.repeat(MIN_TOKEN_LENGTH + 10);
		expect(validateAddRuntimeDraft(d)).toMatch(/JWT/);
	});

	it('an empty tenant is rejected', () => {
		const d = validDraft();
		d.tenant = '';
		expect(validateAddRuntimeDraft(d)).toMatch(/tenant/i);
	});

	it('a tenant with whitespace is rejected', () => {
		const d = validDraft();
		d.tenant = 'two words';
		expect(validateAddRuntimeDraft(d)).toMatch(/whitespace/i);
	});

	it('an empty user is rejected', () => {
		const d = validDraft();
		d.user = '';
		expect(validateAddRuntimeDraft(d)).toMatch(/user/i);
	});

	it('a user with whitespace is rejected', () => {
		const d = validDraft();
		d.user = ' a';
		expect(validateAddRuntimeDraft(d)).toMatch(/whitespace/i);
	});

	it('an empty session is ACCEPTED — session is per-request (D-171)', () => {
		const d = validDraft();
		d.session = '';
		expect(validateAddRuntimeDraft(d)).toBeNull();
	});

	it('a session with whitespace is rejected', () => {
		const d = validDraft();
		d.session = 'a\tb';
		expect(validateAddRuntimeDraft(d)).toMatch(/whitespace/i);
	});

	it('reports the first violation when several fields are bad — name beats URL', () => {
		const d = validDraft();
		d.name = '';
		d.url = '';
		expect(validateAddRuntimeDraft(d)).toMatch(/name/i);
	});

	it('reports the URL violation when name is OK but URL is malformed', () => {
		const d = validDraft();
		d.url = '://bad';
		expect(validateAddRuntimeDraft(d)).toMatch(/url|valid URL/i);
	});
});
