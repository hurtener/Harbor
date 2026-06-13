// The Console-side adapter that maps the unified `HarborClient` onto the
// chat module's injected `MCPAppHostClient` interface (D-091, D-173).
//
// The shared chat module (`$lib/chat/`) is self-contained: it imports nothing
// from `$lib/protocol` and instead declares the minimal `MCPAppHostClient`
// surface its MCP Apps renderer needs. THIS module — a Console internal,
// outside the chat module — is where the two meet: it adapts the real
// `HarborClient` (which DOES import `$lib/protocol`) onto that interface,
// routing every app→host request through the Harbor Protocol client →
// Runtime → MCP southbound. The Console opens NO direct MCP transport.
//
// The Playground page injects the result of {@link makeMCPAppHostClient} into
// the MCP App renderer's props (`appHostClient`).

import type {
  MCPAppHostClient,
  MCPAppResource,
  MCPAppResourceListing,
  MCPAppToolListing,
  MCPAppToolResult,
} from '$lib/chat/renderers/app-bridge-host.js';
import type { ProtocolClient } from '$lib/protocol/client.js';
import type {
  MCPAppCallToolResponse,
  MCPServerResourcesResponse,
  ReadMCPResourceResponse,
} from '$lib/protocol/mcp.js';
import type { ToolListResponse } from '$lib/protocol/tools.js';

/**
 * Adapts a `HarborClient` (the `ProtocolClient` interface) onto the chat
 * module's `MCPAppHostClient`. Every method delegates to a single Protocol
 * call:
 *
 *   - `readResource` → `mcp.servers.read_resource`
 *   - `callTool`     → `mcp.apps.call_tool` (re-enters the tool-safety gates)
 *   - `listResources`→ `mcp.servers.resources`
 *   - `listTools`    → `tools.list`, narrowed to the server's `<source>_*` rows
 *
 * Identity rides on the Protocol client's request choke point, so each call is
 * `(tenant, user, session)` scoped — there is no parallel, unscoped path.
 */
export function makeMCPAppHostClient(client: ProtocolClient): MCPAppHostClient {
  return {
    async readResource(serverID, resourceURI): Promise<MCPAppResource> {
      const res = await client.mcp.servers.readResource<ReadMCPResourceResponse>(
        serverID,
        resourceURI,
      );
      return {
        resourceUri: res.resource_uri,
        mimeType: res.mime_type,
        content: res.content,
        artifactRef: res.artifact_ref
          ? {
              id: res.artifact_ref.id,
              mimeType: res.artifact_ref.mime_type,
              sizeBytes: res.artifact_ref.size_bytes,
            }
          : undefined,
      };
    },

    async callTool(tool, args): Promise<MCPAppToolResult> {
      const res = await client.mcp.apps.callTool<MCPAppCallToolResponse>(tool, args);
      return {
        tool: res.tool,
        content: res.content,
        isError: res.is_error,
        artifactRef: res.artifact_ref
          ? {
              id: res.artifact_ref.id,
              mimeType: res.artifact_ref.mime_type,
              sizeBytes: res.artifact_ref.size_bytes,
            }
          : undefined,
      };
    },

    async listResources(serverID): Promise<MCPAppResourceListing[]> {
      const res = await client.mcp.servers.resources<MCPServerResourcesResponse>(serverID);
      return res.resources.map((r) => ({
        uri: r.uri,
        name: r.name ?? r.title,
        mimeType: r.mime_type,
      }));
    },

    async listTools(serverID): Promise<MCPAppToolListing[]> {
      // The tool catalog has no per-source filter field; Harbor names an MCP
      // server's tools `<source>_<tool>`, so narrow client-side by prefix.
      const res = await client.tools.list<ToolListResponse>();
      const prefix = `${serverID}_`;
      return res.tools
        .filter((t) => t.name.startsWith(prefix))
        .map((t) => ({ name: t.name, description: t.description }));
    },
  };
}
