// Harbor Console — Memory page reactive state controller
// (Phase 108n / D-186). Svelte 5 runes mode (D-092).
//
// This module owns the Memory page's reactive state; the `.svelte` views read
// it and call its actions, never touching the Protocol client directly
// (CONVENTIONS.md §6). It is the king-file refactor (the page was a ~728-line
// monolith with a `__HARBOR_PROTOCOL_CLIENT__` global-injection seam) — the
// controller + the pure `derive.ts` projections + the focused `MemoryTable` /
// `MemoryEventsCard` components replace it, mirroring the Tools-108k /
// MCP-108m structure.
//
// The page is a pure Protocol consumer — it composes the already-shipped three
// `memory.*` methods (Phase 73j / D-118): `memory.list` (the faceted page +
// aggregates), `memory.get` (the selected-record detail), `memory.health` (the
// right-rail health card) — plus the shipped `events.subscribe` SSE (the LIVE
// memory-event feed) + the Console-DB saved filters (D-061). No new Protocol
// method (§13). The mutation surface (add / edit / evict) is NOT shipped at V1
// (page-memory.md §10), so the bulk-action bar stays disabled-with-tooltip.
//
// Fields an async load assigns AFTER first render (the list response, the
// detail, the health aggregate, the live event page) are `$state` so the
// reactive re-read fires — the Events-page D-180 lesson.

import {
  resolveConnection,
  hasScope,
  type RuntimeConnection
} from '$lib/connection.js';
import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
import { EventsSubscription } from '$lib/events/subscription.svelte.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type { SavedView } from '$lib/components/ui/SavedViewChips.svelte';
import { openMemorySavedFilters } from '$lib/memory/saved_views.js';
import type { MemorySavedFilters } from '$lib/db/saved_filters_memory.js';
import {
  toPageError,
  displayStatus,
  projectMemoryEvents,
  MEMORY_EVENT_TYPES,
  type PageError,
  type MemoryEventEntry
} from '$lib/memory/derive.js';
import {
  DEFAULT_MEMORY_LIST_PAGE_SIZE,
  type MemoryItem,
  type MemoryItemDetail,
  type MemoryFilter,
  type MemoryHealthAggregate,
  type MemoryStrategyName,
  type MemoryListResponse,
  type MemoryStrategyTrace,
  type MemoryStrategyTraceResponse,
  type MemoryPutResponse,
  type MemoryDeleteResponse
} from '$lib/protocol/memory-types.js';

export class MemoryPageState {
  /* ---- connection + client (CONVENTIONS.md §6) ------------------- */
  connection = $state<RuntimeConnection | null>(null);
  /** True when the connection carries the `admin` claim — the mutation
   * surface is deferred (§10), so this only gates future affordances. */
  canAdmin = $state(false);
  disconnected = $derived(this.connection === null);

  /* ---- primary list async state (the four-state contract) -------- */
  status = $state<PageStatus>('loading');
  pageError = $state<PageError | null>(null);
  listResp = $state<MemoryListResponse | null>(null);
  health = $state<MemoryHealthAggregate | null>(null);

  /* ---- filters --------------------------------------------------- */
  contentSearch = $state('');
  scopeFacet = $state('');
  driverFacet = $state('');
  strategyOverlay = $state<MemoryStrategyName | null>(null);

  /* ---- pagination ------------------------------------------------ */
  page = $state(1);
  pageSize = $state(DEFAULT_MEMORY_LIST_PAGE_SIZE);

  /* ---- selection + detail rail ----------------------------------- */
  checkedKeys = $state<Set<string>>(new Set());
  selectedKey = $state<string | null>(null);
  detailStatus = $state<PageStatus>('empty');
  detailError = $state<PageError | null>(null);
  detail = $state<MemoryItemDetail | null>(null);

  /* ---- live memory-event feed (events.subscribe — D-186) --------- */
  subscription = $state<EventsSubscription | null>(null);

