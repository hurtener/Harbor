/**
 * connection.ts tests (D-121).
 *
 * The single Runtime-connection resolver: it returns a resolved
 * `RuntimeConnection` when every storage key is present, and `null`
 * (the honest "no Runtime attached" signal — never an error) when any
 * key is missing (CONVENTIONS.md §6/§8).
 */
import { afterEach, describe, expect, it } from 'vitest';
import { STORAGE_KEYS, hasScope, isConnected, resolveConnection } from '../connection.js';

afterEach(() => {
	localStorage.clear();
});

function seedConnection(): void {
	localStorage.setItem(STORAGE_KEYS.baseURL, 'http://127.0.0.1:18080/');
	localStorage.setItem(STORAGE_KEYS.token, 'dummy-token');
	localStorage.setItem(STORAGE_KEYS.tenant, 't1');
	localStorage.setItem(STORAGE_KEYS.user, 'u1');
	localStorage.setItem(STORAGE_KEYS.session, 's1');
}

describe('resolveConnection', () => {
	it('returns a resolved connection when every key is present', () => {
		seedConnection();
		const conn = resolveConnection();
		expect(conn).not.toBeNull();
		// The trailing slash on the base URL is normalised away.
		expect(conn?.baseURL).toBe('http://127.0.0.1:18080');
		expect(conn?.token).toBe('dummy-token');
		expect(conn?.identity).toEqual({ tenant: 't1', user: 'u1', session: 's1' });
		expect(isConnected()).toBe(true);
	});

	it('returns null when no keys are set (not attached, not an error)', () => {
		expect(resolveConnection()).toBeNull();
		expect(isConnected()).toBe(false);
	});

	it('returns null when any single key is missing', () => {
		seedConnection();
		localStorage.removeItem(STORAGE_KEYS.session);
		expect(resolveConnection()).toBeNull();
	});

	it('resolves the comma-separated scope claims, empty when unset', () => {
		seedConnection();
		expect(resolveConnection()?.scopes).toEqual([]);
		localStorage.setItem(STORAGE_KEYS.scopes, 'admin, observer');
		expect(resolveConnection()?.scopes).toEqual(['admin', 'observer']);
	});
});

describe('hasScope', () => {
	it('is true only when the connection carries the scope', () => {
		seedConnection();
		localStorage.setItem(STORAGE_KEYS.scopes, 'admin');
		const conn = resolveConnection();
		// scope present
		expect(hasScope(conn, 'admin')).toBe(true);
		// scope absent
		expect(hasScope(conn, 'observer')).toBe(false);
	});

	it('is false (never throws) for a null connection', () => {
		expect(hasScope(null, 'admin')).toBe(false);
	});

	it('is false for every scope when the persisted scope set is empty', () => {
		seedConnection();
		// The scopes key is unset — resolveConnection yields an empty []
		// scope set. hasScope must return false for any scope, never throw.
		const conn = resolveConnection();
		expect(conn?.scopes).toEqual([]);
		expect(hasScope(conn, 'admin')).toBe(false);
		expect(hasScope(conn, '')).toBe(false);
	});

	it('is false when the persisted scope value is empty or whitespace-only', () => {
		seedConnection();
		// A malformed persisted value — empty string, lone comma,
		// whitespace — parses to an empty scope set, not a crash.
		for (const malformed of ['', '   ', ',', ', ,', '\t,\n']) {
			localStorage.setItem(STORAGE_KEYS.scopes, malformed);
			const conn = resolveConnection();
			expect(conn?.scopes, `malformed scopes ${JSON.stringify(malformed)}`).toEqual([]);
			expect(hasScope(conn, 'admin')).toBe(false);
		}
	});

	it('ignores blank entries in a malformed comma-separated scope value', () => {
		seedConnection();
		// Stray commas / whitespace around real claims are trimmed away —
		// hasScope still resolves the real claims and rejects the rest.
		localStorage.setItem(STORAGE_KEYS.scopes, ' admin , , observer ,');
		const conn = resolveConnection();
		expect(conn?.scopes).toEqual(['admin', 'observer']);
		expect(hasScope(conn, 'admin')).toBe(true);
		expect(hasScope(conn, 'observer')).toBe(true);
		expect(hasScope(conn, '')).toBe(false);
	});
});
