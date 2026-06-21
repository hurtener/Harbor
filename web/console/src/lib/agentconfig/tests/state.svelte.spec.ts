/**
 * AgentConfigPanelState tests (the consolidated 92h consumer).
 *
 * Pins the panel controller's behaviour without a live Runtime:
 *   (a) the four-state PageStatus contract — Disconnected when no Runtime,
 *       loading → ready on a successful fan-out, error on a failed read;
 *   (b) the admin-scope gate — every write no-ops (issues NO Protocol call)
 *       when the connection lacks the `admin` claim (the 92b precedent);
 *   (c) each of the five areas drives its typed-client call + per-area
 *       saving/error transitions;
 *   (d) the active revision seeds the prompt + exposure forms;
 *   (e) the add-connection secret headers are dropped after submit.
 *
 * A fake `ProtocolClient` is injected (CONVENTIONS.md §6); the connection is
 * seeded into `localStorage`. `EventSource` is stubbed so the live
 * `mcp.connection.*` subscription opens without a browser SSE source.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { STORAGE_KEYS } from '$lib/connection.js';
import { AgentConfigPanelState, DEFAULT_AGENT_ID, AGENT_CONFIG_AREAS } from '../state.svelte.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { ProtocolClient } from '$lib/protocol/harbor.js';

class FakeEventSource {
	onopen: ((ev: unknown) => void) | null = null;
	onerror: ((ev: unknown) => void) | null = null;
	onmessage: ((ev: { data: string }) => void) | null = null;
	addEventListener(): void {}
	close(): void {}
}

function seedConnection(scopes = 'admin'): void {
	localStorage.setItem(STORAGE_KEYS.baseURL, 'http://127.0.0.1:18080');
	localStorage.setItem(STORAGE_KEYS.token, 'dummy-token');
	localStorage.setItem(STORAGE_KEYS.tenant, 't1');
	localStorage.setItem(STORAGE_KEYS.user, 'u1');
	localStorage.setItem(STORAGE_KEYS.session, 's1');
	localStorage.setItem(STORAGE_KEYS.scopes, scopes);
}

const ACTIVE_REVISION = {
	revision_id: 'rev-2',
	content_hash: 'h2',
	created_at: '2026-06-18T10:00:00Z',
	payload: {
		prompt_layers: { base: 'You are a base.', user: 'And a user note.' },
		tool_exposure: { paused_servers: ['github'], disabled_tools: ['github_delete'] },
		skills: { names: ['recap'] }
	}
};

function fakeClient(overrides: Record<string, unknown> = {}): ProtocolClient {
	const agentConfig = {
		get: vi.fn(async () => ({ revision: ACTIVE_REVISION, set: true, protocol_version: '0.1.0' })),
		listRevisions: vi.fn(async () => ({
			revisions: [ACTIVE_REVISION, { revision_id: 'rev-1', content_hash: 'h1', created_at: '2026-06-17T10:00:00Z', payload: {} }],
			protocol_version: '0.1.0'
		})),
		skillsList: vi.fn(async () => ({
			skills: [{ name: 'recap', origin: 'generated', scope: 'project', updated_at: '2026-06-18T10:00:00Z' }],
			protocol_version: '0.1.0'
		})),
		diff: vi.fn(async () => ({
			diff: {
				from_revision_id: 'rev-1',
				to_revision_id: 'rev-2',
				skills: { added: ['recap'] },
				tool_exposure: {},
				prompt_layers: { base_changed: true, base_from: '', base_to: 'You are a base.', user_changed: false },
				connections: {}
			},
			protocol_version: '0.1.0'
		})),
		rollback: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		setToolExposure: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		setPromptLayers: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		skillsUpsert: vi.fn(async () => ({ revision: ACTIVE_REVISION, skill: { name: 's' }, protocol_version: '0.1.0' })),
		skillsDelete: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		addMcpConnection: vi.fn(async () => ({
			connection: { name: 'github', transport: 'stdio' },
			state: 'auth_required',
			reason: 'OAuth required',
			protocol_version: '0.1.0'
		})),
		...overrides
	};
	const events = { subscribeURL: vi.fn(() => 'http://127.0.0.1:18080/v1/events') };
	return { agentConfig, events } as unknown as ProtocolClient;
}

/** Typed accessor for the injected agent-config mocks (avoids `any`). */
type Mocks = Record<string, ReturnType<typeof vi.fn>>;
function ac(client: ProtocolClient): Mocks {
	return client.agentConfig as unknown as Mocks;
}

