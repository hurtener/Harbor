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
		expect(hasScope(conn, 'admin')).toBe(true);
		expect(hasScope(conn, 'observer')).toBe(false);
	});

	it('is false (never throws) for a null connection', () => {
		expect(hasScope(null, 'admin')).toBe(false);
	});
});
