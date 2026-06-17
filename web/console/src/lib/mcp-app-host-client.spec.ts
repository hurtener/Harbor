// Phase 109b — the Console adapter that maps HarborClient onto the chat
// module's injected MCPAppHostClient. Proves each method routes to exactly one
// Protocol call and maps the wire shape correctly — the real production
// consumer of the new `mcp.servers.read_resource` / `mcp.apps.call_tool`
// client methods.

import { afterEach, describe, expect, it, vi } from 'vitest';

import { makeMCPAppHostClient } from './mcp-app-host-client.js';
import type { ProtocolClient } from './protocol/client.js';
import { ProtocolError } from './protocol/errors.js';

function fakeProtocolClient(): { client: ProtocolClient; readResource: ReturnType<typeof vi.fn>; callTool: ReturnType<typeof vi.fn>; resources: ReturnType<typeof vi.fn>; toolsList: ReturnType<typeof vi.fn>; getRef: ReturnType<typeof vi.fn>; toolContext: ReturnType<typeof vi.fn> } {
  const readResource = vi.fn(async () => ({
    resource_uri: 'ui://srv/app.html',
    mime_type: 'text/html',
    content: '<p>app</p>',
    protocol_version: '1',
  }));
  const callTool = vi.fn(async () => ({
    tool: 'srv_echo',
    content: { ok: true },
    is_error: false,
    protocol_version: '1',
  }));
  const resources = vi.fn(async () => ({
    resources: [{ uri: 'ui://srv/app.html', name: 'app', mime_type: 'text/html' }],
    protocol_version: '1',
  }));
  const toolsList = vi.fn(async () => ({
    tools: [
      { name: 'srv_echo', description: 'echo' },
      { name: 'other_tool', description: 'no' },
    ],
  }));
  const getRef = vi.fn(async () => ({
    ref: { id: 'art_studio_abc', size_bytes: 88_500 },
    presigned_url: 'https://artifacts.example/art_studio_abc',
    expires_at: '2026-06-13T00:00:00Z',
    protocol_version: '1',
  }));
  const toolContext = vi.fn(async () => ({
    tool: 'srv_echo',
    input: { content: { q: 1 } },
    result: { content: { ok: true } },
    is_error: false,
    protocol_version: '1',
  }));
  const client = {
    mcp: { servers: { readResource, resources }, apps: { callTool, toolContext } },
    tools: { list: toolsList },
    artifacts: { getRef },
  } as unknown as ProtocolClient;
  return { client, readResource, callTool, resources, toolsList, getRef, toolContext };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('makeMCPAppHostClient', () => {
  it('readResource routes to mcp.servers.read_resource and maps the response', async () => {
    const { client, readResource } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const res = await host.readResource('srv', 'ui://srv/app.html');
    expect(readResource).toHaveBeenCalledWith('srv', 'ui://srv/app.html');
    expect(res.content).toBe('<p>app</p>');
    expect(res.mimeType).toBe('text/html');
  });

  it('callTool routes to mcp.apps.call_tool and maps is_error', async () => {
    const { client, callTool } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const res = await host.callTool('srv_echo', { q: 1 });
    expect(callTool).toHaveBeenCalledWith('srv_echo', { q: 1 });
    expect(res.isError).toBe(false);
    expect(res.content).toEqual({ ok: true });
  });

  it('listResources routes to mcp.servers.resources', async () => {
    const { client, resources } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const rows = await host.listResources('srv');
    expect(resources).toHaveBeenCalledWith('srv');
    expect(rows[0].uri).toBe('ui://srv/app.html');
  });

  it('listTools narrows the catalog to the server prefix', async () => {
    const { client } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const rows = await host.listTools('srv');
    expect(rows.map((t) => t.name)).toEqual(['srv_echo']);
  });

  it('listResourceTemplates resolves to an empty list (graceful, no Protocol call) — the documented follow-up', async () => {
    const { client } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    // No MCP resource-template Protocol method exists yet; the adapter returns
    // an empty list so a conformant app's templates call resolves gracefully
    // rather than the advertised serverResources capability throwing.
    await expect(host.listResourceTemplates('srv')).resolves.toEqual([]);
  });

  it('resolveArtifact routes to artifacts.get_ref and returns the presigned_url (not the absent `url`)', async () => {
    const { client, getRef } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const url = await host.resolveArtifact('art_studio_abc');
    expect(getRef).toHaveBeenCalledWith({ id: 'art_studio_abc' });
    // The Go wire field is `presigned_url` — a `.url` read would be undefined.
    expect(url).toBe('https://artifacts.example/art_studio_abc');
  });

  it('toolContext routes to mcp.apps.tool_context and maps the wire shape', async () => {
    const { client, toolContext } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const ctx = await host.toolContext('srv', 'tc_1');
    expect(toolContext).toHaveBeenCalledWith('srv', 'tc_1');
    expect(ctx).not.toBeNull();
    expect(ctx!.tool).toBe('srv_echo');
    expect(ctx!.input.content).toEqual({ q: 1 });
    expect(ctx!.result.content).toEqual({ ok: true });
    expect(ctx!.isError).toBe(false);
  });

  it('toolContext maps a heavy (artifact_ref) payload half onto artifactRef', async () => {
    const { client } = fakeProtocolClient();
    client.mcp.apps.toolContext = vi.fn(async () => ({
      tool: 'srv_big',
      input: { content: {} },
      result: { artifact_ref: { id: 'art_heavy', mime_type: 'application/json', size_bytes: 9000 } },
      is_error: true,
      protocol_version: '1',
    })) as unknown as ProtocolClient['mcp']['apps']['toolContext'];
    const host = makeMCPAppHostClient(client);
    const ctx = await host.toolContext('srv', 'tc_big');
    expect(ctx!.result.artifactRef).toEqual({
      id: 'art_heavy',
      mimeType: 'application/json',
      sizeBytes: 9000,
    });
    expect(ctx!.isError).toBe(true);
  });

  it('toolContext maps a Runtime not_found onto null (no captured context)', async () => {
    const { client } = fakeProtocolClient();
    client.mcp.apps.toolContext = vi.fn(async () => {
      throw new ProtocolError('not_found', 'no tool context', 404);
    }) as unknown as ProtocolClient['mcp']['apps']['toolContext'];
    const host = makeMCPAppHostClient(client);
    await expect(host.toolContext('srv', 'tc_missing')).resolves.toBeNull();
  });

  it('toolContext re-throws a non-not_found Protocol error (fail-loud)', async () => {
    const { client } = fakeProtocolClient();
    client.mcp.apps.toolContext = vi.fn(async () => {
      throw new ProtocolError('identity_scope_required', 'forbidden', 403);
    }) as unknown as ProtocolClient['mcp']['apps']['toolContext'];
    const host = makeMCPAppHostClient(client);
    await expect(host.toolContext('srv', 'tc_x')).rejects.toThrow(/forbidden/);
  });

  it('fetchArtifactText resolves the ref then fetches the bytes as text', async () => {
    const { client, getRef } = fakeProtocolClient();
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200, text: async () => 'BYTES' }));
    vi.stubGlobal('fetch', fetchMock);
    const host = makeMCPAppHostClient(client);
    const text = await host.fetchArtifactText('art_studio_abc');
    expect(getRef).toHaveBeenCalledWith({ id: 'art_studio_abc' });
    expect(fetchMock).toHaveBeenCalledWith('https://artifacts.example/art_studio_abc');
    expect(text).toBe('BYTES');
  });

  it('fetchArtifactText throws loudly on a non-OK fetch (never silently empty)', async () => {
    const { client } = fakeProtocolClient();
    const fetchMock = vi.fn(async () => ({ ok: false, status: 503, text: async () => '' }));
    vi.stubGlobal('fetch', fetchMock);
    const host = makeMCPAppHostClient(client);
    await expect(host.fetchArtifactText('art_studio_abc')).rejects.toThrow(/HTTP 503/);
  });
});
