// The Harbor Console MCP Apps host — the AppBridge wrapper, the sandbox /
// CSP constants, and the postMessage origin guard.
//
// # What this module is
//
// An MCP tool can declare an interactive HTML UI via a `ui://` resource
// referenced from its result's `_meta.ui.resourceUri`. The Console renders
// that UI inside a sandboxed iframe and bridges app↔host traffic over the
// official `@modelcontextprotocol/ext-apps` AppBridge `postMessage` JSON-RPC
// dialect (`ui/initialize`, `tools/call`, `resources/read`, `resources/list`,
// `ui/request-display-mode`).
//
// # The load-bearing invariant — manual-handler mode (D-173)
//
// The official AppBridge supports two modes: AUTO-FORWARD (it wraps a live
// MCP `Client` and proxies app requests straight to the MCP server) and
// MANUAL-HANDLER (the host registers handlers and wires each itself). The
// Harbor Console is NOT an MCP client — it is a Protocol client of the
// Harbor Runtime (CLAUDE.md §4.5). The Runtime owns the MCP southbound
// connection, the `(tenant, user, session)` isolation boundary, audit
// redaction, and the unified approval / OAuth / pause gates.
//
// So this wrapper ALWAYS constructs `new AppBridge(null, …)` — it can never
// hold an MCP `Client`, and it never opens a direct MCP transport. Every
// app→host request is routed to the INJECTED `MCPAppHostClient` (which the
// caller adapts onto the Harbor Protocol client → Runtime → MCP southbound).
// An app call to a gated tool therefore parks on the unified pause primitive
// exactly as a planner call does — no bypass. The wrapper's public surface
// has no seam for an MCP transport; that is the structural guarantee the
// `mode` getter and the no-direct-transport test assert.
//
// # Encapsulation (D-091, CLAUDE.md §4.5 #11)
//
// This module lives inside the shared chat module (`$lib/chat/`) and imports
// NOTHING from outside it except the external `@modelcontextprotocol/ext-apps`
// + `@modelcontextprotocol/sdk` packages (core + app-bridge entry points
// only — never the `/react` entry) and `svelte`. The Harbor Protocol client
// is INJECTED via the minimal `MCPAppHostClient` interface declared here; the
// caller (the Playground page) adapts the real `HarborClient` onto it. The
// chat module never reaches for a Console-specific singleton.

import {
  AppBridge,
  PostMessageTransport,
  type McpUiDisplayMode,
  type McpUiHostCapabilities,
  type McpUiHostContext,
} from '@modelcontextprotocol/ext-apps/app-bridge';

/** The MCP-Apps display mode union, re-exported for host callers. */
export type { McpUiDisplayMode };
import type {
  CallToolResult,
  ListResourcesResult,
  ReadResourceResult,
} from '@modelcontextprotocol/sdk/types.js';

/* ------------------------------------------------------------------ */
/* Injected client surface (the only seam to the Harbor Protocol)      */
/* ------------------------------------------------------------------ */

/**
 * The host-side projection of an MCP App reference (mirrors the Protocol
 * `MCPAppRef`). The caller maps the wire shape onto this minimal view so the
 * chat module stays free of any `$lib/protocol` import.
 */
export interface MCPAppRefView {
  /** The `ui://`-scheme URI of the app's UI document. */
  resourceUri: string;
  /** The negotiated display mode; Phase 109b consumes only `inline`. */
  displayMode?: McpUiDisplayMode | '';
  /** The per-server raw-HTML trust flag — default-deny. */
  rawHtmlTrusted: boolean;
}

/** A `ui://` resource fetched through the host (mirrors `ReadMCPResourceResponse`). */
export interface MCPAppResource {
  /** The fetched resource URI. */
  resourceUri: string;
  /** The resource's declared media type. */
  mimeType?: string;
  /**
   * The inline resource bytes — set ONLY when the content is below the
   * heavy-content threshold (D-026). A heavy resource carries `artifactRef`
   * instead and the host fails loudly rather than silently inlining.
   */
  content?: string;
  /** The by-reference stub when the content meets/exceeds the heavy threshold. */
  artifactRef?: { id: string; mimeType?: string; sizeBytes?: number };
}

