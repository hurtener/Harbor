// MCP Connections — Console-DB-backed saved-view controller (D-121,
// MCP refactor — Svelte 5 runes mode, D-092).
//
// CONVENTIONS.md §5 depth bar: a Console page carries Console-DB-backed
// `SavedViewChips`. The legacy MCP page had no saved-view surface at all.
// This controller wires the `<SavedViewChips>` UI component to the
// `MCPSavedFilters` typed wrapper over the shipped `saved_filters`
// Console DB table (D-061: Console-local state only — NO new table).
//
// # Connection identity → DB row scope
//
// `saved_filters` rows are scoped by `operator_id = sha256(tenant:user)`.
// The MCP saved filters are non-sensitive Console-local UI state (a JSON
// facet preset, never a credential — the sensitive `patStore` /
// `authProfiles` tables are separate). The controller therefore opens the
// Console DB with a master key DERIVED FROM the operator identity itself:
// stable per-operator, no passphrase prompt. The Settings phase will let
// the operator opt into a passphrase-encrypted profile; saved-filter
// chips do not block on that.

import { resolveConnection } from '$lib/connection.js';
import { openConsoleDB, operatorIdOf, deriveMasterKey } from '$lib/db/index.js';
import { MCPSavedFilters, type MCPSavedFilter } from '$lib/db/saved_filters_mcp.js';
import type { SavedView } from '$lib/components/ui/SavedViewChips.svelte';
import type { MCPListFilter } from '$lib/protocol/mcp.js';

/** A fixed, non-secret 16-byte KDF salt for the identity-derived
 * Console-DB key. The saved-filter table holds Console-local UI state
 * only; the salt does not protect a credential — it merely satisfies the
 * KDF signature (`deriveMasterKey` requires a 16-byte salt). */
const SAVED_VIEW_KDF_SALT = new TextEncoder().encode('harbor-consoleUI');

/**
 * McpSavedViews owns the MCP page's saved-view chip state. It opens the
 * Console DB lazily on first use; when the Console is not attached to a
 * Runtime (`connection.ts` → null) it stays empty — there is no operator
 * identity to scope rows under.
 */
export class McpSavedViews {
  /** The saved-view chips, as the shared `<SavedViewChips>` shape. */
  views = $state<SavedView[]>([]);
  /** The decoded saved filters, keyed by id (for apply). */
  #byId = new Map<string, MCPSavedFilter>();
  #store: MCPSavedFilters | null = null;

  /** Resolves (and caches) the `MCPSavedFilters` store, or null when the
   * Console is not attached to a Runtime. */
  async #resolveStore(): Promise<MCPSavedFilters | null> {
    if (this.#store !== null) {
      return this.#store;
    }
    const connection = resolveConnection();
    if (connection === null) {
      return null;
    }
    const { tenant, user } = connection.identity;
    const operatorID = await operatorIdOf(tenant, user);
    const masterKey = await deriveMasterKey(operatorID, SAVED_VIEW_KDF_SALT);
    const db = await openConsoleDB({
      operatorIdentity: { tenantID: tenant, userID: user },
      masterKey
    });
    this.#store = new MCPSavedFilters(db, operatorID);
    return this.#store;
  }

  /** Loads the operator's saved MCP filters into the chip list. */
  async load(): Promise<void> {
    const store = await this.#resolveStore();
    if (store === null) {
      this.views = [];
      return;
    }
    const filters = await store.list();
    this.#byId = new Map(filters.map((f) => [f.id, f]));
    this.views = filters.map((f) => ({ id: f.id, name: f.name }));
  }

  /** Returns the decoded filter spec for a saved-view id, or null. */
  filterFor(id: string): MCPListFilter | null {
    return this.#byId.get(id)?.filterSpec ?? null;
  }

  /** Persists the current filter as a new named saved view, then reloads. */
  async create(name: string, filterSpec: MCPListFilter): Promise<void> {
    const store = await this.#resolveStore();
    if (store === null) {
      return;
    }
    await store.create(name, filterSpec);
    await this.load();
  }

  /** Deletes a saved view by id, then reloads. */
  async remove(id: string): Promise<void> {
    const store = await this.#resolveStore();
    if (store === null) {
      return;
    }
    await store.delete(id);
    await this.load();
  }
}
