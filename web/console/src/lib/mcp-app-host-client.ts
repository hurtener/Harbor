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
import type { ArtifactsGetResponse, ArtifactsGetRefResponse } from '$lib/protocol/artifacts.js';
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
 * The window length `fetchArtifactText` asks each `artifacts.get` for.
 *
 * Deliberately at the deployment DEFAULT ceiling rather than below it: a
 * request above the effective ceiling is SERVED at the ceiling and reports the
 * clamp through the same `truncated` / `returned_bytes` / `total_size_bytes`
 * fields every other bound uses (D-353) — it is not an error — so asking big
 * costs nothing and an operator who lowered the ceiling simply gets more,
 * smaller windows. The read pages until the response says it is complete, so
 * this value bounds ROUND TRIPS, never the bytes the host ends up with.
 */
export const ARTIFACT_READ_WINDOW_BYTES = 1024 * 1024;

/** Decode one base64 `artifacts.get` window into its raw bytes. */
function decodeArtifactWindow(content: string | undefined): Uint8Array {
  if (!content) return new Uint8Array(0);
  const binary = atob(content);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) out[i] = binary.charCodeAt(i);
  return out;
}

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
 *                         hands to the browser as a frame source, D-026)
 *   - `fetchArtifactText` → `artifacts.get` (the driver-independent byte read
 *                         every registered store serves, D-353)
 *
 * The two artifact methods are NOT two ways to do one thing. `artifacts.get`
 * is the CONTRACT read: it resolves through the mandatory `ArtifactStore.Get`,
 * so it answers on every driver including the `inmem` default a fresh
 * `harbor dev` boots on. `artifacts.get_ref` is a driver-specific TRANSPORT
 * OPTIMISATION: it type-asserts the optional presign capability that exactly
 * one of five shipped drivers implements, so it answers `presign_unsupported`
 * everywhere else. `resolveArtifact` keeps it deliberately — it needs a URL a
 * browser can load directly into a frame, which a Protocol method cannot mint
 * — while `fetchArtifactText`, which only ever needed the BYTES, reads them
 * through the method that works everywhere.
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
    async readResource(serverID, resourceURI, agentID): Promise<MCPAppResource> {
      const res = await client.mcp.servers.readResource<ReadMCPResourceResponse>(
        serverID,
        resourceURI,
        agentID,
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

    async callTool(tool, args, agentID): Promise<MCPAppToolResult> {
      let res: MCPAppCallToolResponse;
      try {
        res = await client.mcp.apps.callTool<MCPAppCallToolResponse>(tool, args, agentID);
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
      // the artifact id; the renderer hands the returned time-bounded URL to
      // the browser as the source it loads the heavy `ui://` document from.
      // The wire field is `presigned_url`
      // (internal/protocol/types/artifacts.go::ArtifactsGetRefResponse).
      //
      // This one stays on `get_ref` ON PURPOSE. Its caller needs a URL a
      // browser can load, which no Protocol method can mint (D-353 part 1:
      // Harbor advertises no externally-reachable address), so swapping it for
      // the byte read would be a regression, not a fix. The heavy-document arm
      // was already made driver-independent from the other side, by raising the
      // Runtime's inline cap for `ui://` documents (D-218), so a stock
      // deployment does not reach this call.
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
      // The heavy tool-context path: read the artifact's BYTES through
      // `artifacts.get` (D-353) and decode them as UTF-8 text.
      //
      // This used to resolve a presigned URL and fetch it. That route rests on
      // the OPTIONAL `artifacts.Presigner` capability, which exactly one of five
      // shipped drivers implements and which is NOT the `inmem` default — so on
      // a stock deployment it threw, and every heavy tool payload reached a
      // rendered App as the host's "unavailable" stub instead of its data
      // (D-347 consumer 1). `artifacts.get` resolves through the MANDATORY
      // `ArtifactStore.Get`, so every registered driver serves it.
      //
      // The read PAGES, because one response is bounded by the deployment's
      // fetch ceiling and says so: `truncated` is true while bytes remain, and
      // the next window starts at `offset + returned_bytes` — the response's own
      // report, never a locally-guessed cursor. A `truncated` response that
      // returned nothing would page forever, so it fails loudly instead.
      //
      // The windows are concatenated as BYTES and decoded ONCE. Decoding each
      // window on arrival would mangle any multi-byte rune that straddles a
      // window boundary, and a byte window may begin and end mid-rune by
      // design (windows are byte-addressed and MIME-agnostic, D-353 part 3).
      const windows: Uint8Array[] = [];
      let byteLength = 0;
      let offset = 0;
      for (;;) {
        const res = await client.artifacts.get<ArtifactsGetResponse>({
          id: artifactID,
          offset,
          max_bytes: ARTIFACT_READ_WINDOW_BYTES,
        });
        const window = decodeArtifactWindow(res.content);
        windows.push(window);
        byteLength += window.length;
        if (!res.truncated) break;
        const next = res.offset + res.returned_bytes;
        if (next <= offset) {
          throw new Error(
            `artifact ${artifactID}: the runtime reported more bytes after offset ` +
              `${offset} but returned none — refusing to page forever`,
          );
        }
        offset = next;
      }
      const all = new Uint8Array(byteLength);
      let at = 0;
      for (const window of windows) {
        all.set(window, at);
        at += window.length;
      }
      return new TextDecoder().decode(all);
    },
  };
}