/** An app-proxied tool result (mirrors `MCPAppCallToolResponse`). */
export interface MCPAppToolResult {
  /** The invoked tool name. */
  tool: string;
  /** The inline tool-result JSON — set only below the heavy threshold. */
  content?: unknown;
  /** The by-reference stub when the result meets/exceeds the heavy threshold. */
  artifactRef?: { id: string; mimeType?: string; sizeBytes?: number };
  /** Whether the MCP server returned a tool error. */
  isError: boolean;
}

/** One advertised resource (mirrors a row of `mcp.servers.resources`). */
export interface MCPAppResourceListing {
  uri: string;
  name?: string;
  mimeType?: string;
}

/** One advertised tool (mirrors a row of the tool catalog). */
export interface MCPAppToolListing {
  name: string;
  description?: string;
  inputSchema?: Record<string, unknown>;
}

/**
 * The minimal, injected Protocol surface the MCP Apps host drives every
 * app→host request through. The caller (the Playground page) implements this
 * by delegating to the Harbor Protocol client's `mcp.servers.read_resource`
 * / `mcp.apps.call_tool` / `mcp.servers.resources` / `tools.list` methods —
 * NEVER to a direct MCP transport (D-173). Identity rides on the Protocol
 * client's request choke point, so every call is `(tenant, user, session)`
 * scoped.
 */
export interface MCPAppHostClient {
  /** Route `resources/read` → `mcp.servers.read_resource`. */
  readResource(serverID: string, resourceURI: string): Promise<MCPAppResource>;
  /** Route `tools/call` → `mcp.apps.call_tool` (re-enters the tool-safety gates). */
  callTool(tool: string, args?: unknown): Promise<MCPAppToolResult>;
  /** Route `resources/list` → `mcp.servers.resources`. */
  listResources(serverID: string): Promise<MCPAppResourceListing[]>;
  /**
   * Route a tool listing → the tool catalog filtered to this server. NOTE:
   * the official AppBridge exposes no `onlisttools` host handler (an app
   * discovers its tools via host-pushed `tool-input`, not an app-initiated
   * `tools/list`), so this is a host-callable capability the renderer may use
   * — it is intentionally NOT wired as a bridge handler (Phase 109b plan
   * deviation; the named `onlisttools` handler does not exist in
   * `@modelcontextprotocol/ext-apps`).
   */
  listTools(serverID: string): Promise<MCPAppToolListing[]>;
}

/* ------------------------------------------------------------------ */
/* The sandbox + CSP + origin-guard surface lives in `sandbox-policy.ts` */
/* (dependency-free so the Playwright sandbox-escape spec imports the    */
/* SAME constants the renderer ships). Re-exported here for callers that */
/* already reach for the host module.                                    */
/* ------------------------------------------------------------------ */

export {
  APP_IFRAME_SANDBOX_BASE,
  appIframeSandbox,
  assertSandboxTokensSafe,
  buildAppCSP,
  wrapAppDocument,
  isTrustedAppMessage,
  type ExpectedAppPeer,
} from './sandbox-policy.js';

/* ------------------------------------------------------------------ */
/* Manual-handler factory + the AppBridge host wrapper                 */
/* ------------------------------------------------------------------ */

/** A record of a (necessarily acked-but-not-applied) display-mode request. */
export interface DisplayModeRequest {
  /** The mode the app asked for. */
  requested: McpUiDisplayMode;
  /** The mode actually in force after the request (inline-only in 109b). */
  granted: McpUiDisplayMode;
}

/** Options for {@link createAppHandlers} / {@link AppBridgeHost}. */
export interface AppBridgeHostOptions {
  /** The injected Harbor Protocol surface — the ONLY path off the iframe. */
  client: MCPAppHostClient;
  /** The MCP server (source id) hosting the app's tools + resources. */
  serverID: string;
  /**
   * Called when the app requests a display mode. The request is recorded and
   * acked with the GRANTED mode (see {@link availableDisplayModes}). The
   * Playground page consumes this to drive its page-level layout (fullscreen /
   * pip). Optional.
   */
  onDisplayModeRequest?: (req: DisplayModeRequest) => void;
  /**
   * The display modes the host can actually apply. Defaults to `['inline']`
   * (the inline-only chat-scroll host). The Playground page passes the full set
   * (`['inline','fullscreen','pip']`) because it owns the page-level layout that
   * applies fullscreen / pip; a requested mode outside this set is granted as
   * `inline` (fail-safe to the always-available mode). Optional.
   */
  availableDisplayModes?: McpUiDisplayMode[];
}

