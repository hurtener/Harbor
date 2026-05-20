// MCP Connections page reactive state (D-121, MCP refactor — Svelte 5
// runes mode, D-092).
//
// This module owns the page's reactive state; the `.svelte` components
// read it and call its actions, never touching the Protocol client
// directly. The D-121 refactor migrates the page off the legacy `mcpApi`
// object onto the unified `HarborClient` + `connection.ts`:
//
//   - `connection.ts` returns `null` when the Console is not attached to
//     a Runtime — that is the Disconnected state, DISTINCT from Error
//     (CONVENTIONS.md §4/§8). The legacy state machine had no
//     Disconnected branch; this one does.
//   - every `mcp.servers.*` call routes through `client.mcp.servers.*`,
//     the single `fetch` choke point, and rejects with the one
//     `ProtocolError` carrying `(code, message, status)` — the status is
//     never dropped (the legacy `ProtocolCallError` dropped it).

import { resolveConnection } from '$lib/connection.js';
import { HarborClient } from '$lib/protocol/harbor.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type {
  MCPServerView,
  MCPListFilter,
  MCPServerState,
  MCPServersListResponse,
  MCPServerGetResponse,
  MCPServerResourcesResponse,
  MCPServerPromptsResponse,
  MCPServerHealthResponse,
  MCPServerPolicyResponse,
  MCPServerBindingsListResponse,
  MCPServerRefreshDiscoveryResponse,
  MCPServerProbeResponse,
  MCPServerRefreshBindingResponse,
  MCPServerRevokeBindingResponse,
  MCPServerSetRawHTMLTrustResponse,
  MCPToolPolicyView,
  MCPResourceView,
  MCPPromptView,
  MCPBindingView
} from '$lib/protocol/mcp.js';

/** The default page size for the servers list. */
export const DEFAULT_PAGE_SIZE = 25;

/**
 * Builds a `HarborClient` from the resolved Runtime connection, or returns
 * `null` when the Console is not attached. A `null` is the honest
 * "no Runtime" signal — the caller renders `PageState`'s Disconnected
 * state, never an Error (CONVENTIONS.md §8).
 */
function buildClient(): HarborClient | null {
  const connection = resolveConnection();
  if (connection === null) {
    return null;
  }
  return new HarborClient({ connection });
}

/**
 * describeError renders a `ProtocolError` into a page-friendly message,
 * keeping the canonical code visible so the operator knows the recovery.
 */
