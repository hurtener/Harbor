/**
 * Agent-config control-panel Protocol-client tests (the consolidated 92h
 * consumer).
 *
 * The `AgentConfigNamespace` is exercised with an injected `fetchImpl`
 * stub so it is verified without a live Runtime: route dispatch, the
 * request body (agent id + the area payload), and the uniform
 * `(code, message, status)` error mapping onto the single `ProtocolError`.
 * The panel is a pure CONSUMER of the `agent_config.*` surface shipped in
 * 92a–f; these pin each of the five areas' calls.
 */
import { describe, expect, it, vi } from 'vitest';
import { HarborClient, ProtocolError } from '../harbor.js';
import type { RuntimeConnection } from '../../connection.js';

const CONNECTION: RuntimeConnection = {
	baseURL: 'http://127.0.0.1:18080',
	token: 'dummy-bearer-token',
	identity: { tenant: 't1', user: 'u1', session: 's1' },
	scopes: ['admin']
};

function okResponse(body: unknown): Response {
	return new Response(JSON.stringify(body), {
		status: 200,
		headers: { 'Content-Type': 'application/json' }
	});
}

function call(fetchImpl: ReturnType<typeof vi.fn>): [string, RequestInit] {
	return fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
}

describe('AgentConfig revision history + diff + rollback (92a)', () => {
	it('routes list_revisions with the agent id', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ revisions: [], protocol_version: '0.1.0' }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.agentConfig.listRevisions('harbor-dev-agent');
		const [url, init] = call(fetchImpl);
		expect(url).toBe('http://127.0.0.1:18080/v1/agent_config/list_revisions');
		expect(init.method).toBe('POST');
		const body = JSON.parse(init.body as string);
		expect(body.agent_id).toBe('harbor-dev-agent');
		expect(body.identity).toEqual(CONNECTION.identity);
	});

	it('routes diff with the from/to revisions', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({ diff: { from_revision_id: 'a', to_revision_id: 'b' }, protocol_version: '0.1.0' })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.agentConfig.diff('a1', 'rev-a', 'rev-b');
		const [url, init] = call(fetchImpl);
		expect(url).toBe('http://127.0.0.1:18080/v1/agent_config/diff');
		const body = JSON.parse(init.body as string);
		expect(body.from_revision).toBe('rev-a');
		expect(body.to_revision).toBe('rev-b');
	});

	it('routes rollback with the revision id', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({ revision: { revision_id: 'rev-a' }, protocol_version: '0.1.0' })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.agentConfig.rollback('a1', 'rev-a');
		const [url, init] = call(fetchImpl);
		expect(url).toBe('http://127.0.0.1:18080/v1/agent_config/rollback');
		const body = JSON.parse(init.body as string);
		expect(body.revision_id).toBe('rev-a');
	});
});

describe('AgentConfig skills (92c)', () => {
	it('routes skills.list to the skills sub-path', async () => {
		const fetchImpl = vi.fn(async () => okResponse({ skills: [], protocol_version: '0.1.0' }));
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.agentConfig.skillsList('a1');
		const [url] = call(fetchImpl);
		expect(url).toBe('http://127.0.0.1:18080/v1/agent_config/skills/list');
	});

	it('routes skills.upsert with the skill payload', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({ revision: { revision_id: 'r' }, skill: { name: 's' }, protocol_version: '0.1.0' })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.agentConfig.skillsUpsert('a1', {
			name: 'recap',
			trigger: 'when asked to recap',
			steps: ['read', 'summarise'],
			origin: 'generated',
			scope: 'project'
		});
		const [url, init] = call(fetchImpl);
		expect(url).toBe('http://127.0.0.1:18080/v1/agent_config/skills/upsert');
		const body = JSON.parse(init.body as string);
		expect(body.skill.name).toBe('recap');
		expect(body.skill.steps).toEqual(['read', 'summarise']);
	});

	it('surfaces a pack-overwrite refusal as a ProtocolError (not a silent failure)', async () => {
		const fetchImpl = vi.fn(
			async () =>
				new Response(
					JSON.stringify({ code: 'skill_pack_immutable', message: 'cannot overwrite a pack skill' }),
					{ status: 409, headers: { 'Content-Type': 'application/json' } }
				)
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await expect(
			client.agentConfig.skillsUpsert('a1', {
				name: 'recap',
				trigger: 't',
				steps: [],
				origin: 'generated',
				scope: 'project'
			})
		).rejects.toBeInstanceOf(ProtocolError);
	});

	it('routes skills.delete with the name', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({ revision: { revision_id: 'r' }, protocol_version: '0.1.0' })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.agentConfig.skillsDelete('a1', 'recap');
		const [url, init] = call(fetchImpl);
		expect(url).toBe('http://127.0.0.1:18080/v1/agent_config/skills/delete');
		expect(JSON.parse(init.body as string).name).toBe('recap');
	});
});

