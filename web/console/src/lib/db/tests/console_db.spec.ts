/**
 * Console DB list-page bootstrap tests (D-121).
 *
 * `openListPageDB` is the single seam every Console list page uses to
 * open the Console DB for its `SavedViewChips` (D-061) without an
 * interactive auth passphrase. These tests pin: a real (faked) IDB store
 * opens, the saved-view tables round-trip, and the same operator
 * identity yields a stable store across reopens (deterministic salt) so
 * saved filters survive a page reload.
 */
import { describe, expect, it } from 'vitest';
import { openListPageDB } from '../console_db.js';
import { ToolsSavedFilters } from '../saved_filters_tools.js';
import { operatorIdOf } from '../schema.js';
import type { RuntimeConnection } from '../../connection.js';

function connectionFor(tenant: string, user: string): RuntimeConnection {
	return {
		baseURL: 'http://127.0.0.1:18080',
		token: 'dummy-bearer-token',
		identity: { tenant, user, session: 'session-x' }
	};
}

describe('openListPageDB', () => {
	it('opens a Console DB the Tools saved-filter store can write to', async () => {
		const db = await openListPageDB(connectionFor('tenant-a', 'user-a'));
		const op = await operatorIdOf('tenant-a', 'user-a');
		const store = new ToolsSavedFilters(db, op);

		const created = await store.create('MCP failures', {
			transports: ['MCP'],
			search: 'fail'
		});
		expect(created.name).toBe('MCP failures');

		const listed = await store.list();
		expect(listed).toHaveLength(1);
		expect(listed[0].id).toBe(created.id);
	});

	it('yields a stable store across reopens for the same operator', async () => {
		const conn = connectionFor('tenant-b', 'user-b');
		const op = await operatorIdOf('tenant-b', 'user-b');

		const db1 = await openListPageDB(conn);
		await new ToolsSavedFilters(db1, op).create('HTTP tools', {
			transports: ['HTTP']
		});

		// A fresh open with the SAME identity must see the persisted row —
		// the deterministic salt keeps the IndexedDB store stable across a
		// page reload (a random salt would yield an empty store).
		const db2 = await openListPageDB(conn);
		const reread = await new ToolsSavedFilters(db2, op).list();
		expect(reread).toHaveLength(1);
		expect(reread[0].name).toBe('HTTP tools');
	});
});
