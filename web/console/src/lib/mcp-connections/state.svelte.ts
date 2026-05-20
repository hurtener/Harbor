// Phase 73k (D-119) — MCP Connections page reactive state (Svelte 5
// runes mode — D-092). This module owns the page's reactive state; the
// `.svelte` components read it and call its actions, never touching the
// Protocol client directly.

import type { OperatorIdentity } from '$lib/protocol';
import {
  mcpApi,
  ProtocolCallError,
  type MCPServerView,
  type MCPListFilter,
  type MCPServerState
} from './api';

/** A fixed dev operator identity. A later Console phase wires the real
 * operator identity from the Console DB `profiles` table; until then the
 * page uses a stable dev identity so the Protocol calls carry a triple. */
const DEV_OPERATOR: OperatorIdentity = { tenantID: 'console', userID: 'operator' };
const DEV_SESSION = 'console-mcp-connections';

/** McpListState owns the servers-list view. */
export class McpListState {
  servers = $state<MCPServerView[]>([]);
  loading = $state(false);
  error = $state<string | null>(null);
  filter = $state<MCPListFilter>({});

  /** load fetches the server list applying the current filter. */
  async load(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const resp = await mcpApi.list(DEV_OPERATOR, DEV_SESSION, this.filter);
      this.servers = resp.servers;
    } catch (e) {
      this.error = describeError(e);
    } finally {
      this.loading = false;
    }
  }

  /** setStateFilter narrows the list to a single state chip (or clears). */
  setStateFilter(state: MCPServerState | null): void {
    this.filter = state ? { ...this.filter, state: [state] } : { ...this.filter, state: undefined };
    void this.load();
  }
}

/** A page detail tab. */
export type McpDetailTab = 'tools' | 'resources' | 'prompts' | 'oauth' | 'health' | 'policy';

/** McpDetailState owns the per-server detail view. */
export class McpDetailState {
  server = $state<MCPServerView | null>(null);
  displayModes = $state<string[]>([]);
  contentShapes = $state<string[]>([]);
  toolPolicy = $state<{ timeout_ms: number; max_retries: number; concurrency_cap: number } | null>(
    null
  );
  resources = $state<{ uri: string; mime_type?: string }[]>([]);
  prompts = $state<{ name: string; description?: string }[]>([]);
  bindings = $state<{ principal_id: string; binding_scope: string; scopes: string[] }[]>([]);
  health = $state<{ transport_error_rate: number; buckets: number[] } | null>(null);
  activeTab = $state<McpDetailTab>('tools');
  loading = $state(false);
  error = $state<string | null>(null);
  /** isAdmin gates the raw-HTML toggle + OAuth admin actions in the UI.
   * The server is always the authoritative gate (CodeScopeMismatch);
   * this only drives the disabled-state affordance. A later Console
   * phase wires it from the verified scope set. */
  isAdmin = $state(false);
  /** lastTrustToggleError surfaces a scope-mismatch from the toggle. */
  lastActionError = $state<string | null>(null);

  /** load fetches the per-server detail (header + policy + bindings). */
  async load(name: string): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const detail = await mcpApi.get(DEV_OPERATOR, DEV_SESSION, name);
      this.server = detail.server;
      this.displayModes = detail.display_modes_advertised;
      this.contentShapes = detail.content_shapes;
      this.toolPolicy = detail.tool_policy;
    } catch (e) {
      this.error = describeError(e);
    } finally {
      this.loading = false;
    }
  }

  /** selectTab switches the active detail tab and lazily loads its data. */
  async selectTab(name: string, tab: McpDetailTab): Promise<void> {
    this.activeTab = tab;
    this.lastActionError = null;
    try {
      if (tab === 'resources' && this.resources.length === 0) {
        const r = await mcpApi.resources(DEV_OPERATOR, DEV_SESSION, name);
        this.resources = r.resources;
      } else if (tab === 'prompts' && this.prompts.length === 0) {
        const p = await mcpApi.prompts(DEV_OPERATOR, DEV_SESSION, name);
        this.prompts = p.prompts;
      } else if (tab === 'oauth' && this.bindings.length === 0) {
        const b = await mcpApi.bindings(DEV_OPERATOR, DEV_SESSION, name);
        this.bindings = b.bindings;
      } else if (tab === 'health' && this.health === null) {
        const h = await mcpApi.health(DEV_OPERATOR, DEV_SESSION, name);
        this.health = {
          transport_error_rate: h.transport_error_rate,
          buckets: h.handshake_latency_buckets.map((x) => x.latency_ms)
        };
      } else if (tab === 'policy' && this.toolPolicy === null) {
        const pol = await mcpApi.policy(DEV_OPERATOR, DEV_SESSION, name);
        this.toolPolicy = pol.tool_policy;
      }
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }

  /** refreshDiscovery triggers a control-plane discovery refresh. */
  async refreshDiscovery(name: string): Promise<void> {
    this.lastActionError = null;
    try {
      await mcpApi.refreshDiscovery(DEV_OPERATOR, DEV_SESSION, name);
      await this.load(name);
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }

  /** probe runs a transport test-connection. */
  async probe(name: string): Promise<void> {
    this.lastActionError = null;
    try {
      await mcpApi.probe(DEV_OPERATOR, DEV_SESSION, name);
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }

  /** setRawHTMLTrust flips the per-server raw-HTML opt-in flag and
   * refreshes the server view from the follow-up get. */
  async setRawHTMLTrust(name: string, trusted: boolean): Promise<void> {
    this.lastActionError = null;
    try {
      await mcpApi.setRawHTMLTrust(DEV_OPERATOR, DEV_SESSION, name, trusted);
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
    try {
      const resp = await mcpApi.refreshBinding(DEV_OPERATOR, DEV_SESSION, name, principalId);
      if (typeof window !== 'undefined' && resp.authorize_url) {
        window.open(resp.authorize_url, '_blank', 'width=600,height=720');
      }
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }

  /** revokeBinding revokes an OAuth binding. */
  async revokeBinding(name: string, principalId: string): Promise<void> {
    this.lastActionError = null;
    try {
      await mcpApi.revokeBinding(DEV_OPERATOR, DEV_SESSION, name, principalId);
      const b = await mcpApi.bindings(DEV_OPERATOR, DEV_SESSION, name);
      this.bindings = b.bindings;
    } catch (e) {
      this.lastActionError = describeError(e);
    }
  }
}

/** describeError renders a Protocol error into a page-friendly message,
 * keeping the canonical code visible so the operator knows the recovery. */
export function describeError(e: unknown): string {
  if (e instanceof ProtocolCallError) {
    switch (e.code) {
      case 'scope_mismatch':
        return 'This action requires the admin scope claim.';
      case 'identity_required':
        return 'Identity scope is incomplete — re-attach to the runtime.';
      case 'not_found':
        return 'MCP server not found — it may have been removed from the runtime config.';
      default:
        return `${e.code}: ${e.message}`;
    }
  }
  if (e instanceof Error) {
    return e.message;
  }
  return 'Unknown error';
}