export function describeError(e: unknown): { code: string; message: string } {
  if (e instanceof ProtocolError) {
    switch (e.code) {
      case 'scope_mismatch':
        return { code: e.code, message: 'This action requires the admin scope claim.' };
      case 'identity_required':
        return {
          code: e.code,
          message: 'Identity scope is incomplete — re-attach to the runtime.'
        };
      case 'not_found':
        return {
          code: e.code,
          message: 'MCP server not found — it may have been removed from the runtime config.'
        };
      default:
        return { code: e.code, message: e.message };
    }
  }
  if (e instanceof Error) {
    return { code: 'runtime_error', message: e.message };
  }
  return { code: 'runtime_error', message: 'Unknown error' };
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
  error = $state<{ code: string; message: string } | null>(null);
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

  /** load fetches the server list applying the current filter + page. */
  async load(): Promise<void> {
    const client = buildClient();
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
      this.error = describeError(e);
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

/** A page detail tab. */
export type McpDetailTab = 'tools' | 'resources' | 'prompts' | 'oauth' | 'health' | 'policy';

/** The six detail tabs, in mockup order (page-mcp §12). */
export const MCP_DETAIL_TABS: { id: McpDetailTab; label: string }[] = [
  { id: 'tools', label: 'Tools' },
  { id: 'resources', label: 'Resources' },
  { id: 'prompts', label: 'Prompts' },
  { id: 'oauth', label: 'OAuth & Auth' },
  { id: 'health', label: 'Health' },
  { id: 'policy', label: 'Policy' }
];

/**
 * McpDetailState owns the per-server detail view. The detail route is the
 * page's tabbed-detail surface (CONVENTIONS.md §5 — a page clears the
 * depth bar with a `DetailRail` OR a tabbed detail route; MCP uses both:
 * a list-page rail summary plus this dedicated tabbed route).
 */
export class McpDetailState {
  /** The four-state async status of the detail header load. */
  status = $state<PageStatus>('loading');
  /** The thrown error for the header load. */
  error = $state<{ code: string; message: string } | null>(null);

  server = $state<MCPServerView | null>(null);
  displayModes = $state<string[]>([]);
  contentShapes = $state<string[]>([]);
  toolPolicy = $state<MCPToolPolicyView | null>(null);
  resources = $state<MCPResourceView[]>([]);
  prompts = $state<MCPPromptView[]>([]);
  bindings = $state<MCPBindingView[]>([]);
  health = $state<{ transport_error_rate: number; buckets: number[] } | null>(null);
  activeTab = $state<McpDetailTab>('tools');

  /** isAdmin gates the raw-HTML toggle + OAuth admin actions in the UI.
   * The server is always the authoritative gate (scope_mismatch); this
   * only drives the disabled-state affordance. */
  isAdmin = $state(false);
  /** lastActionError surfaces a scope-mismatch / failure from an action. */
  lastActionError = $state<{ code: string; message: string } | null>(null);

  /** load fetches the per-server detail (header + policy). */
  async load(name: string): Promise<void> {
    const client = buildClient();
    if (client === null) {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.error = null;
    try {
      const detail = await client.mcp.servers.get<MCPServerGetResponse>(name);
      this.server = detail.server;
      this.displayModes = detail.display_modes_advertised;
      this.contentShapes = detail.content_shapes;
      this.toolPolicy = detail.tool_policy;
      this.status = 'ready';
    } catch (e) {
      this.server = null;
      this.error = describeError(e);
      this.status = 'error';
    }
  }

  /** selectTab switches the active detail tab and lazily loads its data. */
  async selectTab(name: string, tab: McpDetailTab): Promise<void> {
    this.activeTab = tab;
    this.lastActionError = null;
    const client = buildClient();
    if (client === null) {
      return;
    }
    try {
      if (tab === 'resources' && this.resources.length === 0) {
        const r = await client.mcp.servers.resources<MCPServerResourcesResponse>(name);
        this.resources = r.resources;
      } else if (tab === 'prompts' && this.prompts.length === 0) {
        const p = await client.mcp.servers.prompts<MCPServerPromptsResponse>(name);
        this.prompts = p.prompts;
      } else if (tab === 'oauth' && this.bindings.length === 0) {
        const b = await client.mcp.servers.bindings<MCPServerBindingsListResponse>(name);
        this.bindings = b.bindings;
      } else if (tab === 'health' && this.health === null) {
        const h = await client.mcp.servers.health<MCPServerHealthResponse>(name);
        this.health = {
          transport_error_rate: h.transport_error_rate,
          buckets: h.handshake_latency_buckets.map(
            (x: { start_ms: number; latency_ms: number }) => x.latency_ms
          )
        };
      } else if (tab === 'policy' && this.toolPolicy === null) {
        const pol = await client.mcp.servers.policy<MCPServerPolicyResponse>(name);
        this.toolPolicy = pol.tool_policy;
      }
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }

  /** refreshDiscovery triggers a control-plane discovery refresh. */
  async refreshDiscovery(name: string): Promise<void> {
    this.lastActionError = null;
    const client = buildClient();
    if (client === null) {
      return;
    }
    try {
      await client.mcp.servers.refreshDiscovery<MCPServerRefreshDiscoveryResponse>(name);
      await this.load(name);
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }

  /** probe runs a transport test-connection. */
  async probe(name: string): Promise<void> {
    this.lastActionError = null;
    const client = buildClient();
    if (client === null) {
      return;
    }
    try {
      await client.mcp.servers.probe<MCPServerProbeResponse>(name);
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }

  /** setRawHTMLTrust flips the per-server raw-HTML opt-in flag. */
  async setRawHTMLTrust(name: string, trusted: boolean): Promise<void> {
    this.lastActionError = null;
    const client = buildClient();
    if (client === null) {
      return;
    }
    try {
      await client.mcp.servers.setRawHTMLTrust<MCPServerSetRawHTMLTrustResponse>(
        name,
        trusted
      );
      await this.load(name);
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }

  /** connectBinding initiates an OAuth (re)connect flow and opens the
   * runtime-provided AuthorizeURL in a popup. The Console never sees
   * plaintext tokens — the runtime closes the flow via pause/resume. */
  async connectBinding(name: string, principalId: string): Promise<void> {
    this.lastActionError = null;
    const client = buildClient();
    if (client === null) {
      return;
    }
    try {
      const resp = await client.mcp.servers.refreshBinding<MCPServerRefreshBindingResponse>(
        name,
        principalId
      );
      if (typeof window !== 'undefined' && resp.authorize_url) {
        window.open(resp.authorize_url, '_blank', 'width=600,height=720');
      }
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }

  /** revokeBinding revokes an OAuth binding and refreshes the list. */
  async revokeBinding(name: string, principalId: string): Promise<void> {
    this.lastActionError = null;
    const client = buildClient();
    if (client === null) {
      return;
    }
    try {
      await client.mcp.servers.revokeBinding<MCPServerRevokeBindingResponse>(
        name,
        principalId
      );
      const b = await client.mcp.servers.bindings<MCPServerBindingsListResponse>(name);
      this.bindings = b.bindings;
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }
}