/**
 * The set of manual handlers wired onto the AppBridge. Each routes through the
 * injected `MCPAppHostClient` — never a direct MCP transport. Exported as a
 * factory so the handler logic is unit-testable without a DOM / iframe.
 */
export interface AppHandlers {
  oncalltool(params: { name: string; arguments?: unknown }): Promise<CallToolResult>;
  onreadresource(params: { uri: string }): Promise<ReadResourceResult>;
  onlistresources(): Promise<ListResourcesResult>;
  onrequestdisplaymode(params: { mode: McpUiDisplayMode }): Promise<{ mode: McpUiDisplayMode }>;
}

/**
 * Builds the manual handlers. Every handler dispatches to `opts.client`
 * (the injected Protocol surface) and NOTHING else — no `fetch`, no
 * `WebSocket`, no `EventSource`. The no-direct-transport test drives these
 * with network spies installed and asserts zero network calls.
 */
export function createAppHandlers(opts: AppBridgeHostOptions): AppHandlers {
  const { client, serverID } = opts;
  return {
    async oncalltool({ name, arguments: args }) {
      // → mcp.apps.call_tool: re-enters the SAME identity + approval-gate +
      //   tool-side-OAuth path a planner call uses. A gated tool parks on the
      //   unified pause primitive (D-173).
      const result = await client.callTool(name, args);
      const blocks: CallToolResult['content'] = [];
      if (result.artifactRef) {
        // Heavy result rides by reference (D-026) — surface the stub, never
        // silently inline (fail loudly).
        blocks.push({
          type: 'text',
          text: `[artifact ${result.artifactRef.id}${
            result.artifactRef.sizeBytes ? ` · ${result.artifactRef.sizeBytes} bytes` : ''
          }]`,
        });
      } else {
        blocks.push({ type: 'text', text: stringifyContent(result.content) });
      }
      const out: CallToolResult = { content: blocks, isError: result.isError };
      if (result.content !== undefined && result.content !== null) {
        out.structuredContent = asStructured(result.content);
      }
      return out;
    },

    async onreadresource({ uri }) {
      // → mcp.servers.read_resource, identity-scoped + heavy-content aware.
      const res = await client.readResource(serverID, uri);
      if (res.artifactRef) {
        // The app asked to read a heavy resource inline — refuse loudly
        // rather than truncate or return an empty resource silently.
        throw new Error(
          `resource ${uri} exceeds the inline heavy-content threshold ` +
            `(artifact ${res.artifactRef.id}); read it via the artifacts surface.`,
        );
      }
      return {
        contents: [
          {
            uri: res.resourceUri,
            mimeType: res.mimeType,
            text: res.content ?? '',
          },
        ],
      };
    },

    async onlistresources() {
      const rows = await client.listResources(serverID);
      return {
        resources: rows.map((r) => ({ uri: r.uri, name: r.name ?? r.uri, mimeType: r.mimeType })),
      };
    },

    async onrequestdisplaymode({ mode }) {
      // Grant the requested mode when the host can apply it; otherwise fall back
      // to the always-available `inline`. The inline-only chat-scroll host (the
      // default, no `availableDisplayModes`) keeps the original behaviour; the
      // Playground page passes the full set so fullscreen / pip are granted and
      // routed to its page-level layout machine.
      const available = opts.availableDisplayModes ?? (['inline'] as McpUiDisplayMode[]);
      const granted: McpUiDisplayMode = available.includes(mode) ? mode : 'inline';
      opts.onDisplayModeRequest?.({ requested: mode, granted });
      return { mode: granted };
    },
  };
}

function stringifyContent(content: unknown): string {
  if (content === undefined || content === null) return '';
  if (typeof content === 'string') return content;
  try {
    return JSON.stringify(content);
  } catch {
    // A non-serialisable result is a real bug upstream — surface it, do not
    // swallow it (CLAUDE.md §5 fail-loudly).
    return String(content);
  }
}

