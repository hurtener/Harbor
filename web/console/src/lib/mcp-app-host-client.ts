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
  MCPAppToolContext,
  MCPAppToolListing,
  MCPAppToolResult,
} from '$lib/chat/renderers/app-bridge-host.js';
import type { ArtifactsGetRefResponse } from '$lib/protocol/artifacts.js';
import type { ProtocolClient } from '$lib/protocol/client.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import type {
  MCPAppCallToolResponse,
  MCPServerResourcesResponse,
  ReadMCPResourceResponse,
  ToolContextPayload,
  ToolContextResponse,
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
 *   - `resolveArtifact` → `artifacts.get_ref` (the heavy `ui://` document's
 *                         by-reference stub → a presigned URL the renderer
 *                         fetches the bytes from, D-026)
 *   - `toolContext`  → `mcp.apps.tool_context` (the captured input + lowered
 *                         result the host pushes into a rendered app after it
 *                         initializes — the Data Delivery lifecycle stage; a
 *                         Runtime `not_found` maps to `null`)
 *   - `fetchArtifactText` → `artifacts.get_ref` + a raw `fetch` of the bytes
 *                         (the heavy tool-context path; the raw `fetch` lives
 *                         HERE in the adapter so the chat module stays
 *                         transport-free, D-173)
 *
 * Identity rides on the Protocol client's request choke point, so each call is
 * `(tenant, user, session)` scoped — there is no parallel, unscoped path.
 */
export function makeMCPAppHostClient(client: ProtocolClient): MCPAppHostClient {
  function mapPayload(p: ToolContextPayload): MCPAppToolContext['input'] {
    return {
      content: p.content,
      artifactRef: p.artifact_ref
        ? {
            id: p.artifact_ref.id,
            mimeType: p.artifact_ref.mime_type,
            sizeBytes: p.artifact_ref.size_bytes,
          }
        : undefined,
    };
  }
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

    async resolveArtifact(artifactID): Promise<string> {
      // The read-side presigned-URL resolver (D-026). The Runtime backfills the
      // identity scope from the request choke point, so the body carries only
      // the artifact id; the renderer fetches the document bytes from the
      // returned time-bounded URL. The wire field is `presigned_url`
      // (internal/protocol/types/artifacts.go::ArtifactsGetRefResponse).
      const res = await client.artifacts.getRef<ArtifactsGetRefResponse>({ id: artifactID });
      return res.presigned_url;
    },

    async toolContext(serverID, toolCallID): Promise<MCPAppToolContext | null> {
      try {
        const res = await client.mcp.apps.toolContext<ToolContextResponse>(serverID, toolCallID);
        return {
          tool: res.tool,
          input: mapPayload(res.input),
          result: mapPayload(res.result),
          isError: res.is_error,
        };
      } catch (err) {
        // A missing / evicted / cross-identity (server_id, tool_call_id) is
        // reported by the Runtime as `not_found` — the host treats that as
        // "no captured context" and performs no push (degraded, never a thrown
        // render error). Any OTHER error is a real failure — re-throw it.
        if (err instanceof ProtocolError && err.code === 'not_found') {
          return null;
        }
        throw err;
      }
    },

    async fetchArtifactText(artifactID): Promise<string> {
      // The heavy tool-context path: resolve the by-reference stub to a
      // presigned URL, then fetch the bytes as text. The raw `fetch` lives in
      // this adapter (the same place the renderer's heavy-doc fetch does) so
      // the chat module's `app-bridge-host.ts` never issues a direct network
      // call (D-173). A non-OK response throws — the host then delivers a
      // faithful by-reference stub rather than silently empty data (§13).
      const ref = await client.artifacts.getRef<ArtifactsGetRefResponse>({ id: artifactID });
      const resp = await fetch(ref.presigned_url);
      if (!resp.ok) {
        throw new Error(`failed to fetch tool-context artifact ${artifactID}: HTTP ${resp.status}`);
      }
      return resp.text();
    },
  };
}
