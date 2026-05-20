/**
 * Memory-page saved-view wiring tests (D-121, CONVENTIONS.md §3/§5).
 *
 * `openMemorySavedFilters` is the bridge the Memory page calls to get an
 * IndexedDB-backed `MemorySavedFilters` facade (D-061 — saved views are
 * Console-local). The suite runs against the `fake-indexeddb` polyfill
 * (installed globally by `src/lib/db/tests/setup.ts`) + jsdom's
 * `crypto.subtle`, so the facade is exercised against a real (faked) IDB
 * engine — not a mock (CLAUDE.md §17.3).
 */
import { describe, expect, it } from 'vitest';
import { openMemorySavedFilters } from '../saved_views.js';
import type { RuntimeConnection } from '../../connection.js';

const CONNECTION: RuntimeConnection = {
	baseURL: 'http://runtime.test',
	token: 'tok-abc',
	identity: { tenant: 't-mem', user: 'u-mem', session: 's-mem' },
	scopes: []
};

describe('openMemorySavedFilters', () => {
	it('opens an IndexedDB-backed MemorySavedFilters facade', async () => {
		const facade = await openMemorySavedFilters(CONNECTION);
		expect(facade, 'a facade is returned when WebCrypto + IndexedDB exist').not.toBeNull();
	});

	it('round-trips a saved view through the Console DB', async () => {
		const facade = await openMemorySavedFilters(CONNECTION);
		expect(facade).not.toBeNull();
		if (facade === null) return;

		await facade.put({
			id: 'mem-view-test-1',
			name: 'By session',
			filter: { scopes: ['session'] }
		});
		const rows = await facade.list();
		const saved = rows.find((r) => r.id === 'mem-view-test-1');
		expect(saved, 'the saved view persists').toBeDefined();
		expect(saved?.name).toBe('By session');
		expect(saved?.filter.scopes).toEqual(['session']);

		await facade.delete('mem-view-test-1');
		const got = await facade.get('mem-view-test-1');
		expect(got, 'a deleted view is gone').toBeNull();
	});
});
