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
  type McpUiHostStyles,
  type McpUiTheme,
} from '@modelcontextprotocol/ext-apps/app-bridge';

/** The MCP-Apps display mode union, re-exported for host callers. */
export type { McpUiDisplayMode };
import type {
  CallToolResult,
  ListResourcesResult,
  ListResourceTemplatesResult,
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
	/** Opaque host/render-issued callback capability; never sandbox-authored. */
	binding?: string;
  /**
   * Runtime-authored effective agent configuration. The host echoes this
   * value; it never accepts an agent id from the sandboxed App.
   */
  agentId?: string;
  /** The `ui://`-scheme URI of the app's UI document. */
  resourceUri: string;
  /** The negotiated display mode; the inline host consumes only `inline`. */
  displayMode?: McpUiDisplayMode | '';
  /** The per-server raw-HTML trust flag — default-deny. */
  rawHtmlTrusted: boolean;
  /**
   * The stable per-invocation id of the tool call that declared the app —
   * paired with the server id to fetch the captured tool context (input +
   * lowered result) the host delivers into the app after it initializes (the
   * Data Delivery lifecycle stage). Empty/absent when the discovery event
   * carried no correlation id; the host then performs no delivery.
   */
  toolCallId?: string;
  /**
   * The server-side tool name that declared the app, carried on the
   * `mcp.app_available` discovery event. The host projects it onto the
   * `ui/initialize` host-context `toolInfo` so a rendered app can name the call
   * that instantiated it. Display metadata only — never an authorization input
   * (the app's tool namespace is derived from the server id, not from here).
   * Absent when the discovery recorded no tool name.
   */
  toolName?: string;
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

/**
 * One advertised resource TEMPLATE (a parameterised `uriTemplate` row). Harbor's
 * Protocol advertises no resource-template surface today, so the adapter
 * resolves an empty list — see {@link MCPAppHostClient.listResourceTemplates}.
 */
export interface MCPAppResourceTemplateListing {
  uriTemplate: string;
  name?: string;
  mimeType?: string;
}

/**
 * The typed failure an app-initiated `tools/call` raises when the requested
 * name does not resolve WITHIN the calling app's own server namespace.
 *
 * It exists so an app can degrade DELIBERATELY (render "that action is not
 * available here") instead of guessing from a generic transport error. The host
 * previously surfaced this as an undifferentiated runtime failure whose message
 * read "MCP read failed", which an app could not distinguish from a broken
 * southbound transport.
 *
 * The chat module declares the type; the Console-side adapter (which owns the
 * Protocol import, D-091) raises it when the Runtime answers `not_found`, and
 * {@link createAppHandlers} re-raises it naming the BARE name the app asked for
 * plus the server it is confined to.
 */
export class MCPAppToolNotFoundError extends Error {
  /**
   * The tool name that did not resolve. The adapter raises it holding the
   * SERVER-QUALIFIED name it dispatched; the host handler re-raises it holding
   * the BARE name the app actually asked for, which is the only form the app
   * can match against what it sent.
   */
  readonly tool: string;
  /**
   * The MCP server the calling app is confined to. Undefined on the adapter's
   * raise (which sees only the qualified name and cannot split a server id that
   * may itself contain underscores); always set by the time the app sees it.
   */
  readonly serverID: string | undefined;

  constructor(tool: string, serverID?: string) {
    super(
      serverID === undefined
        ? `tool ${JSON.stringify(tool)} does not resolve`
        : `tool ${JSON.stringify(tool)} is not available on MCP server ` +
          `${JSON.stringify(serverID)} (an app may only call its own server's tools)`,
    );
    this.name = 'MCPAppToolNotFoundError';
    this.tool = tool;
    this.serverID = serverID;
  }
}

/**
 * The typed failure a by-reference payload half raises when its bytes cannot be
 * read — the host asked the injected client for the artifact's text and did not
 * get it (the read failed, or the bytes were not the payload they were promised
 * to be).
 *
 * It exists for the same reason {@link MCPAppToolNotFoundError} does: a caller
 * must be able to tell WHICH thing went wrong. Before it, the input path
 * collapsed every failure into an empty argument map, which asserts a different
 * fact — "the tool ran with no arguments" — and is indistinguishable from the
 * genuinely-absent case (CLAUDE.md §13, no silent degradation). Each call site
 * decides what to tell the app; none of them may claim there was nothing there.
 */
export class MCPAppArtifactUnavailableError extends Error {
  /** The artifact id whose bytes could not be delivered. */
  readonly artifactID: string;
  /** The stub's reported size, when the payload carried one. */
  readonly sizeBytes: number | undefined;

  constructor(artifactID: string, sizeBytes: number | undefined, cause: unknown) {
    super(`artifact ${JSON.stringify(artifactID)} could not be read`, { cause });
    this.name = 'MCPAppArtifactUnavailableError';
    this.artifactID = artifactID;
    this.sizeBytes = sizeBytes;
  }
}

/** One advertised tool (mirrors a row of the tool catalog). */
export interface MCPAppToolListing {
  name: string;
  description?: string;
  inputSchema?: Record<string, unknown>;
}

/**
 * One half (input or result) of a captured tool context (mirrors the Protocol
 * `ToolContextPayload`). EXACTLY ONE of `content` / `artifactRef` is set:
 * `content` carries inline JSON below the heavy-content threshold (D-026);
 * `artifactRef` carries the by-reference stub at or above it, which the host
 * resolves + fetches at the iframe edge before delivering.
 */
export interface MCPAppToolContextPayload {
  /** Inline JSON — set only below the heavy threshold. */
  content?: unknown;
  /** The by-reference stub when the payload meets/exceeds the heavy threshold. */
  artifactRef?: { id: string; mimeType?: string; sizeBytes?: number };
}

/**
 * The captured tool context (input + lowered result) that produced a rendered
 * `ui://` MCP App (mirrors the Protocol `ToolContextResponse`). The host
 * delivers this into the app after `ui/notifications/initialized` — `input`
 * via `sendToolInput`, then `result` via `sendToolResult` — closing the Data
 * Delivery lifecycle.
 */
export interface MCPAppToolContext {
  /** The server-side tool name that declared the app. */
  tool: string;
  /** The tool's input arguments (inline JSON or by reference). */
  input: MCPAppToolContextPayload;
  /** The tool's lowered result (inline JSON or by reference). */
  result: MCPAppToolContextPayload;
  /** Whether the tool returned a server-side error result. */
  isError: boolean;
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
  readResource(serverID: string, resourceURI: string, agentID?: string): Promise<MCPAppResource>;
  /**
   * Route `tools/call` → `mcp.apps.call_tool` (re-enters the tool-safety gates).
   *
   * `tool` is the SERVER-QUALIFIED catalog name (`<serverID>_<name>`) — the
   * caller ({@link createAppHandlers}) qualifies the app-supplied bare name
   * against the bridge's host-derived server id before dispatching, so an app
   * can never name a tool outside its own server. Implementations MUST raise
   * {@link MCPAppToolNotFoundError} when the Runtime reports the name as
   * not-found, so the confinement rejection is distinguishable from a transport
   * failure.
   */
  /** Route an app callback with the host-derived server identity. */
  callTool(serverID: string, tool: string, args?: unknown, agentID?: string): Promise<MCPAppToolResult>;
  /** Route `resources/list` → `mcp.servers.resources`. */
  listResources(serverID: string, agentID?: string): Promise<MCPAppResourceListing[]>;
  /**
   * Route `resources/templates/list` → the server's advertised resource
   * templates. Harbor's Protocol surfaces no resource-template method, so the
   * Console adapter resolves an EMPTY list rather than throwing: the host
   * advertises the `serverResources` capability, and a capability you advertise
   * must be serviceable (the roots-honesty bar) — an app probing for templates
   * gets a truthful "this host exposes none" instead of an error it cannot act
   * on. Full resource-template support is a documented follow-up; the empty
   * answer is the honest state of the surface, not a swallowed failure.
   */
  listResourceTemplates(serverID: string): Promise<MCPAppResourceTemplateListing[]>;
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
  /**
   * Resolve an artifact id to a time-bounded presigned URL the renderer
   * fetches the bytes from (D-026). The MCP Apps host calls this when a
   * `ui://` document is at/above the heavy-content threshold and
   * {@link readResource} returned an {@link MCPAppResource.artifactRef} stub
   * INSTEAD of inline `content` — a routine outcome, since real Svelte/React
   * App bundles are almost always larger than the threshold. The renderer
   * fetches the document bytes from this URL and loads them into the same
   * sandboxed `srcdoc` the inline path uses; only the content SOURCE differs.
   * Maps onto `artifacts.get_ref` (the same read-side resolver every other
   * renderer's presigned `src` flows through).
   */
  resolveArtifact(artifactID: string): Promise<string>;
  /**
   * Fetch the captured tool context (input + lowered result) that produced a
   * rendered app, so the host can deliver it into the app after it initializes
   * (the Data Delivery lifecycle stage). Routes onto `mcp.apps.tool_context`,
   * identity-scoped. The renderer calls this BEFORE mounting the iframe.
   *
   * Returns `null` when no context exists for the `(serverID, toolCallID)`
   * pair — an unknown / evicted / cross-identity id (the adapter maps the
   * Runtime's `not_found` onto `null`). That is the MISS: the renderer mounts
   * NO app and renders an honest "this view is no longer available"
   * placeholder instead of a dataless iframe. Any other failure THROWS (the
   * adapter re-raises a non-`not_found` Protocol error), which the renderer
   * surfaces as its loud error state — neither outcome is ever swallowed
   * (CLAUDE.md §13).
   */
  toolContext(serverID: string, toolCallID: string): Promise<MCPAppToolContext | null>;
  /**
   * Read an artifact's bytes and return them as text — the heavy-payload path
   * for {@link toolContext} and for a heavy app-initiated `tools/call` result.
   * A payload at or above the heavy threshold (D-026) rides by reference; the
   * host reads the bytes here (implemented in the adapter so this module never
   * issues a raw `fetch` — the no-direct-transport invariant, D-173). The text
   * is the tool payload's JSON, parsed/delivered by the host.
   *
   * The implementation MUST reach the bytes on ANY artifact store, not only a
   * presigning one: the Console adapter routes it through `artifacts.get`, the
   * driver-independent byte read (D-353). Implementing it over the optional
   * presign capability instead makes every heavy payload unreadable on four of
   * five drivers, the default included — the gap this contract exists to state.
   *
   * Throws when the bytes cannot be read. Callers surface that as a faithful
   * notice naming the artifact; NONE of them may report it as absent data
   * (fail-loud, CLAUDE.md §13).
   */
  fetchArtifactText(artifactID: string): Promise<string>;
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
   * Effective agent id copied from runtime-authored discovery. Optional only
   * for replay/backward compatibility; the Runtime resolves omission to its
   * configured default and still applies signed reach.
   */
  agentID?: string;
	/** Runtime-issued opaque callback binding for this render. */
	binding?: string;
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
  /**
   * The host identity advertised to the app in the `ui/initialize` handshake
   * (`name` / `version`). Injected through the module seam rather than baked
   * in, so a second framework surface mounting the chat module (the packed dev
   * UI) advertises its own identity. Defaults to {@link DEFAULT_HOST_INFO} —
   * the current Console identity — so an existing caller is unchanged.
   */
  hostInfo?: { name: string; version: string };
  /**
   * The host light/dark preference baked into the app's `McpUiHostContext` at
   * construction, so a rendered app adapts to the host's appearance. Injected
   * through the module seam (the renderer resolves it from OS
   * `prefers-color-scheme`); defaults to `'dark'` — the Console palette — when
   * absent. A LIVE theme change is relayed onto the running bridge via
   * {@link AppBridgeHost.setHostContext}, never by rebuilding the bridge.
   */
  theme?: McpUiTheme;
  /**
   * The host style variables baked into the app's `McpUiHostContext` at
   * construction (the Console `tokens.css` surface projected onto the ext-apps
   * `McpUiStyleVariableKey` namespace). The app-side SDK applies them so its
   * components inherit the host's structural design tokens. Optional; absent
   * means the app falls back to its own defaults.
   */
  styles?: McpUiHostStyles;
  /**
   * The ALREADY-RESOLVED tool context (input + lowered result) the host
   * delivers across the bridge once the app has initialized (the Data
   * Delivery lifecycle stage).
   *
   * The host does NOT fetch it: the renderer resolves it through
   * {@link MCPAppHostClient.toolContext} BEFORE the iframe is mounted, so an
   * unresolvable context never produces a mounted-but-dataless app — the
   * renderer shows an honest placeholder instead and no bridge is ever built.
   * That ordering is what makes the miss path observable rather than silent
   * (CLAUDE.md §13), and it matters most on a REPLAYED app, where the
   * persisted context is the one thing that can have gone away.
   *
   * When unset, the host performs no delivery — the shape a discovery that
   * recorded no correlation id at all takes (nothing was ever captured, so
   * there is no miss to report).
   */
  toolContext?: MCPAppToolContext;
  /**
   * Called once, when the app has completed `ui/initialize` and sent
   * `ui/notifications/initialized`. The renderer uses this to flip a reactive
   * "bridge is live" flag that GATES its live theme relay — so a theme change
   * only patches the bridge AFTER the handshake, never during it. Optional.
   */
  onInitialized?: () => void;
  /**
   * Metadata about the tool call that instantiated this app, projected onto the
   * `ui/initialize` host-context `toolInfo` slot so a rendered app can name the
   * call it belongs to. Supplied by the renderer from the `mcp.app_available`
   * discovery (`toolCallId` + `toolName`); both are host-derived. Optional —
   * absent when the discovery carried no tool name.
   *
   * Baked in at CONSTRUCTION and never patched: it cannot change for the life
   * of a bridge (a different tool call is a different app).
   */
  toolInfo?: { toolCallId?: string; toolName: string };
  /**
   * The dimensions of the container holding the app, projected onto the
   * `ui/initialize` host-context `containerDimensions` slot so an app can lay
   * itself out to the box it was given instead of guessing.
   *
   * Measured ONCE by the renderer, UNTRACKED, at bridge construction. It is
   * deliberately NOT relayed on a later resize: the host resizes the iframe in
   * RESPONSE to the app's own `size-changed` report, so echoing the new box back
   * would close a report→resize→report feedback loop. The value is a snapshot of
   * the box the app was handed, not a live signal.
   */
  containerDimensions?: McpUiHostContext['containerDimensions'];
  /**
   * Called when the app reports its content size (`ui/notifications/size-changed`
   * — the ext-apps view SDK emits it unprompted). The renderer uses it to grow
   * the inline iframe to the app's content height, clamped by the host's own
   * min/max so a misbehaving app cannot seize the viewport. Optional; when
   * absent the notification is simply not consumed and the iframe keeps its
   * fixed height.
   */
  onSizeChanged?: (size: { width?: number; height?: number }) => void;
  /**
   * Called when the app asks to be torn down
   * (`ui/notifications/request-teardown`). The host has already sent the
   * graceful `ui/resource-teardown` and closed the bridge by the time this
   * fires; the renderer uses it to unmount the iframe and render an honest
   * "this app closed itself" placeholder rather than leaving a dead frame.
   * Optional.
   */
  onRequestTeardown?: () => void;
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
  onlistresourcetemplates(): Promise<ListResourceTemplatesResult>;
  onrequestdisplaymode(params: { mode: McpUiDisplayMode }): Promise<{ mode: McpUiDisplayMode }>;
}

/**
 * Qualify an app-supplied tool name into the calling app's own server
 * namespace — the app→host tool-call CONFINEMENT control.
 *
 * Harbor keys its catalog `<source>_<tool>`, so an app naming its server-side
 * tool `get_weather` must reach `srv_get_weather`. Qualifying here does two
 * jobs at once:
 *
 *   1. It RESOLVES the catalog key, so a spec-conformant app (which knows only
 *      its own server-side names) reaches the right tool at all.
 *   2. It CONFINES the app to its own server. The prefix is applied
 *      UNCONDITIONALLY — an already-qualified or cross-server name
 *      (`otherserver_drop_table`) becomes `srv_otherserver_drop_table`, which
 *      cannot resolve. There is no escape: every name the app can utter lands
 *      inside `<serverID>_`.
 *
 * `serverID` is HOST-DERIVED. It arrives on the bridge's construction options
 * from the backend-minted `server_id` on the `mcp.app_available` event; nothing
 * inside the sandboxed iframe can supply or influence it. An app chooses the
 * suffix, never the namespace.
 *
 * This is defence in depth, not the only gate: identity, the tool's approval /
 * OAuth wrappers, and the paused-server / disabled-tool exposure gate all still
 * fire inside `mcp.apps.call_tool`. Confinement narrows WHICH tools are
 * reachable; those gates decide whether a reachable one may run.
 */
export function qualifyAppToolName(serverID: string, name: string): string {
  return `${serverID}_${name}`;
}

/**
 * Build the `ui/initialize` host-context `containerDimensions` from the measured
 * container box. Pure (no DOM) so the mapping is unit-testable; the renderer
 * does the measurement and calls this.
 *
 * The spec's shape is an intersection of two unions — a FIXED or a MAXIMUM for
 * each axis. Harbor's inline host gives an app a fixed WIDTH (the chat column)
 * and a bounded HEIGHT (the app grows into it as it reports its content size,
 * up to the host's maximum), so it emits `{ width, maxHeight }`. A
 * non-positive measurement is omitted rather than sent as zero — a zero box is
 * a lie an app would lay itself out against.
 */
export function containerDimensionsFromBox(box: {
  width: number;
  maxHeight?: number;
}): McpUiHostContext['containerDimensions'] | undefined {
  if (!(box.width > 0)) return undefined;
  const out: { width: number; maxHeight?: number } = { width: box.width };
  if (box.maxHeight !== undefined && box.maxHeight > 0) out.maxHeight = box.maxHeight;
  return out;
}

/**
 * Builds the manual handlers. Every handler dispatches to `opts.client`
 * (the injected Protocol surface) and NOTHING else — no `fetch`, no
 * `WebSocket`, no `EventSource`. The no-direct-transport test drives these
 * with network spies installed and asserts zero network calls.
 */
export function createAppHandlers(opts: AppBridgeHostOptions): AppHandlers {
  const { client, serverID, agentID } = opts;
  return {
    async oncalltool({ name, arguments: args }) {
      // → mcp.apps.call_tool: re-enters the SAME identity + approval-gate +
      //   tool-side-OAuth path a planner call uses. A gated tool parks on the
      //   unified pause primitive (D-173).
      //
      // The name is QUALIFIED into this bridge's host-derived server namespace
      // first (see `qualifyAppToolName`) — the confinement control. An app can
      // only ever reach `<serverID>_*`.
      const qualified = qualifyAppToolName(serverID, name);
      let result: MCPAppToolResult;
      try {
		result = await client.callTool(serverID, qualified, args, agentID, opts.binding);
      } catch (err) {
        if (err instanceof MCPAppToolNotFoundError) {
          // Re-raise naming what the APP asked for (the bare name) and the
          // server it is confined to, so the app can branch on a typed,
          // actionable rejection rather than a generic transport error.
          throw new MCPAppToolNotFoundError(name, serverID);
        }
        throw err;
      }
      if (result.artifactRef) {
        // A heavy result rides by reference (D-026). READ IT: this used to push
        // a bare `[artifact <id> · <n> bytes]` block and return, so an app that
        // called its own tool and got a large answer received prose ABOUT its
        // data instead of the data, with `structuredContent` unset. The read is
        // driver-independent (D-353), so it answers on the default store.
        const ref = result.artifactRef;
        let text: string;
        try {
          text = await fetchArtifactPayload(client, ref);
        } catch (err) {
          if (!(err instanceof MCPAppArtifactUnavailableError)) throw err;
          console.error('MCP App heavy tools/call result could not be read', err);
          return {
            content: [{ type: 'text', text: unavailableArtifactNotice(ref) }],
            isError: result.isError,
          };
        }
        const payload = parsePayloadText(text);
        return {
          content: [{ type: 'text', text: stringifyContent(payload) }],
          structuredContent: asStructured(payload),
          isError: result.isError,
        };
      }
      const out: CallToolResult = {
        content: [{ type: 'text', text: stringifyContent(result.content) }],
        isError: result.isError,
      };
      if (result.content !== undefined && result.content !== null) {
        out.structuredContent = asStructured(result.content);
      }
      return out;
    },

    async onreadresource({ uri }) {
      // → mcp.servers.read_resource, identity-scoped + heavy-content aware.
      const res = await client.readResource(serverID, uri, agentID);
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
      const rows = await client.listResources(serverID, agentID);
      return {
        resources: rows.map((r) => ({ uri: r.uri, name: r.name ?? r.uri, mimeType: r.mimeType })),
      };
    },

    async onlistresourcetemplates() {
      // → the server's advertised resource templates, scoped to THIS bridge's
      //   server. The host advertises `serverResources`, and every advertised
      //   capability must be serviceable: before this handler existed, an app
      //   probing `resources/templates/list` got a "method not found" from a
      //   host that claimed to proxy resource reads. It now answers honestly —
      //   today with the empty list Harbor's Protocol actually exposes.
      const rows = await client.listResourceTemplates(serverID);
      return {
        resourceTemplates: rows.map((r) => ({
          uriTemplate: r.uriTemplate,
          name: r.name ?? r.uriTemplate,
          mimeType: r.mimeType,
        })),
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

/**
 * Read a by-reference payload half's bytes through the INJECTED client, raising
 * the typed {@link MCPAppArtifactUnavailableError} on any failure.
 *
 * Every heavy-payload site funnels through here so the three of them cannot
 * drift into three different ideas of what an unreadable artifact means, and so
 * each of them branches on a TYPE rather than on whatever the client threw.
 */
async function fetchArtifactPayload(
  client: MCPAppHostClient,
  ref: { id: string; sizeBytes?: number },
): Promise<string> {
  try {
    return await client.fetchArtifactText(ref.id);
  } catch (err) {
    throw new MCPAppArtifactUnavailableError(ref.id, ref.sizeBytes, err);
  }
}

/**
 * Interpret a heavy payload's bytes the way the inline path's `content` already
 * arrives: parsed JSON when they are JSON, the raw string when they are not.
 *
 * The fallback is not a swallowed failure — a payload that is not JSON IS a
 * string payload, and `content: "..."` is exactly what the inline branch would
 * carry for one. It keeps the two branches representing one payload identically
 * instead of letting SIZE decide the shape an app receives.
 */
function parsePayloadText(text: string): unknown {
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

/**
 * The faithful notice delivered in place of a heavy payload whose bytes could
 * not be read: it names the artifact and its size rather than rendering as
 * empty or absent data (fail-loud, CLAUDE.md §13).
 */
function unavailableArtifactNotice(ref: { id: string; sizeBytes?: number }): string {
  const size = ref.sizeBytes ? ` · ${ref.sizeBytes} bytes` : '';
  return `[artifact ${ref.id}${size} — could not be read]`;
}

/**
 * The default handshake host identity advertised in `ui/initialize` — the
 * Console's identity. Used when {@link AppBridgeHostOptions.hostInfo} is not
 * supplied, so the Console caller is unchanged while a second framework surface
 * can inject its own identity through the seam.
 */
export const DEFAULT_HOST_INFO = { name: 'harbor-console', version: '1' } as const;

/**
 * How long the host waits for an app to acknowledge the graceful
 * `ui/resource-teardown` before closing the transport regardless. Short on
 * purpose: the teardown is a courtesy on the unmount path, and a wedged app must
 * never hold a Svelte effect cleanup open.
 */
export const APP_TEARDOWN_TIMEOUT_MS = 1_000;

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
  readonly #client: MCPAppHostClient;
  // NOTE: the server id is consumed by `createAppHandlers` (which scopes every
  // app→host read to it); the wrapper itself no longer needs a copy now that
  // the tool context arrives already resolved.
  readonly #toolContext: MCPAppToolContext | undefined;
  readonly #onInitialized: (() => void) | undefined;
  readonly #onRequestTeardown: (() => void) | undefined;
  #transport: PostMessageTransport | undefined;
  // Set the instant `close()` is called, BEFORE any await. It is the flag a
  // still-running `connect()` checks so a close that raced the handshake is not
  // lost — `#connected` alone cannot express "closed before it ever connected".
  #closing = false;
  #displayModeRequests: DisplayModeRequest[] = [];
  #connected = false;
  #initialized = false;

  constructor(opts: AppBridgeHostOptions) {
    this.#handlers = createAppHandlers({
      ...opts,
      onDisplayModeRequest: (req) => {
        this.#displayModeRequests.push(req);
        opts.onDisplayModeRequest?.(req);
      },
    });
    this.#client = opts.client;
    this.#toolContext = opts.toolContext;
    this.#onInitialized = opts.onInitialized;
    this.#onRequestTeardown = opts.onRequestTeardown;

    // Host identity + theme are injected through the seam; both default to the
    // Console's values so an existing caller is unchanged.
    const hostInfo = opts.hostInfo ?? DEFAULT_HOST_INFO;
    const theme: McpUiTheme = opts.theme ?? 'dark';

    const available = opts.availableDisplayModes ?? (['inline'] as McpUiDisplayMode[]);
    // Construct the bridge ONCE with the FINAL host-context — theme + the host
    // style variables baked in. It is NEVER rebuilt for a theme/data change: a
    // theme change patches this live bridge via `setHostContext`; a
    // reconstruction mid-handshake was the reverted-work handshake break.
    const hostContext: McpUiHostContext = {
      theme,
      // The app boots inline (in the chat scroll); fullscreen / pip are reached
      // via a `ui/request-display-mode` the page's layout machine applies.
      displayMode: 'inline',
      availableDisplayModes: available,
    };
    if (opts.styles) {
      hostContext.styles = opts.styles;
    }
    if (opts.toolInfo) {
      // The spec shape: `{ id?: RequestId, tool: Tool }`. Harbor knows the tool
      // NAME (carried on the discovery event) but not the server's full tool
      // definition at render time, so the minimum valid `Tool` is emitted — the
      // name plus the empty object schema the type requires. Naming the call is
      // the point; re-deriving its schema is not.
      hostContext.toolInfo = {
        id: opts.toolInfo.toolCallId,
        tool: { name: opts.toolInfo.toolName, inputSchema: { type: 'object' } },
      };
    }
    if (opts.containerDimensions) {
      hostContext.containerDimensions = opts.containerDimensions;
    }

    // The load-bearing line: the first argument is `null`. The AppBridge is
    // NEVER handed an MCP Client, so it can never auto-forward to a direct MCP
    // transport (D-173).
    this.#bridge = new AppBridge(null, hostInfo, hostCapabilities(), { hostContext });

    this.#bridge.oncalltool = (params) => this.#handlers.oncalltool(params);
    this.#bridge.onreadresource = (params) => this.#handlers.onreadresource(params);
    this.#bridge.onlistresources = () => this.#handlers.onlistresources();
    this.#bridge.onlistresourcetemplates = () => this.#handlers.onlistresourcetemplates();
    this.#bridge.onrequestdisplaymode = (params) => this.#handlers.onrequestdisplaymode(params);
    // The app reports its rendered content size. Purely a host→renderer relay:
    // the notification arrives after the handshake, carries no authority, and
    // never touches the bridge lifecycle. The renderer owns the clamp.
    this.#bridge.onsizechange = (params) => {
      opts.onSizeChanged?.({ width: params.width, height: params.height });
    };
    // The app asks to be torn down. The host DECIDES (per the spec the host may
    // decline); Harbor grants it: send the graceful `ui/resource-teardown`,
    // close the bridge, then tell the renderer to unmount. `close()` gates the
    // teardown send on `#initialized`, so this can never fire mid-handshake.
    this.#bridge.onrequestteardown = () => {
      void this.close().finally(() => {
        this.#onRequestTeardown?.();
      });
    };
    this.#bridge.oninitialized = () => {
      this.#initialized = true;
      // The renderer flips its "bridge is live" gate here, so its live theme
      // relay only fires AFTER the handshake completes.
      this.#onInitialized?.();
      // Data Delivery: once the app has sent `ui/notifications/initialized`,
      // deliver the originating tool's input + result into it. Best-effort and
      // fire-and-forget — a delivery failure is logged, never thrown (the app
      // already rendered its shell).
      void this.#deliverToolContext();
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
    if (this.#connected || this.#closing) return;
    this.#transport = new PostMessageTransport(iframeWindow, iframeWindow);
    await this.#bridge.connect(this.#transport);
    // `close()` can land DURING the await above — the renderer's effect cleanup
    // does not await `connect`, so an unmount racing a slow handshake reaches
    // `close()` while `#connected` is still false, which the old `close()`
    // early-returned on. The bridge then finished connecting into an orphan
    // with a live postMessage listener nobody would ever close. Honour the
    // close that already happened instead of publishing the connection.
    if (this.#closing) {
      await this.#bridge.close();
      return;
    }
    this.#connected = true;
  }

  /** True once the app has sent `ui/notifications/initialized`. */
  get isInitialized(): boolean {
    return this.#initialized;
  }

  /**
   * Patch the LIVE bridge's host-context (→ a `host-context-changed`
   * notification the app's SDK consumes). The relay for a host theme change:
   * the renderer calls this from a SEPARATE effect that no-ops until the bridge
   * reports initialized. This method also GATES on `#initialized` as a
   * belt-and-braces second factor — a patch that raced ahead of the handshake
   * is dropped rather than posted onto a transport the app has not finished
   * `ui/initialize` on. It NEVER tears down or rebuilds the bridge.
   */
  setHostContext(patch: McpUiHostContext): void {
    if (!this.#initialized) return;
    this.#bridge.setHostContext(patch);
  }

  /**
   * Deliver the ALREADY-RESOLVED tool context into the app — `input` via
   * `sendToolInput`, then `result` via `sendToolResult` (in that ORDER: the
   * SDK requires `initialized` before `sendToolResult`, and input-then-result
   * is the lifecycle order). No fetch happens here: the renderer resolved the
   * context before it mounted the iframe, so an unresolvable context never
   * reaches a constructed bridge (it renders the placeholder instead). An
   * absent context therefore means "nothing was ever captured", not "the
   * lookup failed" — no delivery, and nothing to report.
   *
   * The sends themselves stay best-effort: a heavy by-reference half is
   * fetched at the iframe edge between them, and a failure is logged but never
   * propagated (the app has already rendered — fail-safe on the delivery, not
   * the render). The injected client is the ONLY path used — no direct
   * transport (D-173).
   */
  async #deliverToolContext(): Promise<void> {
    const ctx = this.#toolContext;
    if (!ctx) return;
    try {
      // Re-check liveness around EVERY send: a transcript re-render can
      // `close()` this bridge while a heavy by-reference half is being fetched
      // at the iframe edge, swapping the transport out from under us. Sending
      // onto a torn-down transport throws "Not connected" — a stale delivery
      // for a bridge that no longer exists, not a real failure. Bail silently
      // instead; the replacement bridge runs its own delivery on its own
      // `oninitialized`.
      let args: Record<string, unknown> | undefined;
      try {
        args = await this.#payloadToArgs(ctx.input);
      } catch (err) {
        if (!(err instanceof MCPAppArtifactUnavailableError)) throw err;
        // The input's bytes could not be read. The `ui/notifications/tool-input`
        // params make `arguments` OPTIONAL, and sending it empty would state
        // that the tool ran with none — a claim the host cannot make. So the
        // notification is WITHHELD and the failure is reported loudly here,
        // while the result delivery below proceeds: the result is the half an
        // app most needs, and losing the input must not cost it that too.
        console.error(
          'MCP App tool-input artifact could not be read; withholding the tool-input ' +
            'notification rather than reporting an empty argument map',
          err,
        );
      }
      if (!this.#connected || !this.#initialized) return;
      if (args !== undefined) {
        await this.#bridge.sendToolInput({ arguments: args });
      }
      const result = await this.#payloadToResult(ctx.result, ctx.isError);
      if (!this.#connected || !this.#initialized) return;
      await this.#bridge.sendToolResult(result);
    } catch (err) {
      // The delivery is best-effort — surface the failure to the console but
      // never throw (the app already rendered; a delivery error is not a render
      // error).
      console.error('MCP App tool-context delivery failed', err);
    }
  }

  /**
   * Build the `sendToolInput` arguments from a captured input payload. Inline
   * content is coerced to a record; a heavy by-reference payload is read +
   * JSON-parsed at the iframe edge (the bytes are the input JSON).
   *
   * An unreadable by-reference input RAISES {@link MCPAppArtifactUnavailableError}
   * rather than degrading to `{}`. It used to swallow the failure into an empty
   * map, whose JSDoc called that "a faithful 'no input'" — but "the read failed"
   * and "the tool ran with no arguments" are different facts, and the empty map
   * asserts the second one (CLAUDE.md §13). `{}` is now returned by exactly one
   * branch: the one where the capture genuinely holds no input.
   */
  async #payloadToArgs(p: MCPAppToolContextPayload): Promise<Record<string, unknown>> {
    if (p.content !== undefined) {
      return asStructured(p.content);
    }
    if (p.artifactRef) {
      const ref = p.artifactRef;
      const text = await fetchArtifactPayload(this.#client, ref);
      try {
        return asStructured(JSON.parse(text) as unknown);
      } catch (err) {
        // The bytes arrived but are not the input JSON they were promised to
        // be. Same verdict, same type: the app is not told there was no input.
        throw new MCPAppArtifactUnavailableError(ref.id, ref.sizeBytes, err);
      }
    }
    return {};
  }

  /**
   * Build the `sendToolResult` `CallToolResult` from a captured result payload.
   *
   * BOTH delivering branches carry the same field set — a text block AND
   * `structuredContent`. The by-reference branch used to omit
   * `structuredContent`, so one and the same tool result reached an app as
   * structured data or as bare text depending only on its SIZE, and an app
   * rendering off `structuredContent` (the normal case for a rich view) got
   * nothing the moment its data crossed the heavy threshold.
   *
   * When the heavy bytes cannot be read, the host delivers a FAITHFUL notice
   * block naming the artifact — never silently empty (fail-loud), and
   * deliberately WITHOUT `structuredContent`, so its absence keeps meaning
   * "there is no data here" rather than becoming a shape an app must
   * distinguish from real data.
   */
  async #payloadToResult(
    p: MCPAppToolContextPayload,
    isError: boolean,
  ): Promise<CallToolResult> {
    if (p.content !== undefined) {
      return {
        content: [{ type: 'text', text: stringifyContent(p.content) }],
        structuredContent: asStructured(p.content),
        isError,
      };
    }
    if (p.artifactRef) {
      const ref = p.artifactRef;
      let text: string;
      try {
        text = await fetchArtifactPayload(this.#client, ref);
      } catch (err) {
        if (!(err instanceof MCPAppArtifactUnavailableError)) throw err;
        console.error('MCP App heavy tool result could not be read', err);
        return {
          content: [{ type: 'text', text: unavailableArtifactNotice(ref) }],
          isError,
        };
      }
      const payload = parsePayloadText(text);
      return {
        content: [{ type: 'text', text: stringifyContent(payload) }],
        structuredContent: asStructured(payload),
        isError,
      };
    }
    return { content: [{ type: 'text', text: '' }], isError };
  }

  /**
   * Tears the bridge down: sends the graceful `ui/resource-teardown` the app
   * gets to react to, THEN closes the transport and drops the iframe peer.
   *
   * Three properties make the teardown handshake-safe — the failure class that
   * got the original MCP-Apps Console work reverted:
   *
   *   1. **Never mid-handshake.** The teardown request is sent ONLY when the app
   *      has reported `ui/notifications/initialized`. A bridge closed before the
   *      handshake completes (a stale preload losing its generation race, an app
   *      that never boots) closes silently, exactly as it did before — a request
   *      posted onto a transport the app has not finished `ui/initialize` on is
   *      the shape that produced the 30s timeout.
   *   2. **Idempotent, and it drops the liveness flags FIRST.** `#connected` is
   *      cleared before anything is awaited, so a concurrent tool-context
   *      delivery observes a dead bridge at its next re-check and bails, and a
   *      second `close()` (unmount racing an app-requested teardown) is a no-op.
   *   3. **Bounded and best-effort.** The request is a round-trip to code inside
   *      a sandboxed iframe that may be wedged or already gone, so it carries a
   *      short timeout and a failure is logged, never thrown: the unmount MUST
   *      proceed. Closing the transport is the guarantee; the graceful notice is
   *      the courtesy.
   */
  async close(): Promise<void> {
    if (this.#closing) return;
    // Drop liveness BEFORE the first await (see property 2 above). `#closing`
    // is set even when the bridge never finished connecting, so a `connect()`
    // still in flight observes it and closes the transport it just built
    // instead of leaving a live orphan.
    this.#closing = true;
    if (!this.#connected) return;
    this.#connected = false;
    const wasInitialized = this.#initialized;
    this.#initialized = false;
    if (wasInitialized) {
      try {
        await this.#bridge.teardownResource({}, { timeout: APP_TEARDOWN_TIMEOUT_MS });
      } catch (err) {
        // A wedged / already-navigated-away app must not block the unmount.
        console.warn('MCP App resource-teardown failed; closing anyway', err);
      }
    }
    await this.#bridge.close();
  }
}
