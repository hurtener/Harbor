// MCP Connections page reactive state (Phase 108m / D-185 — Svelte 5
// runes mode, D-092).
//
// This module owns the page's reactive state; the `.svelte` components read
// it and call its actions, never touching the Protocol client directly
// (CONVENTIONS.md §6). It is the king-file refactor: the `McpListState`
// servers-catalog controller + the deepened `McpDetailState` per-server
// detail controller + the pure `derive.ts` projections + the focused
// `ServersTable` / `McpDetailRail` / `McpOverviewCard` components replace the
// pre-chrome list-page + separate tabbed-detail route (the Phase 73k / D-119
// layout this supersedes).
//
// The page is a PURE Protocol consumer — it composes ONLY the already-shipped
// `mcp.servers.*` surface (Phase 73k / D-119) + the shipped `tools.list`
// (the per-server Tools tab + recent-event attribution) + the shipped
// `events.subscribe` SSE (the live Recent-events card). No new Protocol
// method (§13).
//
//   - `connection.ts` returns `null` when the Console is not attached to a
//     Runtime — that is the Disconnected state, DISTINCT from Error
//     (CONVENTIONS.md §4/§8).
//   - every `mcp.servers.*` call routes through `client.mcp.servers.*`, the
//     single `fetch` choke point, raising the one `ProtocolError` carrying
//     `(code, message, status)`.
//
// Fields an async load assigns AFTER first render (the list response, the
// detail header, the tabs, the live event page) are `$state` so the reactive
// re-read fires — the Events-page D-180 lesson.

import {
  resolveConnection,
  hasScope,
  type RuntimeConnection
} from '$lib/connection.js';
import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
import { EventsSubscription } from '$lib/events/subscription.svelte.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import {
  toPageError,
  displayStatus,
  serverStateCounts,
  projectServerEvents,
  MCP_RECENT_EVENT_TYPES,
  type PageError,
  type ServerStateCounts,
  type RecentEventEntry
} from '$lib/mcp-connections/derive.js';
import type { Tool, ToolListResponse } from '$lib/protocol/tools.js';
import type {
  MCPServerView,
  MCPListFilter,
  MCPServerState,
  MCPServersListResponse,
  MCPServerGetResponse,
  MCPServerResourcesResponse,
  MCPServerPromptsResponse,
  MCPServerPolicyResponse,
  MCPServerBindingsListResponse,
  MCPServerRefreshDiscoveryResponse,
  MCPServerProbeResponse,
  MCPServerRefreshBindingResponse,
  MCPServerRevokeBindingResponse,
  MCPServerSetRawHTMLTrustResponse,
  MCPToolPolicyView,
  MCPBindingScopeCount,
  MCPResourceView,
  MCPPromptView,
  MCPBindingView
} from '$lib/protocol/mcp.js';

/** The default page size for the servers list. */
export const DEFAULT_PAGE_SIZE = 25;

/**
 * Builds a `HarborClient` from the resolved Runtime connection, or returns
 * `null` when the Console is not attached. A `null` is the honest "no
 * Runtime" signal — the caller renders `PageState`'s Disconnected state,
 * never an Error (CONVENTIONS.md §8).
 */
function buildClient(injected?: ProtocolClient): {
  client: ProtocolClient | null;
  connection: RuntimeConnection | null;
} {
  const connection = resolveConnection();
  if (connection === null) {
    return { client: null, connection: null };
  }
  return { client: injected ?? new HarborClient({ connection }), connection };
}

/**
 * McpListState owns the servers-list view. It exposes a `PageStatus`
 * (CONVENTIONS.md §4 four-state contract) the `<PageState>` boundary
 * consumes, plus the page-size / page / total pagination model.
 */
export class McpListState {
  /** The four-state async status the `<PageState>` boundary reads. */
  status = $state<PageStatus>('loading');
  /** The thrown error — populated only in the `error` status. */
  error = $state<PageError | null>(null);
  /** The loaded server rows (suppressed while in `error`). */
  servers = $state<MCPServerView[]>([]);
  /** The applied facet filter (also persisted by saved-view chips). */
  filter = $state<MCPListFilter>({});
  /** 1-based current page. */
  page = $state(1);
  /** Page size. */
  pageSize = $state(DEFAULT_PAGE_SIZE);
  /** Total matched-row count across all pages. */
  total = $state(0);
  /** The free-text search term (Console-side filter, page-mcp §3). */
  search = $state('');
  /** The applied saved-view id, or null. */
  activeSavedViewId = $state<string | null>(null);