  /* ---- strategy trace (memory.strategy_trace — D-186) ------------ */
  strategyTrace = $state<MemoryStrategyTrace | null>(null);

  /* ---- admin mutations (memory.put / memory.delete — D-186) ------ */
  mutationBusy = $state(false);
  mutationResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- saved views (Console-DB-backed, D-061) -------------------- */
  savedFiltersDB = $state<MemorySavedFilters | null>(null);
  savedViews = $state<SavedView[]>([]);
  activeViewId = $state<string | null>(null);

  #client: ProtocolClient | null = null;

  /* ================================================================ */
  /* Derived projections                                               */
  /* ================================================================ */

  get items(): MemoryItem[] {
    return this.listResp?.items ?? [];
  }

  get totalRows(): number {
    return this.listResp?.total_rows ?? 0;
  }

  /** The status the `<PageState>` boundary renders — ready/empty live. */
  get displayStatus(): PageStatus {
    return displayStatus(this.status, this.items.length);
  }

  /** The live memory-event rows, newest-first (honest-empty when quiet). */
  get recentEvents(): MemoryEventEntry[] {
    return projectMemoryEvents(this.subscription?.events ?? []);
  }

  /** The SSE stream state — drives the event-feed live dot. */
  get streamState(): string {
    return this.subscription?.state ?? 'idle';
  }

  /* ================================================================ */
  /* Boot + loading                                                    */
  /* ================================================================ */

  /**
   * Resolves the connection + the Console-DB saved filters, opens the live
   * memory-event subscription, and loads the first page. `injected` is an
   * optional in-page client the harness supplies (read inside the page's
   * onMount closure — never captured at construction).
   */
  load(injected?: ProtocolClient): void {
    const connection = resolveConnection();
    this.connection = connection;
    this.canAdmin = hasScope(connection, 'admin');

    if (connection === null) {
      this.#client = null;
      this.status = 'disconnected';
      return;
    }
    this.#client = injected ?? new HarborClient({ connection });

    // Open the live `memory.*` event subscription (the right-rail feed). The
    // page calls load() in onMount (browser); unit tests exercise the loaders
    // directly without constructing an EventSource.
    if (injected === undefined && this.subscription === null) {
      const sub = new EventsSubscription(this.#client.events);
      sub.open({ eventTypes: MEMORY_EVENT_TYPES as string[] });
      this.subscription = sub;
    }

    // Wire the Console-DB-backed saved-view store (D-061). Best-effort: a
    // failure leaves the chips empty but the page works.
    void (async () => {
      this.savedFiltersDB = await openMemorySavedFilters(connection);
      await this.refreshSavedViews();
    })();

    void this.loadList();
    void this.loadStrategyTrace();
  }

  /** Opens the live event subscription explicitly (page onMount, browser). */
  openEvents(): void {
    if (this.#client !== null && this.subscription === null) {
      const sub = new EventsSubscription(this.#client.events);
      sub.open({ eventTypes: MEMORY_EVENT_TYPES as string[] });
      this.subscription = sub;
    }
  }

  /** Closes the live subscription on page unmount. */
  close(): void {
    this.subscription?.close();
    this.subscription = null;
  }

  /** Assembles the `MemoryFilter` from the live filter controls. */
  #currentFilter(): MemoryFilter {
    const f: MemoryFilter = {};
    if (this.contentSearch) f.content_search = this.contentSearch;
    if (this.scopeFacet) f.scopes = [this.scopeFacet];
    if (this.driverFacet) f.drivers = [this.driverFacet];
    if (this.strategyOverlay) f.strategies = [this.strategyOverlay];
    return f;
  }

  /** The Console-local NDJSON / CSV export reads the loaded filter. */
  currentFilter(): MemoryFilter {
    return this.#currentFilter();
  }

  /** loadList fetches the memory page + health for the current filter. */
  async loadList(): Promise<void> {
    if (this.#client === null) {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.pageError = null;
    try {
      const [list, healthResp] = await Promise.all([
        this.#client.memory.list({
          filter: this.#currentFilter(),
          page: this.page,
          page_size: this.pageSize
        }),
        this.#client.memory.health()
      ]);
      this.listResp = list;
      this.health = healthResp.aggregate;
      this.status = 'ready';
    } catch (err) {
      this.listResp = null;
      this.health = null;
      this.pageError = toPageError(err);
      this.status = 'error';
    }
  }