describe('AgentConfig MCP policy (92d)', () => {
	it('routes set_tool_exposure with the paused/disabled sets', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({ revision: { revision_id: 'r' }, protocol_version: '0.1.0' })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.agentConfig.setToolExposure('a1', {
			paused_servers: ['github'],
			disabled_tools: ['github_delete_repo']
		});
		const [url, init] = call(fetchImpl);
		expect(url).toBe('http://127.0.0.1:18080/v1/agent_config/set_tool_exposure');
		const body = JSON.parse(init.body as string);
		expect(body.tool_exposure.paused_servers).toEqual(['github']);
		expect(body.tool_exposure.disabled_tools).toEqual(['github_delete_repo']);
	});
});

describe('AgentConfig layered prompt (92e)', () => {
	it('routes set_prompt_layers with the base + user layers', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({ revision: { revision_id: 'r' }, protocol_version: '0.1.0' })
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		await client.agentConfig.setPromptLayers('a1', { base: 'You are…', user: '' });
		const [url, init] = call(fetchImpl);
		expect(url).toBe('http://127.0.0.1:18080/v1/agent_config/set_prompt_layers');
		expect(JSON.parse(init.body as string).prompt_layers.base).toBe('You are…');
	});
});

describe('AgentConfig add MCP connection (92f)', () => {
	it('routes add_mcp_connection with the descriptor + secret headers', async () => {
		const fetchImpl = vi.fn(async () =>
			okResponse({
				connection: { name: 'github', transport: 'stdio' },
				state: 'online',
				protocol_version: '0.1.0'
			})
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		const resp = await client.agentConfig.addMcpConnection(
			'a1',
			{ name: 'github', transport: 'stdio', command: ['npx', 'server'] },
			{ Authorization: 'Bearer secret' }
		);
		const [url, init] = call(fetchImpl);
		expect(url).toBe('http://127.0.0.1:18080/v1/agent_config/add_mcp_connection');
		const body = JSON.parse(init.body as string);
		expect(body.connection.name).toBe('github');
		expect(body.headers.Authorization).toBe('Bearer secret');
		expect(resp.state).toBe('online');
	});

	it('maps a 403 onto a ProtocolError with the status preserved', async () => {
		const fetchImpl = vi.fn(
			async () =>
				new Response(JSON.stringify({ code: 'scope_mismatch', message: 'admin scope required' }), {
					status: 403,
					headers: { 'Content-Type': 'application/json' }
				})
		);
		const client = new HarborClient({ connection: CONNECTION, fetchImpl });
		try {
			await client.agentConfig.addMcpConnection('a1', { name: 'x', transport: 'stdio' });
			expect.unreachable('expected a ProtocolError');
		} catch (err) {
			expect(err).toBeInstanceOf(ProtocolError);
			expect((err as ProtocolError).code).toBe('scope_mismatch');
			expect((err as ProtocolError).status).toBe(403);
		}
	});
});
