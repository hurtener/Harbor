// Harbor Console — Artifacts page reactive state controller
// (Phase 108o / D-187). Svelte 5 runes mode (D-092).
//
// This module owns the Artifacts page's reactive state; the `.svelte` views
// read it and call its actions, never touching the Protocol client directly
// (CONVENTIONS.md §6). It is the king-file refactor (the page was a ~916-line
// monolith) — the controller + the pure `derive.ts` projections + the focused
// `ArtifactsTable` component replace it, mirroring the Memory-108n structure.
//
// The page composes the shipped artifacts surface: `artifacts.list` (the
// catalog), `artifacts.get_ref` (the presigned-URL preview/download),
// `artifacts.put` (upload), plus the NEW Phase 108o (D-187) admin-gated
// `artifacts.delete` (evict) — the primitive + its consumer in one wave
// (§13). Heavy bytes NEVER cross the wire (D-026): the preview/download route
// through the presigned URL; the export is metadata-only.
//
// Fields an async load assigns AFTER first render are `$state` so the reactive
// re-read fires (the Events-page D-180 lesson).

import {
  resolveConnection,
  hasScope,
  type RuntimeConnection
} from '$lib/connection.js';
import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type { SavedView } from '$lib/components/ui/SavedViewChips.svelte';
import type { PreviewState } from '$lib/components/artifacts/ArtifactPreview.svelte';
import { openArtifactsSavedViewStore } from '$lib/artifacts/saved_views.js';
import type { ArtifactsSavedFilters } from '$lib/db/saved_filters_artifacts.js';
import { toPageError, displayStatus, type PageError } from '$lib/artifacts/derive.js';
import type {
  ArtifactRow,
  ArtifactSource,
  ArtifactsListRequest,
  ArtifactsListResponse,
  ArtifactsGetRefResponse,
  ArtifactsPutResponse,
  ArtifactsDeleteResponse
} from '$lib/protocol/artifacts.js';

export class ArtifactsPageState {
  /* ---- connection + client (CONVENTIONS.md §6) ------------------- */
  connection = $state<RuntimeConnection | null>(null);
  /** True when the connection carries the `admin` claim (D-079) — gates the
   * Delete mutation surface (disabled-with-tooltip without it). */
  canAdmin = $state(false);
  disconnected = $derived(this.connection === null);

  /* ---- catalog async state (four-state contract) ----------------- */
  status = $state<PageStatus>('loading');
  pageError = $state<PageError | null>(null);
  rows = $state<ArtifactRow[]>([]);
  totalMatched = $state(0);
  protocolVersion = $state('');

  /* ---- pagination + filters -------------------------------------- */
  page = $state(1);
  pageSize = $state(50);
  mimeFilter = $state('');
  sourceFilter = $state<ArtifactSource | ''>('');

  /* ---- saved views (Console-DB-backed, D-061) -------------------- */
  savedViewStore = $state<ArtifactsSavedFilters | null>(null);
  savedViews = $state<SavedView[]>([]);
  activeViewId = $state<string | null>(null);

  /* ---- selection + preview --------------------------------------- */
  selectedRow = $state<ArtifactRow | null>(null);
  selection = $state<Set<string>>(new Set());
  preview = $state<PreviewState>({ src: '', errorCode: '', loading: false });

  /* ---- admin mutation (artifacts.delete — D-187) ----------------- */
  mutationBusy = $state(false);
  mutationResult = $state<{ ok: boolean; message: string } | null>(null);

  #client: ProtocolClient | null = null;

  /** The display status — ready/empty derived live from the row count. */
  get displayStatus(): PageStatus {
    return displayStatus(this.status, this.rows.length);
  }

  /* ================================================================ */
  /* Boot                                                              */
  /* ================================================================ */

  load(injectedClient?: ProtocolClient, injectedStore?: ArtifactsSavedFilters): void {
    const connection = resolveConnection();
    this.connection = connection;
    this.canAdmin = hasScope(connection, 'admin');
    if (injectedClient !== undefined) {
      this.#client = injectedClient;
    } else if (connection !== null) {
      this.#client = new HarborClient({ connection });
    } else {
      this.#client = null;
    }

    void (async () => {
      if (injectedStore !== undefined) {
        this.savedViewStore = injectedStore;
      } else {
        try {
          this.savedViewStore = await openArtifactsSavedViewStore();
        } catch {
          this.savedViewStore = null;
        }
      }
      await this.refreshSavedViews();
      await this.loadCatalog();
    })();
  }