function asStructured(content: unknown): Record<string, unknown> {
  if (content && typeof content === 'object' && !Array.isArray(content)) {
    return content as Record<string, unknown>;
  }
  return { value: content };
}

/** The handshake host identity advertised in `ui/initialize`. */
const HOST_INFO = { name: 'harbor-console', version: '1' } as const;

/**
 * The host capabilities advertised to the app. Phase 109b advertises the two
 * server proxies (tools + resources), inline display only, and the host
 * sandbox posture. Notably absent: `experimental` auto-forward — there is no
 * MCP client to forward to.
 */
function hostCapabilities(): McpUiHostCapabilities {
  return {
    serverTools: {},
    serverResources: {},
  };
}

/**
 * The MCP Apps host wrapper. Constructs the official `AppBridge` in
 * MANUAL-HANDLER mode (the first constructor argument is ALWAYS `null` — no
 * MCP `Client`, ever), wires the manual handlers, and drives the
 * `ui/initialize` handshake over a `PostMessageTransport` bound to the app's
 * iframe `contentWindow`.
 *
 * The wrapper's public surface has no seam for an MCP transport; the `mode`
 * getter reports `'manual-handler'` so the no-direct-transport invariant is
 * observable.
 */
export class AppBridgeHost {
  readonly #bridge: AppBridge;
  readonly #handlers: AppHandlers;
  #transport: PostMessageTransport | undefined;
  #displayModeRequests: DisplayModeRequest[] = [];
  #connected = false;
  #initialized = false;

  constructor(opts: AppBridgeHostOptions, theme: 'light' | 'dark' = 'dark') {
    this.#handlers = createAppHandlers({
      ...opts,
      onDisplayModeRequest: (req) => {
        this.#displayModeRequests.push(req);
        opts.onDisplayModeRequest?.(req);
      },
    });

    const available = opts.availableDisplayModes ?? (['inline'] as McpUiDisplayMode[]);
    const hostContext: McpUiHostContext = {
      theme,
      // The app boots inline (in the chat scroll); fullscreen / pip are reached
      // via a `ui/request-display-mode` the page's layout machine applies.
      displayMode: 'inline',
      availableDisplayModes: available,
    };

    // The load-bearing line: the first argument is `null`. The AppBridge is
    // NEVER handed an MCP Client, so it can never auto-forward to a direct MCP
    // transport (D-173).
    this.#bridge = new AppBridge(null, HOST_INFO, hostCapabilities(), { hostContext });

    this.#bridge.oncalltool = (params) => this.#handlers.oncalltool(params);
    this.#bridge.onreadresource = (params) => this.#handlers.onreadresource(params);
    this.#bridge.onlistresources = () => this.#handlers.onlistresources();
    this.#bridge.onrequestdisplaymode = (params) => this.#handlers.onrequestdisplaymode(params);
    this.#bridge.oninitialized = () => {
      this.#initialized = true;
    };
  }

  /** Always `'manual-handler'` — the wrapper holds no MCP client (D-173). */
  get mode(): 'manual-handler' {
    return 'manual-handler';
  }

  /** The display-mode requests the app has made (recorded, inline-only acked). */
  get displayModeRequests(): readonly DisplayModeRequest[] {
    return this.#displayModeRequests;
  }

  /**
   * Connects the bridge to the app iframe. Builds a `PostMessageTransport`
   * bound to the iframe's `contentWindow` for BOTH the send target and the
   * source-validation window — so messages are only accepted from the
   * expected frame. The transport's source check is the official guard;
   * {@link isTrustedAppMessage} is the host's belt-and-braces second factor
   * for any message the wrapper inspects directly.
   */
  async connect(iframeWindow: Window): Promise<void> {
    if (this.#connected) return;
    this.#transport = new PostMessageTransport(iframeWindow, iframeWindow);
    await this.#bridge.connect(this.#transport);
    this.#connected = true;
  }

  /** True once the app has sent `ui/notifications/initialized`. */
  get isInitialized(): boolean {
    return this.#initialized;
  }

  /** Tears the bridge down — closes the transport and drops the iframe peer. */
  async close(): Promise<void> {
    if (!this.#connected) return;
    this.#connected = false;
    await this.#bridge.close();
  }
}
