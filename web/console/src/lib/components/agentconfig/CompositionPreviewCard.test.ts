/**
 * CompositionPreviewCard component tests (D-414/D-415, HA-66).
 *
 * Mounts the REAL card against a real `AgentConfigPanelState` whose
 * preview data is seeded directly (no live Runtime, no fetch — the
 * card is a pure view over the panel). Asserts:
 *
 *   - boot-only items render READ-ONLY (no remove-shadow affordance,
 *     an explicit "No durable shadow" label);
 *   - revision/both items offer "Remove shadow", which is admin-gated
 *     (disabled-with-tooltip for non-admin);
 *   - the deterministic set hashes + provenance markers render;
 *   - the typed unavailable / conflict / retired outcomes render their
 *     honest messages (never a blank state — D-311);
 *   - an un-wired preview (error phase) renders the typed error + Retry.
 */
import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

import { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';
import type { AgentConfigCompositionPreviewResponse } from '$lib/protocol/agentconfig.js';
import CompositionPreviewCard from './CompositionPreviewCard.svelte';

const PREVIEW: AgentConfigCompositionPreviewResponse = {
	outcome: 'available',
	boot_pack_set_hash: 'bootpackset-hash',
	combined_hash: 'combined-hash',
	revision_hash: 'revision-hash',
	revision_id: 'rev-2',
	content_hash: 'h2',
	items: [
		{
			name: 'boot-pack-skill',
			semantic_hash: 'sema-boot-hash',
			source: 'boot',
			skill: {
				name: 'boot-pack-skill',
				title: 'Boot pack skill',
				origin: 'boot',
				scope: 'operator',
				updated_at: '2026-08-14T00:00:00Z'
			}
		},
		{
			name: 'recap',
			semantic_hash: 'sema-recap-hash',
			source: 'both',
			skill: {
				name: 'recap',
				title: 'Summarise a thread',
				trigger: 'recap the thread',
				origin: 'generated',
				scope: 'project',
				updated_at: '2026-08-14T00:00:00Z'
			}
		}
	],
	widened: false,
	protocol_version: '0.1.0'
};

/** Builds a panel whose preview data + connection are seeded directly. */
function panelWith(preview: AgentConfigCompositionPreviewResponse | null, scopes = ['admin']): AgentConfigPanelState {
	const panel = new AgentConfigPanelState();
	panel.agentId = 'harbor-dev-agent';
	panel.preview = preview;
	panel.previewPhase = preview === null ? 'error' : 'ready';
	panel.connection = {
		baseURL: 'http://127.0.0.1:18080',
		token: 'dummy',
		identity: { tenant: 't1', user: 'u1', session: 's1' },
		scopes
	};
	return panel;
}

let mounted: ReturnType<typeof mount> | undefined;
let target: HTMLElement | undefined;

function render(panel: AgentConfigPanelState): HTMLElement {
	target = document.createElement('div');
	document.body.appendChild(target);
	mounted = mount(CompositionPreviewCard, { target, props: { panel } });
	flushSync();
	return target;
}

afterEach(() => {
	if (mounted) {
		unmount(mounted);
		mounted = undefined;
	}
	if (target) {
		target.remove();
		target = undefined;
	}
});

describe('CompositionPreviewCard — available outcome', () => {
	it('renders the typed outcome, the deterministic set hashes, and the revision identity', () => {
		const panel = panelWith(PREVIEW);
		const el = render(panel);

		expect(el.querySelector('[data-testid="agentcfg-preview-outcome"]')?.textContent).toContain('available');
		const hashes = el.querySelector('[data-testid="agentcfg-preview-hashes"]');
		expect(hashes?.textContent).toContain('bootpackset-hash');
		expect(hashes?.textContent).toContain('combined-hash');
		expect(hashes?.textContent).toContain('revision-hash');
		expect(hashes?.textContent).toContain('rev-2');
		expect(hashes?.textContent).toContain('h2');
	});

	it('renders the exact boot|revision|both provenance per item', () => {
		const panel = panelWith(PREVIEW);
		const el = render(panel);
		const items = el.querySelectorAll('[data-testid="agentcfg-preview-item"]');
		expect(items).toHaveLength(2);
		expect(items[0].textContent).toContain('boot');
		expect(items[1].textContent).toContain('both');
	});

	it('boot-only items render READ-ONLY: no remove affordance, explicit "No durable shadow"', () => {
		const panel = panelWith(PREVIEW);
		const el = render(panel);
		const items = el.querySelectorAll('[data-testid="agentcfg-preview-item"]');
		// boot-only row: read-only marker, NO remove button.
		expect(items[0].querySelector('[data-testid="agentcfg-preview-item-readonly"]')).not.toBeNull();
		expect(items[0].querySelector('[data-testid="agentcfg-preview-remove-shadow"]')).toBeNull();
		// both row (a real durable revision shadow exists): remove offered.
		expect(items[1].querySelector('[data-testid="agentcfg-preview-item-readonly"]')).toBeNull();
		expect(items[1].querySelector('[data-testid="agentcfg-preview-remove-shadow"]')).not.toBeNull();
	});

	it('the remove-shadow button is admin-gated (disabled without the admin claim)', () => {
		const panel = panelWith(PREVIEW, ['viewer']);
		const el = render(panel);
		const button = el.querySelector('[data-testid="agentcfg-preview-remove-shadow"]') as HTMLButtonElement | null;
		expect(button).not.toBeNull();
		expect(button!.disabled).toBe(true);
	});
});

describe('CompositionPreviewCard — typed outcomes (never a blank state)', () => {
	it('unavailable renders the non-oracular note', () => {
		const panel = panelWith({ outcome: 'unavailable', widened: false, protocol_version: '0.1.0' });
		const el = render(panel);
		expect(el.querySelector('[data-testid="agentcfg-preview-unavailable"]')?.textContent).toContain('non-oracular');
	});

	it('conflict renders the offending name + the never-silent-overwrite note', () => {
		const panel = panelWith({ outcome: 'conflict', conflict_name: 'recap', widened: false, protocol_version: '0.1.0' });
		const el = render(panel);
		const note = el.querySelector('[data-testid="agentcfg-preview-conflict"]');
		expect(note?.textContent).toContain('recap');
		expect(note?.textContent).toContain('never a silent overwrite');
	});

	it('retired renders the tombstone note', () => {
		const panel = panelWith({ outcome: 'retired', widened: false, protocol_version: '0.1.0' });
		const el = render(panel);
		expect(el.querySelector('[data-testid="agentcfg-preview-retired"]')?.textContent).toContain('tombstone');
	});
});

describe('CompositionPreviewCard — un-wired preview', () => {
	it('renders the typed error + Retry (never a blank state)', () => {
		const panel = panelWith(null);
		panel.previewError = { code: 'unknown_method', message: 'composition preview is not wired on this runtime' };
		const el = render(panel);
		expect(el.querySelector('[data-testid="agentcfg-preview-error"]')?.textContent).toContain('unknown_method');
		expect(el.querySelector('[data-testid="agentcfg-preview-retry"]')).not.toBeNull();
	});
});