  /** The LIST scope — tenant is mandatory; an empty session is a wildcard
   * (the runtime matches every session in scope). */
  #scope(): { tenant: string; user: string; session: string } | null {
    const c = this.connection;
    if (c === null) return null;
    return { tenant: c.identity.tenant, user: c.identity.user, session: c.identity.session };
  }

  /**
   * The ITEM scope for the full-triple methods (`get_ref` / `put` / `delete`),
   * which require a complete identity. Under the D-171 default the connection
   * session is blank (the token pins the session, not the connection) — so when
   * it is empty we send an ALL-EMPTY scope `{}` and let the runtime backfill the
   * full triple from the verified JWT (the "simplest correct client sends
   * identity: {}" contract). A non-empty connection session is sent verbatim.
   */
  #itemScope(): { tenant: string; user: string; session: string } | null {
    const c = this.connection;
    if (c === null) return null;
    if (c.identity.session !== '') {
      return { tenant: c.identity.tenant, user: c.identity.user, session: c.identity.session };
    }
    return { tenant: '', user: '', session: '' };
  }

  async loadCatalog(): Promise<void> {
    if (this.#client === null) {
      this.status = 'disconnected';
      return;
    }
    const scope = this.#scope();
    if (scope === null) {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.pageError = null;
    const req: ArtifactsListRequest = { scope, limit: this.pageSize };
    if (this.mimeFilter) req.mime_type = [this.mimeFilter];
    if (this.sourceFilter) req.source = [this.sourceFilter];
    try {
      const resp = await this.#client.artifacts.list<ArtifactsListResponse>(
        req as unknown as Record<string, unknown>
      );
      this.rows = resp.rows ?? [];
      this.totalMatched = resp.total_matched ?? this.rows.length;
      this.protocolVersion = resp.protocol_version ?? '';
      this.status = 'ready';
    } catch (e) {
      this.rows = [];
      this.totalMatched = 0;
      this.pageError = toPageError(e);
      this.status = 'error';
    }
  }

  refresh(): void {
    void this.loadCatalog();
  }

  /* ================================================================ */
  /* Selection + preview (artifacts.get_ref)                           */
  /* ================================================================ */

  selectRow(row: ArtifactRow): void {
    this.selectedRow = row;
    void this.resolvePreview(row);
  }

  clearSelectedRow(): void {
    this.selectedRow = null;
    this.preview = { src: '', errorCode: '', loading: false };
  }

  async resolvePreview(row: ArtifactRow): Promise<void> {
    if (this.#client === null) return;
    const scope = this.#itemScope();
    if (scope === null) {
      this.preview = { src: '', errorCode: 'disconnected', loading: false };
      return;
    }
    this.preview = { src: '', errorCode: '', loading: true };
    try {
      const resp = await this.#client.artifacts.getRef<ArtifactsGetRefResponse>({
        scope,
        id: row.ref.id
      });
      this.preview = { src: resp.presigned_url, errorCode: '', loading: false };
    } catch (e) {
      this.preview = { src: '', errorCode: toPageError(e).code, loading: false };
    }
  }

  setSelection(next: Set<string>): void {
    this.selection = next;
  }

  /* ================================================================ */
  /* Filtering + pagination                                            */
  /* ================================================================ */

  applyMime(value: string): void {
    this.mimeFilter = value;
    this.activeViewId = null;
    this.page = 1;
    void this.loadCatalog();
  }

  applySource(value: ArtifactSource | ''): void {
    this.sourceFilter = value;
    this.activeViewId = null;
    this.page = 1;
    void this.loadCatalog();
  }

  clearFilters(): void {
    this.mimeFilter = '';
    this.sourceFilter = '';
    this.activeViewId = null;
    this.page = 1;
    void this.loadCatalog();
  }

  changePage(next: number): void {
    this.page = next;
    void this.loadCatalog();
  }

  changePageSize(size: number): void {
    this.pageSize = size;
    this.page = 1;
    void this.loadCatalog();
  }

  /* ================================================================ */
  /* Saved views (Console-DB-backed, D-061)                            */
  /* ================================================================ */

  async refreshSavedViews(): Promise<void> {
    if (this.savedViewStore === null) {
      this.savedViews = [];
      return;
    }
    const stored = await this.savedViewStore.list();
    this.savedViews = stored.map((s) => ({ id: s.id, name: s.name }));
  }

  async applySavedView(id: string): Promise<void> {
    if (this.savedViewStore === null) return;
    const view = await this.savedViewStore.get(id);
    if (view === null) return;
    this.activeViewId = id;
    this.mimeFilter = view.filterSpec.mimeType ?? '';
    this.sourceFilter = (view.filterSpec.source as ArtifactSource | '') ?? '';
    this.page = 1;
    await this.loadCatalog();
  }

  async deleteSavedView(id: string): Promise<void> {
    if (this.savedViewStore === null) return;
    await this.savedViewStore.delete(id);
    if (this.activeViewId === id) this.activeViewId = null;
    await this.refreshSavedViews();
  }

  async saveCurrentView(): Promise<void> {
    if (this.savedViewStore === null) return;
    const label = `${this.mimeFilter || 'any MIME'} · ${this.sourceFilter || 'any source'}`.trim();
    const created = await this.savedViewStore.create(label, {
      mimeType: this.mimeFilter,
      source: this.sourceFilter
    });
    this.activeViewId = created.id;
    await this.refreshSavedViews();
  }

  /* ================================================================ */
  /* Upload (artifacts.put) + export (Console-local, D-061)            */
  /* ================================================================ */

  /** Uploads a file via the real `artifacts.put`, then re-loads + selects it. */
  async uploadFile(file: File): Promise<void> {
    if (this.#client === null) return;
    const scope = this.#itemScope();
    if (scope === null) return;
    const buf = new Uint8Array(await file.arrayBuffer());
    let binary = '';
    for (const b of buf) binary += String.fromCharCode(b);
    const base64 = btoa(binary);
    try {
      const putResp = await this.#client.artifacts.put<ArtifactsPutResponse>({
        scope,
        bytes: base64,
        opts: { mime_type: file.type || 'application/octet-stream', filename: file.name }
      });
      await this.loadCatalog();
      const uploaded = this.rows.find((r) => r.ref.id === putResp.ref.id);
      if (uploaded) this.selectRow(uploaded);
    } catch (e) {
      this.pageError = toPageError(e);
      this.status = 'error';
    }
  }

  /** Metadata-only CSV of the loaded page (D-026 — never blob bytes). */
  exportCSV(): string {
    const header = 'id,filename,mime_type,size_bytes,source,driver,created_at';
    const lines = this.rows.map((r) =>
      [
        r.ref.id,
        r.ref.filename ?? '',
        r.ref.mime_type ?? '',
        String(r.ref.size_bytes),
        r.source ?? '',
        r.driver ?? '',
        r.created_at ?? ''
      ].join(',')
    );
    return [header, ...lines].join('\n');
  }

  /* ================================================================ */
  /* Admin mutation — the REAL artifacts.delete (§13 / D-079)          */
  /* ================================================================ */

  /** Evicts one artifact via the REAL admin `artifacts.delete`, then
   * re-loads. No-op without the admin claim (the UI gates it
   * disabled-with-tooltip; the runtime is the authoritative gate). */
  async deleteArtifact(id: string): Promise<void> {
    if (this.#client === null || !this.canAdmin || this.mutationBusy) return;
    const scope = this.#itemScope();
    if (scope === null) return;
    this.mutationBusy = true;
    this.mutationResult = null;
    try {
      const resp = await this.#client.artifacts.delete<ArtifactsDeleteResponse>({ scope, id });
      this.mutationResult = {
        ok: resp.deleted,
        message: resp.deleted ? `Artifact ${id} evicted.` : `Artifact ${id} not found — nothing evicted.`
      };
      if (this.selectedRow?.ref.id === id) this.clearSelectedRow();
      this.selection = new Set([...this.selection].filter((s) => s !== id));
      await this.loadCatalog();
    } catch (e) {
      const err = toPageError(e);
      this.mutationResult = { ok: false, message: `${err.code}: ${err.message}` };
    } finally {
      this.mutationBusy = false;
    }
  }

  /** Evicts every checked artifact (one real `artifacts.delete` per id). */
  async deleteSelected(): Promise<void> {
    if (this.#client === null || !this.canAdmin || this.mutationBusy) return;
    const scope = this.#itemScope();
    if (scope === null) return;
    const ids = [...this.selection];
    if (ids.length === 0) return;
    this.mutationBusy = true;
    this.mutationResult = null;
    let ok = 0;
    let failed = 0;
    for (const id of ids) {
      try {
        const resp = await this.#client.artifacts.delete<ArtifactsDeleteResponse>({ scope, id });
        if (resp.deleted) ok += 1;
        else failed += 1;
      } catch {
        failed += 1;
      }
    }
    this.mutationResult = {
      ok: failed === 0,
      message: `Delete selected: ${ok} evicted, ${failed} failed (of ${ids.length}).`
    };
    this.selection = new Set();
    this.clearSelectedRow();
    await this.loadCatalog();
    this.mutationBusy = false;
  }
}