  /** Phase 83r disconnected predicate — drives the disabled-with-tooltip. */
  get disconnected(): boolean {
    return this.status === 'disconnected';
  }

  /** The search-narrowed projection of `servers`. */
  get visibleServers(): MCPServerView[] {
    const q = this.search.trim().toLowerCase();
    if (q === '') {
      return this.servers;
    }
    return this.servers.filter(
      (s) =>
        s.name.toLowerCase().includes(q) ||
        s.url_or_command.toLowerCase().includes(q)
    );
  }

  /** The display status — ready/empty derived live from the visible rows. */
  get displayStatus(): PageStatus {
    return displayStatus(this.status, this.visibleServers.length);
  }

  /** Per-state roll-up for the idle catalog-overview card. */
  get overview(): ServerStateCounts {
    return serverStateCounts(this.servers);
  }

  /** load fetches the server list applying the current filter + page. */
  async load(): Promise<void> {
    const { client } = buildClient();
    if (client === null) {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.error = null;
    try {
      const resp = await client.mcp.servers.list<MCPServersListResponse>({
        ...this.filter,
        page: this.page,
        page_size: this.pageSize
      });
      this.servers = resp.servers;
      this.total = resp.total ?? resp.servers.length;
      this.status = resp.servers.length === 0 ? 'empty' : 'ready';
    } catch (e) {
      this.servers = [];
      this.error = toPageError(e);
      this.status = 'error';
    }
  }

  /** setStateFilter narrows the list to a single state chip (or clears). */
  setStateFilter(state: MCPServerState | null): void {
    this.activeSavedViewId = null;
    this.filter = state
      ? { ...this.filter, state: [state] }
      : { ...this.filter, state: undefined };
    this.page = 1;
    void this.load();
  }

  /** The currently-applied single state facet, or null. */
  get activeStateFilter(): MCPServerState | null {
    return this.filter.state?.[0] ?? null;
  }

  /** applyFilter replaces the whole filter (used by saved-view chips). */
  applyFilter(filter: MCPListFilter, savedViewId: string | null = null): void {
    this.filter = { ...filter };
    this.activeSavedViewId = savedViewId;
    this.page = 1;
    void this.load();
  }

  /** setSearch narrows the visible rows Console-side; no Protocol call. */
  setSearch(term: string): void {
    this.search = term;
  }

  /** clearFilters drops the facet + search and reloads. */
  clearFilters(): void {
    this.search = '';
    this.activeSavedViewId = null;
    this.filter = {};
    this.page = 1;
    void this.load();
  }

  /** goToPage requests a new 1-based page and re-loads. */
  goToPage(page: number): void {
    this.page = page;
    void this.load();
  }

  /** setPageSize changes the page size and re-loads from page 1. */
  setPageSize(size: number): void {
    this.pageSize = size;
    this.page = 1;
    void this.load();
  }
}

/** A page detail tab (page-mcp-connections.md §4 — five tabs). */
export type McpDetailTab = 'tools' | 'resources' | 'prompts' | 'oauth' | 'policy';

/** The five detail tabs, in mockup order. */
export const MCP_DETAIL_TABS: { id: McpDetailTab; label: string }[] = [
  { id: 'tools', label: 'Tools' },
  { id: 'resources', label: 'Resources' },
  { id: 'prompts', label: 'Prompts' },
  { id: 'oauth', label: 'OAuth bindings' },
  { id: 'policy', label: 'Policy' }
];

/** The honest result of a control-plane action (refresh / probe). */
export interface ActionResult {
  ok: boolean;
  message: string;
}

/**
 * McpDetailState owns the per-server detail surface — the right rail of the
 * master-detail page (CONVENTIONS.md §5 / the Tools-108k pattern). It wires
 * the header (`mcp.servers.get`), the five tabs (`tools.list` filtered to the
 * server, `resources`, `prompts`, `bindings.list`, `policy`), the control
 * actions (`refresh_discovery`, `probe`, `set_raw_html_trust`,
 * `refresh_binding`, `revoke_binding`), and the LIVE Recent-events card
 * (`events.subscribe`). Every action reports the REAL runtime outcome — a
 * probe shows reachable/error, a refresh shows the re-read counts; nothing is
 * fabricated (CLAUDE.md §13).
 */
export class McpDetailState {
  /* ---- connection + admin gate ----------------------------------- */
  connection = $state<RuntimeConnection | null>(null);
  /** True when the connection carries the `admin` claim (D-079) — drives the
   * disabled-state affordance on the raw-HTML toggle + OAuth admin actions.
   * The server is always the authoritative gate (scope_mismatch). */
  isAdmin = $state(false);
  get disconnected(): boolean {
    return this.connection === null;
  }

