// Phase 109b — the Console adapter that maps HarborClient onto the chat
// module's injected MCPAppHostClient. Proves each method routes to exactly one
// Protocol call and maps the wire shape correctly — the real production
// consumer of the new `mcp.servers.read_resource` / `mcp.apps.call_tool`
// client methods.

import { describe, expect, it, vi } from 'vitest';

import { makeMCPAppHostClient } from './mcp-app-host-client.js';
import type { ProtocolClient } from './protocol/client.js';

function fakeProtocolClient(): { client: ProtocolClient; readResource: ReturnType<typeof vi.fn>; callTool: ReturnType<typeof vi.fn>; resources: ReturnType<typeof vi.fn>; toolsList: ReturnType<typeof vi.fn>; getRef: ReturnType<typeof vi.fn> } {
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
  const client = {
    mcp: { servers: { readResource, resources }, apps: { callTool } },
    tools: { list: toolsList },
    artifacts: { getRef },
  } as unknown as ProtocolClient;
  return { client, readResource, callTool, resources, toolsList, getRef };
}

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

  it('resolveArtifact routes to artifacts.get_ref and returns the presigned_url (not the absent `url`)', async () => {
    const { client, getRef } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const url = await host.resolveArtifact('art_studio_abc');
    expect(getRef).toHaveBeenCalledWith({ id: 'art_studio_abc' });
    // The Go wire field is `presigned_url` — a `.url` read would be undefined.
    expect(url).toBe('https://artifacts.example/art_studio_abc');
  });
});