beforeEach(() => {
	vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
	localStorage.clear();
	vi.restoreAllMocks();
	vi.unstubAllGlobals();
});

describe('AGENT_CONFIG_AREAS — the rail descriptor (single-section composition)', () => {
	it('lists the five control-plane areas in display order with rail-matching ids', () => {
		// The panel renders these as a left sub-nav rail (the Settings
		// single-section model); the ids must match the per-section
		// `data-testid`s the e2e spec asserts.
		expect(AGENT_CONFIG_AREAS.map((a) => a.id)).toEqual([
			'revisions',
			'prompt',
			'skills',
			'mcp',
			'add-connection'
		]);
		expect(AGENT_CONFIG_AREAS.map((a) => a.label)).toEqual([
			'Revision history',
			'Layered prompt',
			'Skills',
			'MCP policy',
			'Add connection'
		]);
	});
});

describe('AgentConfigPanelState — four-state contract', () => {
	it('enters the disconnected status when no Runtime is attached', async () => {
		const state = new AgentConfigPanelState();
		await state.load();
		expect(state.status).toBe('disconnected');
		expect(state.disconnected).toBe(true);
	});

	it('loads to ready, seeds the prompt + exposure forms, and defaults the diff selection', async () => {
		seedConnection();
		const state = new AgentConfigPanelState();
		await state.load(fakeClient());
		expect(state.status).toBe('ready');
		expect(state.agentId).toBe(DEFAULT_AGENT_ID);
		expect(state.activeRevisionId).toBe('rev-2');
		expect(state.promptBase).toBe('You are a base.');
		expect(state.promptUser).toBe('And a user note.');
		expect(state.pausedServers).toEqual(['github']);
		expect(state.disabledTools).toEqual(['github_delete']);
		expect(state.skills).toHaveLength(1);
		// Newest-first: to=rev-2, from=rev-1.
		expect(state.toRevision).toBe('rev-2');
		expect(state.fromRevision).toBe('rev-1');
	});

	it('enters the error status when the primary read fails', async () => {
		seedConnection();
		const state = new AgentConfigPanelState();
		await state.load(fakeClient({ get: vi.fn(async () => Promise.reject(new Error('boom'))) }));
		expect(state.status).toBe('error');
		expect(state.error?.message).toContain('boom');
	});

	it('a non-admin client routes to the info branch UP-FRONT, without a doomed read', async () => {
		// The agent-config read surface is admin-scoped (D-235); a non-admin
		// client cannot load it, so the panel shows the not-applicable info
		// state immediately — no scary error / Retry, and no wasted 403 read.
		seedConnection('console:fleet');
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		expect(state.status).toBe('info');
		expect(state.error).toBeNull();
		expect(state.info?.headline).toContain('Admin scope');
		// No read fired — the panel never attempted the admin-only surface.
		expect(ac(client).get).not.toHaveBeenCalled();
	});

	it('a read scope_mismatch (stale admin client scope, non-admin token) still falls back to info', async () => {
		// Defence-in-depth: the client scope claims admin but the token does
		// not, so the read 403s — the catch routes to info, not the error branch.
		seedConnection('admin');
		const state = new AgentConfigPanelState();
		await state.load(
			fakeClient({
				get: vi.fn(async () =>
					Promise.reject(new ProtocolError('scope_mismatch', 'requires admin', 403))
				)
			})
		);
		expect(state.status).toBe('info');
		expect(state.error).toBeNull();
		expect(state.info?.headline).toContain('Admin scope');
	});
});

