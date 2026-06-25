/**
 * AppStatusBar connect-time wire-drift surfacing guard.
 *
 * The global app status bar fetches `runtime.info` at attach and compares the
 * live runtime's `wire_surface_digest` against the digest this Console build
 * vendored from `wire-manifest.gen.json` (via the pure `compareWireDigest`
 * seam). This spec mounts the REAL component and asserts:
 *
 *   - a planted MISMATCH digest surfaces the loud, role="alert" drift signal;
 *   - an ABSENT digest surfaces the informational "predates digest support"
 *     note, NOT a drift alarm;
 *   - a MATCHING digest surfaces neither.
 *
 * `compareWireDigest` is kept REAL (only `resolveConnection` is overridden);
 * the runtime client + Console DB are stubbed so the test exercises the real
 * comparison + surfacing path.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import wireManifest from '$lib/protocol/wire-manifest.gen.json';

// The exact digest this build vendored — what a healthy runtime reports back.
const VENDORED_DIGEST = (wireManifest as { wire_surface_digest: string }).wire_surface_digest;

// A fixed fake connection so the bar treats the Console as attached.
const FAKE_CONN = {
	baseURL: 'http://127.0.0.1:18080',
	token: 'header.payload.sig',
	identity: { tenant: 'dev', user: 'dev', session: 'dev' },
	scopes: [] as string[]
};

// The digest the stubbed runtime.info reports — set per-test before mount.
const live = vi.hoisted(() => ({ digest: undefined as string | undefined }));

// Keep compareWireDigest REAL; only override resolveConnection.
vi.mock('$lib/connection.js', async (importActual) => {
	const actual = await importActual<typeof import('$lib/connection.js')>();
	return { ...actual, resolveConnection: () => FAKE_CONN };
});

// Stub the runtime client: posture.info() resolves the planted digest.
vi.mock('$lib/protocol/harbor.js', () => ({
	HarborClient: class {
		posture = {
			info: async () => ({
				protocol_version: '0.1.0',
				capabilities: ['events_subscribe'],
				...(live.digest === undefined ? {} : { wire_surface_digest: live.digest })
			})
		};
	}
}));

// The runtime-name lookup is irrelevant here; make it fail so the bar falls
// back to the base URL (the catch path the component already carries).
vi.mock('$lib/db/console_db.js', () => ({
	openListPageDB: async () => {
		throw new Error('no db in test');
	}
}));
vi.mock('$lib/db/schema.js', () => ({
	operatorIdOf: async () => 'op-test'
}));

import AppStatusBar from './AppStatusBar.svelte';

let mounted: ReturnType<typeof mount> | undefined;
let target: HTMLElement | undefined;

function render(): HTMLElement {
	target = document.createElement('div');
	document.body.appendChild(target);
	mounted = mount(AppStatusBar, { target, props: {} });
	flushSync();
	return target;
}

afterEach(() => {
	if (mounted) {
		unmount(mounted);
		mounted = undefined;
	}
	target?.remove();
	target = undefined;
	live.digest = undefined;
	vi.restoreAllMocks();
});

describe('AppStatusBar — connect-time wire-surface drift', () => {
	it('surfaces the loud drift signal on a planted digest mismatch', async () => {
		// A well-formed digest that cannot match the vendored manifest's.
		live.digest = 'sha256:' + '0123456789abcdef'.repeat(4);
		const root = render();

		const drift = await vi.waitFor(() => {
			const el = root.querySelector("[data-testid='wire-drift']");
			if (!el) throw new Error('drift signal not yet surfaced');
			return el;
		});
		expect(drift).not.toBeNull();
		expect(drift.getAttribute('role')).toBe('alert');
		expect(drift.getAttribute('data-wire-state')).toBe('drift');
		// The informational note must NOT be shown on a drift.
		expect(root.querySelector("[data-testid='wire-unsupported']")).toBeNull();
	});

	it('surfaces the informational note (not a drift alarm) when the runtime reports no digest', async () => {
		live.digest = undefined; // runtime predates digest support
		const root = render();

		const note = await vi.waitFor(() => {
			const el = root.querySelector("[data-testid='wire-unsupported']");
			if (!el) throw new Error('unsupported note not yet surfaced');
			return el;
		});
		expect(note).not.toBeNull();
		expect(note.getAttribute('data-wire-state')).toBe('unsupported');
		// It is NOT a drift alarm.
		expect(root.querySelector("[data-testid='wire-drift']")).toBeNull();
	});

	it('surfaces neither signal when the live digest matches the vendored manifest', async () => {
		live.digest = VENDORED_DIGEST; // a healthy runtime on this build's surface
		const root = render();

		// Wait until the resolved wire state lands on 'match' (proves the
		// fetch + compare ran), THEN assert neither the drift alert nor the
		// unsupported note is rendered — a match is silent.
		const bar = await vi.waitFor(() => {
			const el = root.querySelector("[data-testid='app-status-bar']");
			if (el?.getAttribute('data-wire-state') !== 'match') {
				throw new Error('match state not yet resolved');
			}
			return el;
		});
		expect(bar.getAttribute('data-wire-state')).toBe('match');
		expect(root.querySelector("[data-testid='wire-drift']")).toBeNull();
		expect(root.querySelector("[data-testid='wire-unsupported']")).toBeNull();
	});
});