  /** The Refresh button + the post-filter reload re-fetch the current page. */
  refresh(): void {
    void this.loadList();
    void this.loadStrategyTrace();
  }

  /** loadStrategyTrace fetches the live strategy-compaction projection
   * (`memory.strategy_trace`). Honest-empty when the session has no memory. */
  async loadStrategyTrace(): Promise<void> {
    if (this.#client === null) return;
    try {
      const resp = await this.#client.memory.strategyTrace<MemoryStrategyTraceResponse>();
      this.strategyTrace = resp.trace;
    } catch {
      // A trace-load failure leaves the card empty; the list page still works.
      this.strategyTrace = null;
    }
  }

  /* ================================================================ */
  /* Admin mutations — the REAL memory.put / memory.delete (§13 / D-079) */
  /* ================================================================ */

  /**
   * addTurn appends an operator-supplied turn via the REAL admin
   * `memory.put`, then re-loads the list + trace so the rendered state
   * reflects the runtime. No-op without the admin claim (the UI gates it
   * disabled-with-tooltip; the runtime is the authoritative gate, D-079).
   */
  async addTurn(userMessage: string, assistantResponse: string): Promise<void> {
    if (this.#client === null || !this.canAdmin || this.mutationBusy) return;
    this.mutationBusy = true;
    this.mutationResult = null;
    try {
      const resp = await this.#client.memory.put<MemoryPutResponse>(userMessage, assistantResponse);
      this.mutationResult = { ok: true, message: `Memory turn added (${resp.key}).` };
      await this.loadList();
      await this.loadStrategyTrace();
    } catch (err) {
      const e = toPageError(err);
      this.mutationResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      this.mutationBusy = false;
    }
  }

  /** evict removes one memory turn (by key) via the REAL admin
   * `memory.delete`, then re-loads. Clears the rail when the evicted row
   * was selected. */
  async evict(key: string): Promise<void> {
    if (this.#client === null || !this.canAdmin || this.mutationBusy) return;
    this.mutationBusy = true;
    this.mutationResult = null;
    try {
      const resp = await this.#client.memory.delete<MemoryDeleteResponse>(key);
      this.mutationResult = {
        ok: resp.deleted,
        message: resp.deleted
          ? `Evicted — ${resp.remaining_turns} turn(s) remain in the session record.`
          : 'Nothing evicted — the turn was not found.'
      };
      if (this.selectedKey === key) this.clearSelection();
      this.checkedKeys = new Set([...this.checkedKeys].filter((k) => k !== key));
      await this.loadList();
      await this.loadStrategyTrace();
    } catch (err) {
      const e = toPageError(err);
      this.mutationResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      this.mutationBusy = false;
    }
  }

  /** evictSelected evicts every checked turn (one real `memory.delete` per
   * key), then re-loads. Honest aggregate result line. */
  async evictSelected(): Promise<void> {
    if (this.#client === null || !this.canAdmin || this.mutationBusy) return;
    const keys = [...this.checkedKeys];
    if (keys.length === 0) return;
    this.mutationBusy = true;
    this.mutationResult = null;
    let ok = 0;
    let failed = 0;
    for (const key of keys) {
      try {
        const resp = await this.#client.memory.delete<MemoryDeleteResponse>(key);
        if (resp.deleted) ok += 1;
        else failed += 1;
      } catch {
        failed += 1;
      }
    }
    this.mutationResult = {
      ok: failed === 0,
      message: `Evict selected: ${ok} evicted, ${failed} failed (of ${keys.length}).`
    };
    this.checkedKeys = new Set();
    this.clearSelection();
    await this.loadList();
    await this.loadStrategyTrace();
    this.mutationBusy = false;
  }

  /** loadDetail loads one record's full detail into the rail's PageState. */
  async loadDetail(key: string): Promise<void> {
    this.selectedKey = key;
    if (this.#client === null) {
      this.detailStatus = 'disconnected';
      return;
    }
    this.detailStatus = 'loading';
    this.detailError = null;
    this.detail = null;
    try {
      const resp = await this.#client.memory.get(key);
      this.detail = resp.detail;
      this.detailStatus = 'ready';
    } catch (err) {
      this.detailError = toPageError(err);
      this.detailStatus = 'error';
    }
  }

  /** Clears the selected-item detail back to the rail hint. */
  clearSelection(): void {
    this.selectedKey = null;
    this.detail = null;
    this.detailStatus = 'empty';
    this.detailError = null;
  }

  setSelection(next: Set<string>): void {
    this.checkedKeys = next;
  }

  /* ================================================================ */
  /* Filtering + pagination (each resets to page 1 + reloads)          */
  /* ================================================================ */

  applyContentSearch(value: string): void {
    this.contentSearch = value;
    this.page = 1;
    this.activeViewId = null;
    void this.loadList();
  }

  applyScopeFacet(value: string): void {
    this.scopeFacet = value;
    this.page = 1;
    this.activeViewId = null;
    void this.loadList();
  }

  applyDriverFacet(value: string): void {
    this.driverFacet = value;
    this.page = 1;
    this.activeViewId = null;
    void this.loadList();
  }

  applyStrategyOverlay(s: MemoryStrategyName | null): void {
    this.strategyOverlay = s;
    this.page = 1;
    this.activeViewId = null;
    void this.loadList();
  }

  clearFilters(): void {
    this.contentSearch = '';
    this.scopeFacet = '';
    this.driverFacet = '';
    this.strategyOverlay = null;
    this.activeViewId = null;
    this.page = 1;
    void this.loadList();
  }

  changePage(p: number): void {
    this.page = p;
    void this.loadList();
  }

  changePageSize(size: number): void {
    this.pageSize = size;
    this.page = 1;
    void this.loadList();
  }

  /* ================================================================ */
  /* Saved views (Console-DB-backed, D-061)                            */
  /* ================================================================ */

  async refreshSavedViews(): Promise<void> {
    if (this.savedFiltersDB === null) return;
    try {
      const rows = await this.savedFiltersDB.list();
      this.savedViews = rows.map((r) => ({ id: r.id, name: r.name }));
    } catch {
      this.savedViews = [];
    }
  }

  async applySavedView(id: string): Promise<void> {
    if (this.savedFiltersDB === null) return;
    const saved = await this.savedFiltersDB.get(id);
    if (saved === null) return;
    this.activeViewId = id;
    this.contentSearch = saved.filter.content_search ?? '';
    this.scopeFacet = saved.filter.scopes?.[0] ?? '';
    this.driverFacet = saved.filter.drivers?.[0] ?? '';
    this.strategyOverlay = (saved.filter.strategies?.[0] as MemoryStrategyName) ?? null;
    this.page = 1;
    void this.loadList();
  }

  async deleteSavedView(id: string): Promise<void> {
    if (this.savedFiltersDB === null) return;
    await this.savedFiltersDB.delete(id);
    if (this.activeViewId === id) this.activeViewId = null;
    await this.refreshSavedViews();
  }

  async saveCurrentView(name: string): Promise<void> {
    if (this.savedFiltersDB === null || name.trim().length === 0) return;
    const id = `mem-view-${this.savedViews.length}-${name.trim().toLowerCase().replace(/\s+/g, '-')}`;
    await this.savedFiltersDB.put({ id, name: name.trim(), filter: this.#currentFilter() });
    await this.refreshSavedViews();
    this.activeViewId = id;
  }
}
