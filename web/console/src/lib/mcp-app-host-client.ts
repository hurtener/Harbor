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

import { MCPAppToolNotFoundError } from '$lib/chat/renderers/app-bridge-host.js';
import type {
  MCPAppHostClient,
  MCPAppResource,
  MCPAppResourceListing,
  MCPAppResourceTemplateListing,
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
 *   - `callTool`     → `mcp.apps.call_tool` (re-enters the tool-safety gates);
 *                      a `not_found` becomes the typed `MCPAppToolNotFoundError`
 *                      so an app can tell "no such tool here" from "the
 *                      transport broke"
 *   - `listResources`→ `mcp.servers.resources`
 *   - `listResourceTemplates` → no Protocol method exists; resolves empty
 *                      (see the method's note)
 *   - `listTools`    → `tools.list`, narrowed to the server's `<source>_*` rows
 *   - `resolveArtifact` → `artifacts.get_ref` (the heavy `ui://` document's
 *                         by-reference stub → a presigned URL the renderer
 *                         fetches the bytes from, D-026)
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
      let res: MCPAppCallToolResponse;
      try {
        res = await client.mcp.apps.callTool<MCPAppCallToolResponse>(tool, args);
      } catch (err) {
        // `tool` is already server-qualified by the caller (the confinement
        // control), so a `not_found` means the name does not exist WITHIN the
        // calling app's own server — the one outcome an app can act on. Raise
        // the typed error so the host handler can tell the app that, instead of
        // the undifferentiated runtime failure the Runtime used to return for
        // an unresolvable tool. Every other failure propagates unchanged.
        if (err instanceof ProtocolError && err.code === 'not_found') {
          throw new MCPAppToolNotFoundError(tool);
        }
        throw err;
      }
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

    async listResourceTemplates(_serverID): Promise<MCPAppResourceTemplateListing[]> {
      // Harbor's Protocol exposes no resource-TEMPLATE method — `mcp.servers.
      // resources` lists concrete resources only. The host nonetheless
      // advertises the `serverResources` capability, and an advertised
      // capability must answer rather than error, so this reports the honest
      // state of the surface: this host exposes no templates. It is not a
      // swallowed failure — there is no call to fail. When a Protocol
      // `mcp.servers.resource_templates` lands, this method routes to it and
      // nothing else in the host changes.
      return [];
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
        // "no captured context" and performs no delivery (degraded, never a
        // thrown render error). Any OTHER error is a real failure — re-throw it.
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
      // faithful by-reference stub rather than silently empty data.
      const ref = await client.artifacts.getRef<ArtifactsGetRefResponse>({ id: artifactID });
      const resp = await fetch(ref.presigned_url);
      if (!resp.ok) {
        throw new Error(`failed to fetch tool-context artifact ${artifactID}: HTTP ${resp.status}`);
      }
      return resp.text();
    },
  };
}