  /* ---- header (mcp.servers.get) ---------------------------------- */
  status = $state<PageStatus>('loading');
  error = $state<PageError | null>(null);
  server = $state<MCPServerView | null>(null);
  displayModes = $state<string[]>([]);
  contentShapes = $state<string[]>([]);
  toolPolicy = $state<MCPToolPolicyView | null>(null);
  bindingsSummary = $state<MCPBindingScopeCount[]>([]);

  /* ---- tabs ------------------------------------------------------- */
  activeTab = $state<McpDetailTab>('tools');
  tools = $state<Tool[]>([]);
  toolsStatus = $state<PageStatus>('loading');
  resources = $state<MCPResourceView[]>([]);
  resourcesLoaded = $state(false);
  prompts = $state<MCPPromptView[]>([]);
  promptsLoaded = $state(false);
  bindings = $state<MCPBindingView[]>([]);
  bindingsLoaded = $state(false);

  /* ---- live recent events (events.subscribe — page-wide) --------- */
  subscription = $state<EventsSubscription | null>(null);

  /* ---- control-action results (honest — §13) --------------------- */
  actionBusy = $state<string | null>(null);
  probeResult = $state<{ ok: boolean; latency_ms: number; error?: string } | null>(null);
  refreshResult = $state<ActionResult | null>(null);
  rawHtmlResult = $state<ActionResult | null>(null);
  lastActionError = $state<PageError | null>(null);

  #client: ProtocolClient | null = null;
  #name = '';

  /* ================================================================ */
  /* Derived projections                                               */
  /* ================================================================ */

  /** The server name this controller is scoped to. */
  get serverName(): string {
    return this.#name;
  }

  /** The set of tool names this server owns — used for event attribution. */
  get serverToolNames(): Set<string> {
    return new Set(this.tools.map((t) => t.name));
  }