describe('AgentConfigPanelState — admin-scope gate', () => {
	it('hasAdminScope is false without the admin claim and every write no-ops', async () => {
		seedConnection('viewer');
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		expect(state.hasAdminScope).toBe(false);

		state.confirmRollback = () => true;
		await state.rollback('rev-1');
		await state.savePrompt();
		await state.saveExposure();
		state.skillName = 'x';
		state.skillTrigger = 'y';
		await state.addSkill();
		state.connName = 'z';
		await state.addConnection();

		const mocks = ac(client);
		expect(mocks.rollback).not.toHaveBeenCalled();
		expect(mocks.setPromptLayers).not.toHaveBeenCalled();
		expect(mocks.setToolExposure).not.toHaveBeenCalled();
		expect(mocks.skillsUpsert).not.toHaveBeenCalled();
		expect(mocks.addMcpConnection).not.toHaveBeenCalled();
	});

	it('hasAdminScope is true with the admin claim', async () => {
		seedConnection('admin');
		const state = new AgentConfigPanelState();
		await state.load(fakeClient());
		expect(state.hasAdminScope).toBe(true);
	});
});

describe('AgentConfigPanelState — the five areas drive their client calls', () => {
	it('computeDiff calls diff and lands ready', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.computeDiff();
		expect(ac(client).diff).toHaveBeenCalledWith(DEFAULT_AGENT_ID, 'rev-1', 'rev-2');
		expect(state.diffPhase).toBe('ready');
		expect(state.diff?.skills.added).toEqual(['recap']);
	});

	it('rollback (admin + confirmed) calls rollback and records the target', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.confirmRollback = () => true;
		await state.rollback('rev-1');
		expect(ac(client).rollback).toHaveBeenCalledWith(DEFAULT_AGENT_ID, 'rev-1');
		expect(state.rolledBackTo).toBe('rev-1');
	});

	it('rollback respects the confirmation gate (declined → no call)', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.confirmRollback = () => false;
		await state.rollback('rev-1');
		expect(ac(client).rollback).not.toHaveBeenCalled();
	});

	it('addSkill validates required fields before calling', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.addSkill(); // empty form
		expect(state.skillsError?.code).toBe('invalid_input');
		expect(ac(client).skillsUpsert).not.toHaveBeenCalled();
	});

	it('addSkill (valid) calls skillsUpsert and clears the form', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.skillName = 'recap';
		state.skillTrigger = 'when asked to recap';
		state.skillSteps = 'read\nsummarise';
		await state.addSkill();
		const upsert = ac(client).skillsUpsert;
		expect(upsert).toHaveBeenCalled();
		expect(upsert.mock.calls[0][1].steps).toEqual(['read', 'summarise']);
		expect(state.skillName).toBe('');
	});

	it('addSkill surfaces a pack-overwrite refusal as an inline error', async () => {
		seedConnection();
		const { ProtocolError } = await import('$lib/protocol/errors.js');
		const client = fakeClient({
			skillsUpsert: vi.fn(async () =>
				Promise.reject(new ProtocolError('skill_pack_immutable', 'cannot overwrite a pack skill', 409))
			)
		});
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.skillName = 'recap';
		state.skillTrigger = 't';
		await state.addSkill();
		expect(state.skillsError?.code).toBe('skill_pack_immutable');
	});

	it('saveExposure persists the desired-state sets and lands ready', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.toggleServerPaused('slack');
		await state.saveExposure();
		const setExp = ac(client).setToolExposure;
		expect(setExp).toHaveBeenCalled();
		expect(setExp.mock.calls[0][1].paused_servers).toContain('slack');
		expect(state.exposurePhase).toBe('ready');
		expect(state.exposureSaved).toBe(true);
	});

	it('savePrompt persists the base layer and lands ready', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.promptBase = 'New base.';
		await state.savePrompt();
		expect(ac(client).setPromptLayers).toHaveBeenCalled();
		expect(state.promptPhase).toBe('ready');
		expect(state.promptSaved).toBe(true);
	});

	it('addConnection surfaces the attach state and drops the secret headers', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.connName = 'github';
		state.connCommand = 'npx -y server-github';
		state.connHeaders = 'Authorization: Bearer secret';
		await state.addConnection();
		const add = ac(client).addMcpConnection;
		expect(add).toHaveBeenCalled();
		expect(add.mock.calls[0][2]).toEqual({ Authorization: 'Bearer secret' });
		expect(state.addConnState).toBe('auth_required');
		// The secret header material is write-only — cleared after submit.
		expect(state.connHeaders).toBe('');
	});
});
