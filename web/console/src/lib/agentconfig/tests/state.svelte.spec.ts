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
import {
	AgentConfigPanelState,
	DEFAULT_AGENT_ID,
	AGENT_CONFIG_AREAS,
	changedSectionLabels,
	shortRevision
} from '../state.svelte.js';
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
				connections: {},
				llm_params: { model_changed: false, temperature_changed: false, max_tokens_changed: false, reasoning_effort_changed: false }
			},
			protocol_version: '0.1.0'
		})),
		rollback: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		setRevision: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		setToolExposure: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		setPromptLayers: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		setLlmParams: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		skillsUpsert: vi.fn(async () => ({ revision: ACTIVE_REVISION, skill: { name: 's' }, protocol_version: '0.1.0' })),
		skillsDelete: vi.fn(async () => ({ revision: ACTIVE_REVISION, protocol_version: '0.1.0' })),
		addMcpConnection: vi.fn(async () => ({
			connection: { name: 'github', transport: 'stdio' },
			state: 'auth_required',
			reason: 'OAuth required',
			protocol_version: '0.1.0'
		})),
		setMcpDiscoveryOrigins: vi.fn(async () => ({
			revision: ACTIVE_REVISION,
			name: 'srv',
			granted: ['https://as.example.net'],
			revoked: [],
			applied_live: true,
			protocol_version: '0.1.0'
		})),
		removeMcpConnection: vi.fn(async () => ({
			revision: ACTIVE_REVISION,
			name: 'srv',
			protocol_version: '0.1.0'
		})),
		setOAuthProvider: vi.fn(async () => ({
			revision: ACTIVE_REVISION,
			name: 'm365',
			protocol_version: '0.1.0'
		})),
		removeOAuthProvider: vi.fn(async () => ({
			revision: ACTIVE_REVISION,
			name: 'm365',
			uninstalled: true,
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
	it('lists the six control-plane areas in display order with rail-matching ids', () => {
		// The panel renders these as a left sub-nav rail (the Settings
		// single-section model); the ids must match the per-section
		// `data-testid`s the e2e spec asserts. 92i adds "Model & sampling".
		expect(AGENT_CONFIG_AREAS.map((a) => a.id)).toEqual([
			'revisions',
			'prompt',
			'llm',
			'skills',
			'mcp',
			'add-connection'
		]);
		expect(AGENT_CONFIG_AREAS.map((a) => a.label)).toEqual([
			'Revision history',
			'Layered prompt',
			'Model & sampling',
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

	it('degrades gracefully when skills control is not wired (panel still ready)', async () => {
		// §17.6 — a live runtime without a SkillStore returns unknown_method on
		// skills.list. The core reads (active revision + history) gate the panel;
		// a skills-list failure must NOT sink the whole panel (the revisions /
		// prompt / model-&-sampling / MCP areas don't need skills).
		seedConnection();
		const state = new AgentConfigPanelState();
		await state.load(
			fakeClient({
				skillsList: vi.fn(async () =>
					Promise.reject(new ProtocolError('unknown_method', 'skills control not wired', 501))
				)
			})
		);
		expect(state.status).toBe('ready'); // panel is usable
		expect(state.revisions.length).toBeGreaterThan(0); // core read succeeded
		expect(state.skills).toEqual([]); // skills area degraded
		expect(state.skillsPhase).toBe('error');
		expect(state.skillsError?.code).toBe('unknown_method');
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

		await state.requestRollback('rev-1');
		await state.confirmRollbackPreview();
		await state.savePrompt();
		await state.saveExposure();
		state.llmModel = 'model-x';
		await state.saveLlmParams();
		state.promptDirty = true;
		await state.saveAll();
		state.skillName = 'x';
		state.skillTrigger = 'y';
		await state.addSkill();
		state.connName = 'z';
		await state.addConnection();

		const mocks = ac(client);
		expect(mocks.diff).not.toHaveBeenCalled(); // requestRollback no-ops for non-admin
		expect(mocks.rollback).not.toHaveBeenCalled();
		expect(mocks.setPromptLayers).not.toHaveBeenCalled();
		expect(mocks.setToolExposure).not.toHaveBeenCalled();
		expect(mocks.setLlmParams).not.toHaveBeenCalled();
		expect(mocks.setRevision).not.toHaveBeenCalled();
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

describe('AgentConfigPanelState — Protocol-installed OAuth providers (169 / D-303)', () => {
	it('addConnection threads oauth_provider into the descriptor (fixes the silent drop)', async () => {
		seedConnection('admin');
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.connName = 'graph';
		state.connTransport = 'http';
		state.connUrl = 'https://graph.microsoft.com/mcp';
		state.connOAuthProvider = 'm365';
		await state.addConnection();
		const call = ac(client).addMcpConnection.mock.calls[0];
		// call = [agentId, descriptor, headers]
		expect(call[1].oauth_provider).toBe('m365');
	});

	it('addConnection omits oauth_provider when none is selected', async () => {
		seedConnection('admin');
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.connName = 'graph';
		state.connTransport = 'http';
		state.connUrl = 'https://graph.microsoft.com/mcp';
		await state.addConnection();
		const call = ac(client).addMcpConnection.mock.calls[0];
		expect(call[1].oauth_provider).toBeUndefined();
	});

	it('installProvider sends the ZERO-URL descriptor (driver/source pinned)', async () => {
		seedConnection('admin');
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.provName = 'm365';
		state.provBroker = 'm365-broker';
		state.provScopes = 'mail.read  mail.send';
		await state.installProvider();
		const call = ac(client).setOAuthProvider.mock.calls[0];
		// call = [agentId, descriptor]
		expect(call[1]).toEqual({
			name: 'm365',
			driver: 'tokenexchange',
			credential_source: 'remote',
			credential_broker: 'm365-broker',
			scopes: ['mail.read', 'mail.send']
		});
		// No URL / env / secret field ever leaves the client.
		expect(Object.keys(call[1])).not.toContain('token_url');
		expect(Object.keys(call[1])).not.toContain('client_secret_env');
	});

	it('removeProvider calls remove_oauth_provider', async () => {
		seedConnection('admin');
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.removeProvider('m365');
		expect(ac(client).removeOAuthProvider).toHaveBeenCalledWith(DEFAULT_AGENT_ID, 'm365');
	});

	it('non-admin install/remove no-op', async () => {
		seedConnection('viewer');
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.provName = 'm365';
		state.provBroker = 'b';
		await state.installProvider();
		await state.removeProvider('m365');
		expect(ac(client).setOAuthProvider).not.toHaveBeenCalled();
		expect(ac(client).removeOAuthProvider).not.toHaveBeenCalled();
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

	it('requestRollback fetches the active→target diff and does NOT roll back yet', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client); // active = rev-2
		await state.requestRollback('rev-1');
		// The preview diffs active (rev-2) → target (rev-1); rollback is NOT fired.
		expect(ac(client).diff).toHaveBeenCalledWith(DEFAULT_AGENT_ID, 'rev-2', 'rev-1');
		expect(state.rollbackTarget).toBe('rev-1');
		expect(state.rollbackPreviewDiff).not.toBeNull();
		expect(state.rollbackPreviewPhase).toBe('ready');
		expect(ac(client).rollback).not.toHaveBeenCalled();
	});

	it('confirmRollbackPreview repoints only after the preview is confirmed', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.requestRollback('rev-1');
		await state.confirmRollbackPreview();
		expect(ac(client).rollback).toHaveBeenCalledWith(DEFAULT_AGENT_ID, 'rev-1');
		expect(state.rolledBackTo).toBe('rev-1');
		expect(state.rollbackTarget).toBeNull(); // preview closed
	});

	it('cancelRollbackPreview is a no-op (never repoints)', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.requestRollback('rev-1');
		state.cancelRollbackPreview();
		expect(state.rollbackTarget).toBeNull();
		expect(ac(client).rollback).not.toHaveBeenCalled();
	});

	it('requestRollback is a no-op for the already-active revision', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client); // active = rev-2
		await state.requestRollback('rev-2');
		expect(ac(client).diff).not.toHaveBeenCalled();
		expect(state.rollbackTarget).toBeNull();
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

describe('changedSectionLabels — the derived per-revision summary mapping (92i)', () => {
	it('maps each changed section to its display label, in display order', () => {
		const from = { prompt_layers: { base: 'a' }, skills: { names: ['x'] } };
		const to = {
			prompt_layers: { base: 'b' }, // changed
			skills: { names: ['x'] }, // unchanged
			llm_params: { model: 'm' } // added
		};
		expect(changedSectionLabels(from, to)).toEqual(['Prompt', 'Model & sampling']);
	});

	it('treats a present-vs-absent section as a change', () => {
		expect(changedSectionLabels({}, { tool_exposure: { paused_servers: ['s'] } })).toEqual([
			'MCP policy'
		]);
		expect(changedSectionLabels({ connections: { servers: [] } }, {})).toEqual(['Connections']);
	});

	it('returns no labels for identical payloads', () => {
		const p = { prompt_layers: { base: 'a' }, llm_params: { temperature: 0.5 } };
		expect(changedSectionLabels(p, p)).toEqual([]);
		expect(changedSectionLabels(undefined, undefined)).toEqual([]);
	});
});

describe('AgentConfigPanelState.revisionSummary — derived, client-side (92i)', () => {
	function withRevisions(revs: unknown[]): AgentConfigPanelState {
		const s = new AgentConfigPanelState();
		// revisions is public reactive state; seed it directly (no client).
		(s as unknown as { revisions: unknown[] }).revisions = revs;
		return s;
	}

	it('labels the first revision (no parent) "Initial configuration"', () => {
		const rev = { revision_id: 'r1', content_hash: 'h1', created_at: '', payload: {} };
		const s = withRevisions([rev]);
		expect(s.revisionSummary(rev as never)).toBe('Initial configuration');
	});

	it('lists the changed sections vs the parent', () => {
		const parent = { revision_id: 'r1', content_hash: 'h1', created_at: '', payload: { prompt_layers: { base: 'a' } } };
		const child = {
			revision_id: 'r2',
			parent_revision_id: 'r1',
			content_hash: 'h2',
			created_at: '',
			payload: { prompt_layers: { base: 'a' }, llm_params: { model: 'm' } }
		};
		const s = withRevisions([child, parent]); // newest-first
		expect(s.revisionSummary(child as never)).toBe('Model & sampling');
	});

	it('recognises a revert-by-re-set (content_hash matches an older revision)', () => {
		const r1 = { revision_id: 'rev-original', content_hash: 'hA', created_at: '', payload: { prompt_layers: { base: 'a' } } };
		const r2 = { revision_id: 'r2', parent_revision_id: 'rev-original', content_hash: 'hB', created_at: '', payload: { prompt_layers: { base: 'b' } } };
		const r3 = { revision_id: 'r3', parent_revision_id: 'r2', content_hash: 'hA', created_at: '', payload: { prompt_layers: { base: 'a' } } };
		const s = withRevisions([r3, r2, r1]); // newest-first
		expect(s.revisionSummary(r3 as never)).toBe(`Rolled back to ${shortRevision('rev-original')}`);
	});
});

describe('AgentConfigPanelState — atomic "Save all" (92i)', () => {
	it('commits staged edits across areas as ONE set_revision (full merged payload)', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client); // active = rev-2 (prompt + exposure + skills)
		expect(state.stagedAreaCount).toBe(0);

		// Edit prompt + model & sampling (two areas).
		state.promptBase = 'A new base.';
		state.markPromptEdited();
		state.llmModel = 'model-x';
		state.llmTemperature = 0.4;
		state.markLlmEdited();
		expect(state.stagedAreaCount).toBe(2);
		expect(state.hasStagedChanges).toBe(true);

		await state.saveAll();

		const mocks = ac(client);
		// Exactly ONE write — set_revision — not the per-section verbs.
		expect(mocks.setRevision).toHaveBeenCalledTimes(1);
		expect(mocks.setPromptLayers).not.toHaveBeenCalled();
		expect(mocks.setLlmParams).not.toHaveBeenCalled();
		const payload = mocks.setRevision.mock.calls[0][1];
		expect(payload.prompt_layers.base).toBe('A new base.');
		expect(payload.llm_params).toEqual({ model: 'model-x', temperature: 0.4 });
		// Non-form sections carried forward from the active revision (whole-
		// envelope replace preserves them).
		expect(payload.skills).toEqual({ names: ['recap'] });
		// The exposure section (seeded, unedited) is reproduced from the form.
		expect(payload.tool_exposure).toEqual({
			paused_servers: ['github'],
			disabled_tools: ['github_delete']
		});
	});

	it('saveAll no-ops when nothing is staged', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.saveAll();
		expect(ac(client).setRevision).not.toHaveBeenCalled();
	});

	it('a prior per-area write advances activeRevisionId so Save-all carries the FRESH sections (no data loss)', async () => {
		// Regression for the stale-activeRevisionId data-loss bug: after a
		// discrete write (e.g. add-connection) the registry advances the active
		// pointer to the NEW revision (which carries the just-added section).
		// A later Save-all must base its whole-envelope merge on THAT revision,
		// not the pre-write one — else the connection/skills are dropped.
		seedConnection();
		// rev-3 is the post-add active revision: it carries a NEW connection +
		// the skills, and is newest in the list (newest-first).
		const rev3 = {
			revision_id: 'rev-3',
			parent_revision_id: 'rev-2',
			content_hash: 'h3',
			created_at: '2026-06-18T11:00:00Z',
			payload: {
				skills: { names: ['recap'] },
				connections: { servers: [{ name: 'github', transport: 'stdio' }] }
			}
		};
		const client = fakeClient({
			// After the per-area save, the history (newest-first) leads with rev-3.
			listRevisions: vi.fn(async () => ({
				revisions: [rev3, ACTIVE_REVISION, { revision_id: 'rev-1', content_hash: 'h1', created_at: '2026-06-17T10:00:00Z', payload: {} }],
				protocol_version: '0.1.0'
			}))
		});
		const state = new AgentConfigPanelState();
		await state.load(client); // get → ACTIVE_REVISION (rev-2)
		expect(state.activeRevisionId).toBe('rev-2');

		// A per-area save records rev-3 → reloadRevisions must advance the active id.
		await state.savePrompt();
		expect(state.activeRevisionId).toBe('rev-3');

		// Now stage a prompt edit + Save-all: the merged payload must carry the
		// connection (from rev-3), not drop it.
		state.promptBase = 'edited again';
		state.markPromptEdited();
		await state.saveAll();
		const payload = ac(client).setRevision.mock.calls[0][1];
		expect(payload.connections).toEqual({ servers: [{ name: 'github', transport: 'stdio' }] });
		expect(payload.skills).toEqual({ names: ['recap'] });
	});

	it('discardStaged drops staged edits and re-seeds the forms (no write)', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.promptBase = 'dirty';
		state.markPromptEdited();
		expect(state.hasStagedChanges).toBe(true);
		state.discardStaged();
		expect(state.hasStagedChanges).toBe(false);
		expect(state.promptBase).toBe('You are a base.'); // re-seeded from active
		expect(ac(client).setRevision).not.toHaveBeenCalled();
	});
});

describe('AgentConfigPanelState — model & sampling (92j/92i)', () => {
	it('saveLlmParams sends the parsed params and clears the dirty flag', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.llmModel = 'model-x';
		state.llmTemperature = 0.7;
		state.llmMaxTokens = 2048;
		state.llmReasoningEffort = 'high';
		state.markLlmEdited();
		await state.saveLlmParams();
		expect(ac(client).setLlmParams).toHaveBeenCalledWith(DEFAULT_AGENT_ID, {
			model: 'model-x',
			reasoning_effort: 'high',
			temperature: 0.7,
			max_tokens: 2048
		});
		expect(state.llmSaved).toBe(true);
		expect(state.llmDirty).toBe(false);
	});

	it('saveLlmParams omits the unset (null) numeric fields — inherit the next layer', async () => {
		seedConnection();
		const client = fakeClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		// Only a model + reasoning effort; temperature / max-tokens left null
		// (the number inputs are empty → inherit the tenant baseline).
		state.llmModel = 'model-x';
		state.llmReasoningEffort = 'low';
		state.llmTemperature = null;
		state.llmMaxTokens = null;
		await state.saveLlmParams();
		expect(ac(client).setLlmParams).toHaveBeenCalledWith(DEFAULT_AGENT_ID, {
			model: 'model-x',
			reasoning_effort: 'low'
		});
	});
});

describe('AgentConfigPanelState — failed-write paths keep state (§13 no silent drop, 92i)', () => {
	it('saveAll: a rejected set_revision keeps the staged edits + surfaces the error (no reload)', async () => {
		seedConnection();
		const client = fakeClient({
			setRevision: vi.fn(async () =>
				Promise.reject(new ProtocolError('runtime_error', 'boom', 500))
			)
		});
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.promptBase = 'edited';
		state.markPromptEdited();
		expect(state.hasStagedChanges).toBe(true);

		await state.saveAll();

		expect(state.saveAllPhase).toBe('error');
		expect(state.saveAllError?.message).toContain('boom');
		// The staged edits are PRESERVED (a failed write must not silently
		// discard them, and must not reload/re-seed the form).
		expect(state.hasStagedChanges).toBe(true);
		expect(state.promptDirty).toBe(true);
		expect(state.promptBase).toBe('edited');
	});

	it('confirmRollbackPreview: a rejected rollback sets the error and leaves the preview open', async () => {
		seedConnection();
		const client = fakeClient({
			rollback: vi.fn(async () => Promise.reject(new ProtocolError('runtime_error', 'nope', 500)))
		});
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.requestRollback('rev-1');
		await state.confirmRollbackPreview();
		expect(state.rollbackError?.message).toContain('nope');
		// The preview stays open so the operator can retry / cancel.
		expect(state.rollbackTarget).toBe('rev-1');
		expect(state.rolledBackTo).toBeNull();
	});

	it('requestRollback: a rejected diff lands the preview in the error state, never rolling back', async () => {
		seedConnection();
		const client = fakeClient({
			diff: vi.fn(async () => Promise.reject(new ProtocolError('runtime_error', 'diff-fail', 500)))
		});
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.requestRollback('rev-1');
		expect(state.rollbackPreviewPhase).toBe('error');
		expect(state.rollbackPreviewError?.message).toContain('diff-fail');
		expect(ac(client).rollback).not.toHaveBeenCalled();
	});
});

describe('AgentConfigPanelState — the per-connection discovery-origins editor (D-302)', () => {
	const REV_WITH_CONN = {
		revision_id: 'rev-c',
		content_hash: 'hc',
		created_at: '2026-07-10T10:00:00Z',
		payload: {
			connections: {
				servers: [
					{
						name: 'srv',
						transport: 'http',
						url: 'https://mcp.example.com/rpc',
						oauth_discovery_allowed_origins: ['https://a.example.net']
					}
				]
			}
		}
	};

	function connClient(overrides: Record<string, unknown> = {}): ProtocolClient {
		return fakeClient({
			get: vi.fn(async () => ({ revision: REV_WITH_CONN, set: true, protocol_version: '0.1.0' })),
			listRevisions: vi.fn(async () => ({ revisions: [REV_WITH_CONN], protocol_version: '0.1.0' })),
			...overrides
		});
	}

	it('exposes the active connections + the current allow-list as the input default', async () => {
		seedConnection();
		const state = new AgentConfigPanelState();
		await state.load(connClient());
		expect(state.activeConnections.map((c) => c.name)).toEqual(['srv']);
		expect(state.discoveryInputFor(state.activeConnections[0])).toBe('https://a.example.net');
	});

	it('setDiscoveryOrigins parses the draft, FULL-REPLACE writes, and reloads', async () => {
		seedConnection();
		const client = connClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		// The operator edits the draft (newline + comma separated, deduped).
		state.discoveryDraft['srv'] = 'https://a.example.net\nhttps://b.example.net, https://b.example.net';
		await state.setDiscoveryOrigins('srv');
		expect(ac(client).setMcpDiscoveryOrigins).toHaveBeenCalledWith(DEFAULT_AGENT_ID, 'srv', [
			'https://a.example.net',
			'https://b.example.net'
		]);
		// A confirmed write clears the draft + reloads (get called again).
		expect(state.discoveryDraft['srv']).toBeUndefined();
		expect(ac(client).listRevisions).toHaveBeenCalledTimes(2);
	});

	it('removeConnection calls remove_mcp_connection (its first Console caller) + reloads', async () => {
		seedConnection();
		const client = connClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.removeConnection('srv');
		expect(ac(client).removeMcpConnection).toHaveBeenCalledWith(DEFAULT_AGENT_ID, 'srv');
		expect(ac(client).listRevisions).toHaveBeenCalledTimes(2);
	});

	it('the editor writes are admin-gated (no Protocol call without the admin claim)', async () => {
		seedConnection('read'); // non-admin
		const client = connClient();
		const state = new AgentConfigPanelState();
		await state.load(client);
		await state.setDiscoveryOrigins('srv');
		await state.removeConnection('srv');
		expect(ac(client).setMcpDiscoveryOrigins).not.toHaveBeenCalled();
		expect(ac(client).removeMcpConnection).not.toHaveBeenCalled();
	});

	it('surfaces a write failure without reloading (no silent drop)', async () => {
		seedConnection();
		const client = connClient({
			setMcpDiscoveryOrigins: vi.fn(async () =>
				Promise.reject(new ProtocolError('invalid_request', 'bad origin', 400))
			)
		});
		const state = new AgentConfigPanelState();
		await state.load(client);
		state.discoveryDraft['srv'] = 'http://not-https';
		await state.setDiscoveryOrigins('srv');
		expect(state.discoveryError?.code).toBe('invalid_request');
		expect(ac(client).listRevisions).toHaveBeenCalledTimes(1); // no reload on failure
	});
});