  /** The live recent-event rows attributed to this server, newest-first. */
  get recentEvents(): RecentEventEntry[] {
    return projectServerEvents(this.subscription?.events ?? [], this.#name, this.serverToolNames);
  }

  /** The SSE stream state — drives the Recent-events live dot. */
  get streamState(): string {
    return this.subscription?.state ?? 'idle';
  }

  /* ================================================================ */
  /* Boot — open the page-wide live subscription once                  */
  /* ================================================================ */

  /**
   * Resolves the connection and opens the live Recent-events subscription
   * once. The PAGE calls this in `onMount` (browser-only); unit tests call
   * {@link load} directly and never construct an `EventSource`.
   */
  boot(injected?: ProtocolClient): void {
    const { client, connection } = buildClient(injected);
    this.connection = connection;
    this.isAdmin = hasScope(connection, 'admin');
    if (client === null) {
      this.#client = null;
      return;
    }
    this.#client = client;
    if (this.subscription === null) {
      const sub = new EventsSubscription(this.#client.events);
      sub.open({ eventTypes: MCP_RECENT_EVENT_TYPES as string[] });
      this.subscription = sub;
    }
  }

  /* ================================================================ */
  /* Select a server — load header + tabs                              */
  /* ================================================================ */

  /** load selects a server: resets the per-server caches, loads the header
   * (`mcp.servers.get`) + the server's tools (`tools.list`). */
  async load(name: string, injected?: ProtocolClient): Promise<void> {
    this.#name = name;
    this.#resetCaches();
    const { client, connection } = buildClient(injected);
    this.connection = connection;
    this.isAdmin = hasScope(connection, 'admin');
    if (client === null) {
      this.#client = null;
      this.status = 'disconnected';
      this.toolsStatus = 'disconnected';
      return;
    }
    this.#client = client;
    this.status = 'loading';
    this.error = null;
    try {
      const detail = await this.#client.mcp.servers.get<MCPServerGetResponse>(name);
      this.server = detail.server;
      this.displayModes = detail.display_modes_advertised;
      this.contentShapes = detail.content_shapes;
      this.toolPolicy = detail.tool_policy;
      this.bindingsSummary = detail.bindings_summary;
      this.status = 'ready';
    } catch (e) {
      this.server = null;
      this.error = toPageError(e);
      this.status = 'error';
      this.toolsStatus = 'error';
      return;
    }
    void this.loadTools(name);
  }

  /** Clears the rail back to the idle catalog-overview state. */
  clear(): void {
    this.#name = '';
    this.#resetCaches();
    this.server = null;
    this.status = 'loading';
    this.error = null;
  }

  #resetCaches(): void {
    this.activeTab = 'tools';
    this.tools = [];
    this.toolsStatus = 'loading';
    this.resources = [];
    this.resourcesLoaded = false;
    this.prompts = [];
    this.promptsLoaded = false;
    this.bindings = [];
    this.bindingsLoaded = false;
    this.bindingsSummary = [];
    this.probeResult = null;
    this.refreshResult = null;
    this.rawHtmlResult = null;
    this.lastActionError = null;
  }

  /** loadTools fetches the server's tools (`tools.list` filtered on owner). */
  async loadTools(name: string): Promise<void> {
    if (this.#client === null) {
      this.toolsStatus = 'disconnected';
      return;
    }
    this.toolsStatus = 'loading';
    try {
      const resp = await this.#client.tools.list<ToolListResponse>({}, 1, 200);
      // MCP-sourced tools carry `owner === <server name>` (verified live —
      // the youtube server's tools all report owner=youtube). Attribute by
      // owner, never by a name-prefix heuristic.
      this.tools = (resp.tools ?? []).filter((t) => t.owner === name);
      this.toolsStatus = this.tools.length === 0 ? 'empty' : 'ready';
    } catch (e) {
      this.tools = [];
      this.lastActionError = toPageError(e);
      this.toolsStatus = 'error';
    }
  }

  /** selectTab switches the active detail tab and lazily loads its data. */
  async selectTab(tab: McpDetailTab): Promise<void> {
    this.activeTab = tab;
    const name = this.#name;
    if (this.#client === null || name === '') {
      return;
    }
    try {
      if (tab === 'resources' && !this.resourcesLoaded) {
        const r = await this.#client.mcp.servers.resources<MCPServerResourcesResponse>(name);
        this.resources = r.resources;
        this.resourcesLoaded = true;
      } else if (tab === 'prompts' && !this.promptsLoaded) {
        const p = await this.#client.mcp.servers.prompts<MCPServerPromptsResponse>(name);
        this.prompts = p.prompts;
        this.promptsLoaded = true;
      } else if (tab === 'oauth' && !this.bindingsLoaded) {
        const b = await this.#client.mcp.servers.bindings<MCPServerBindingsListResponse>(name);
        this.bindings = b.bindings;
        this.bindingsLoaded = true;
      } else if (tab === 'policy' && this.toolPolicy === null) {
        const pol = await this.#client.mcp.servers.policy<MCPServerPolicyResponse>(name);
        this.toolPolicy = pol.tool_policy;
      }
    } catch (e) {
      this.lastActionError = toPageError(e);
    }
  }

  /* ================================================================ */
  /* Control actions — the SHIPPED mcp.servers.* verbs (§13 / D-066)   */
  /* ================================================================ */

  /** refreshDiscovery re-runs the server's discovery, then re-loads so the
   * rendered counts reflect the runtime. Reports the honest re-read counts. */
  async refreshDiscovery(): Promise<void> {
    const name = this.#name;
    if (this.#client === null || name === '') return;
    this.actionBusy = 'refresh';
    this.refreshResult = null;
    this.lastActionError = null;
    try {
      const resp =
        await this.#client.mcp.servers.refreshDiscovery<MCPServerRefreshDiscoveryResponse>(name);
      this.refreshResult = {
        ok: true,
        message: `Discovery refreshed — ${resp.tool_count} tool(s), ${resp.resource_count} resource(s), ${resp.prompt_count} prompt(s).`
      };
      await this.load(name);
    } catch (e) {
      const err = toPageError(e);
      this.lastActionError = err;
      this.refreshResult = { ok: false, message: `${err.code}: ${err.message}` };
    } finally {
      this.actionBusy = null;
    }
  }

  /** probe runs a transport test-connection and surfaces the REAL outcome
   * (reachable + latency, or the transport error) — never a faked "OK". */
  async probe(): Promise<void> {
    const name = this.#name;
    if (this.#client === null || name === '') return;
    this.actionBusy = 'probe';
    this.probeResult = null;
    this.lastActionError = null;
    try {
      const resp = await this.#client.mcp.servers.probe<MCPServerProbeResponse>(name);
      this.probeResult = { ok: resp.ok, latency_ms: resp.latency_ms, error: resp.error };
    } catch (e) {
      const err = toPageError(e);
      this.lastActionError = err;
      this.probeResult = { ok: false, latency_ms: 0, error: `${err.code}: ${err.message}` };
    } finally {
      this.actionBusy = null;
    }
  }

  /** setRawHTMLTrust flips the per-server raw-HTML opt-in flag (admin-gated,
   * audited). The server re-read reflects the new trust; no fabricated OK. */
  async setRawHTMLTrust(trusted: boolean): Promise<void> {
    const name = this.#name;
    if (this.#client === null || name === '' || !this.isAdmin) return;
    this.actionBusy = 'raw-html';
    this.rawHtmlResult = null;
    this.lastActionError = null;
    try {
      const resp = await this.#client.mcp.servers.setRawHTMLTrust<MCPServerSetRawHTMLTrustResponse>(
        name,
        trusted
      );
      this.rawHtmlResult = {
        ok: true,
        message: `Raw-HTML trust for ${resp.name} is now ${resp.trusted ? 'trusted' : 'default-deny'}.`
      };
      await this.load(name);
    } catch (e) {
      const err = toPageError(e);
      this.lastActionError = err;
      this.rawHtmlResult = { ok: false, message: `${err.code}: ${err.message}` };
    } finally {
      this.actionBusy = null;
    }
  }

  /** connectBinding initiates an OAuth (re)connect flow and opens the
   * runtime-provided AuthorizeURL in a popup. The Console never sees
   * plaintext tokens — the runtime closes the flow via pause/resume. */
  async connectBinding(principalId: string): Promise<void> {
    const name = this.#name;
    if (this.#client === null || name === '' || !this.isAdmin) return;
    this.lastActionError = null;
    try {
      const resp =
        await this.#client.mcp.servers.refreshBinding<MCPServerRefreshBindingResponse>(
          name,
          principalId
        );
      if (typeof window !== 'undefined' && resp.authorize_url) {
        window.open(resp.authorize_url, '_blank', 'width=600,height=720');
      }
    } catch (e) {
      this.lastActionError = toPageError(e);
    }
  }

  /** revokeBinding revokes an OAuth binding and refreshes the bindings tab. */
  async revokeBinding(principalId: string): Promise<void> {
    const name = this.#name;
    if (this.#client === null || name === '' || !this.isAdmin) return;
    this.lastActionError = null;
    try {
      await this.#client.mcp.servers.revokeBinding<MCPServerRevokeBindingResponse>(
        name,
        principalId
      );
      const b = await this.#client.mcp.servers.bindings<MCPServerBindingsListResponse>(name);
      this.bindings = b.bindings;
      this.bindingsLoaded = true;
    } catch (e) {
      this.lastActionError = toPageError(e);
    }
  }

  /** Closes the live subscription on page unmount. */
  close(): void {
    this.subscription?.close();
    this.subscription = null;
  }
}
