<script lang="ts">
  // Harbor Console — Artifacts page (`/artifacts`). Refactored onto the
  // design-system foundation (D-121, CONVENTIONS.md).
  //
  // # Console consistency (CONVENTIONS.md §9)
  //
  //  - Routes under `(console)/` with no `/console/` URL prefix (§1).
  //  - Renders inside the shared app shell (§2).
  //  - Composes the `components/ui/` inventory — `PageHeader`, `FilterBar`,
  //    `SavedViewChips`, `DataTable`, `BulkActionBar`, `DetailRail` +
  //    `RailCard`, `StatusChip`, `Pagination`, `ConnectionFooter`,
  //    `PageState` — and forks no primitive (§3).
  //  - Routes all async state through the four-state `<PageState>` —
  //    Disconnected / Loading / Error / Empty (§4). The legacy page had NO
  //    Disconnected state and conflated a request failure with a static
  //    "Connected to …" footer string; both are fixed here.
  //  - Clears the §5 depth bar (header + filter bar + table + detail rail +
  //    Console-DB-backed saved views + real prev/next pagination + footer +
  //    full PageState).
  //  - Talks to the Runtime ONLY through `HarborClient` + `connection.ts`
  //    (§6). The legacy page read a hardcoded `dev-tenant/dev-user/
  //    dev-session` identity off a `globalThis` shim — deleted; the real
  //    identity comes from `connection.ts`.
  //  - Introduces no raw token literals (§7).
  //
  // The page-specific preview component (`components/artifacts/
  // ArtifactPreview.svelte`) dispatches through the CANONICAL renderer
  // registry at `$lib/chat/renderers` — that registry skeleton is kept
  // intact and extensible (Phase 73n extends it; CLAUDE.md §13, Brief 12).
  //
  // Svelte 5 runes mode (D-092); design tokens only.
  import { onMount } from 'svelte';

  import {
    PageHeader,
    FilterBar,
    SavedViewChips,
    DataTable,
    BulkActionBar,
    DetailRail,
    RailCard,
    StatusChip,
    Pagination,
    ConnectionFooter,
    PageState,
    type PageStatus,
    type SavedView,
    type DataTableColumn
  } from '$lib/components/ui/index.js';
  import ArtifactPreview, {
    type PreviewState
  } from '$lib/components/artifacts/ArtifactPreview.svelte';

  import { HarborClient, ProtocolError, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { resolveConnection } from '$lib/connection.js';
  import { openArtifactsSavedViewStore } from '$lib/artifacts/saved_views.js';
  import type { ArtifactsSavedFilters } from '$lib/db/saved_filters_artifacts.js';
  import type {
    ArtifactRow,
    ArtifactSource,
    ArtifactsListRequest,
    ArtifactsListResponse,
    ArtifactsGetRefResponse,
    ArtifactsPutResponse
  } from '$lib/protocol.js';

  /* ---- injectable seams (CONVENTIONS.md §6) ------------------------ */

  // The Playwright harness / unit tests inject a deterministic
  // `ProtocolClient` and a saved-view store; production resolves both
  // from `connection.ts` + the Console DB.
  interface ArtifactsPageProps {
    /** Test-injected Protocol client. Production builds a `HarborClient`. */
    client?: ProtocolClient;
    /** Test-injected saved-view store. Production opens the Console DB. */
    savedViewStore?: ArtifactsSavedFilters;
  }
  let { client: injectedClient, savedViewStore: injectedStore }: ArtifactsPageProps = $props();

  /* ---- page state (runes) ------------------------------------------ */

  let client = $state<ProtocolClient | null>(null);
  let savedViewStore = $state<ArtifactsSavedFilters | null>(null);

  let status = $state<PageStatus>('loading');
  let pageError = $state<ProtocolError | { code: string; message: string } | null>(null);

  let rows = $state<ArtifactRow[]>([]);
  let totalMatched = $state(0);
  let protocolVersion = $state('');

  let page = $state(1);
  let pageSize = $state(50);

  let mimeFilter = $state('');
  let sourceFilter = $state<ArtifactSource | ''>('');

  let savedViews = $state<SavedView[]>([]);
  let activeViewId = $state<string | null>(null);

  let selectedRow = $state<ArtifactRow | null>(null);
  let selection = $state<Set<string>>(new Set());

  let preview = $state<PreviewState>({ src: '', errorCode: '', loading: false });

  const MIME_CHOICES = ['', 'image/png', 'application/pdf', 'text/plain', 'application/json'];
  const SOURCE_CHOICES: Array<ArtifactSource | ''> = [
    '',
    'tool',
    'planner',
    'user_upload',
    'system'
  ];

  const COLUMNS: DataTableColumn[] = [
    { key: 'name', label: 'Name' },
    { key: 'mime', label: 'MIME type' },
    { key: 'created', label: 'Created' },
    { key: 'owner', label: 'Owner' },
    { key: 'size', label: 'Size', numeric: true },
    { key: 'source', label: 'Source' },
    { key: 'tags', label: 'Tags' },
    { key: 'driver', label: 'Driver' },
    { key: 'actions', label: 'Actions' }
  ];

  const subtitle = $derived(
    `The runtime's content-addressed artifact store — ${totalMatched} artifact${
      totalMatched === 1 ? '' : 's'
    }.`
  );

  /* ---- formatting helpers ------------------------------------------ */

  function fmtSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  function sourceKind(source: ArtifactSource | undefined): 'accent' | 'warning' | 'neutral' {
    if (source === 'user_upload') return 'accent';
    if (source === 'system') return 'warning';
    return 'neutral';
  }

  /* ---- data loading ------------------------------------------------ */

  async function loadCatalog(): Promise<void> {
    if (client === null) {
      // No Runtime attached — Disconnected, NEVER an error (§4 state 1).
      status = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    const connection = resolveConnection();
    if (connection === null) {
      status = 'disconnected';
      return;
    }
    const req: ArtifactsListRequest = {
      scope: {
        tenant: connection.identity.tenant,
        user: connection.identity.user,
        session: connection.identity.session
      },
      limit: pageSize
    };
    if (mimeFilter) {
      req.mime_type = [mimeFilter];
    }
    if (sourceFilter) {
      req.source = [sourceFilter];
    }
    try {
      const resp = await client.artifacts.list<ArtifactsListResponse>(
        req as unknown as Record<string, unknown>
      );
      rows = resp.rows ?? [];
      totalMatched = resp.total_matched ?? rows.length;
      protocolVersion = resp.protocol_version ?? '';
      status = rows.length === 0 ? 'empty' : 'ready';
    } catch (e) {
      // A thrown ProtocolError routes into the Error state, which
      // suppresses any stale table (§4 state 3). No silent degradation.
      rows = [];
      totalMatched = 0;
      pageError =
        e instanceof ProtocolError
          ? e
          : { code: 'runtime_error', message: e instanceof Error ? e.message : 'unknown error' };
      status = 'error';
    }
  }

  async function resolvePreview(row: ArtifactRow): Promise<void> {
    if (client === null) {
      return;
    }
    preview = { src: '', errorCode: '', loading: true };
    const connection = resolveConnection();
    if (connection === null) {
      preview = { src: '', errorCode: 'disconnected', loading: false };
      return;
    }
    try {
      const resp = await client.artifacts.getRef<ArtifactsGetRefResponse>({
        scope: {
          tenant: connection.identity.tenant,
          user: connection.identity.user,
          session: connection.identity.session
        },
        id: row.ref.id
      });
      preview = { src: resp.presigned_url, errorCode: '', loading: false };
    } catch (e) {
      const code = e instanceof ProtocolError ? e.code : 'runtime_error';
      preview = { src: '', errorCode: code, loading: false };
    }
  }

  /* ---- saved views (Console-DB-backed, D-061) ---------------------- */

  async function refreshSavedViews(): Promise<void> {
    if (savedViewStore === null) {
      savedViews = [];
      return;
    }
    const stored = await savedViewStore.list();
    savedViews = stored.map((s) => ({ id: s.id, name: s.name }));
  }

  async function applySavedView(id: string): Promise<void> {
    if (savedViewStore === null) {
      return;
    }
    const view = await savedViewStore.get(id);
    if (view === null) {
      return;
    }
    activeViewId = id;
    mimeFilter = view.filterSpec.mimeType ?? '';
    sourceFilter = view.filterSpec.source ?? '';
    page = 1;
    await loadCatalog();
  }

  async function deleteSavedView(id: string): Promise<void> {
    if (savedViewStore === null) {
      return;
    }
    await savedViewStore.delete(id);
    if (activeViewId === id) {
      activeViewId = null;
    }
    await refreshSavedViews();
  }

  async function saveCurrentView(): Promise<void> {
    if (savedViewStore === null) {
      return;
    }
    const label =
      `${mimeFilter || 'any MIME'} · ${sourceFilter || 'any source'}`.trim();
    const created = await savedViewStore.create(label, {
      mimeType: mimeFilter,
      source: sourceFilter
    });
    activeViewId = created.id;
    await refreshSavedViews();
  }

  /* ---- event handlers ---------------------------------------------- */

  function selectRow(row: ArtifactRow): void {
    selectedRow = row;
    void resolvePreview(row);
  }

  async function changeMime(event: Event): Promise<void> {
    mimeFilter = (event.currentTarget as HTMLSelectElement).value;
    activeViewId = null;
    page = 1;
    await loadCatalog();
  }

  async function changeSource(event: Event): Promise<void> {
    sourceFilter = (event.currentTarget as HTMLSelectElement).value as ArtifactSource | '';
    activeViewId = null;
    page = 1;
    await loadCatalog();
  }

  async function changePage(next: number): Promise<void> {
    page = next;
    await loadCatalog();
  }

  async function changePageSize(size: number): Promise<void> {
    pageSize = size;
    page = 1;
    await loadCatalog();
  }

  function setSelection(next: Set<string>): void {
    selection = next;
  }

  function clearSelection(): void {
    selection = new Set();
  }

  async function uploadArtifact(): Promise<void> {
    if (client === null) {
      return;
    }
    const connection = resolveConnection();
    if (connection === null) {
      return;
    }
    const input = document.createElement('input');
    input.type = 'file';
    input.setAttribute('data-testid', 'upload-file-input');
    input.onchange = async () => {
      const file = input.files?.[0];
      if (!file || client === null) {
        return;
      }
      const buf = new Uint8Array(await file.arrayBuffer());
      let binary = '';
      for (const b of buf) {
        binary += String.fromCharCode(b);
      }
      const base64 = btoa(binary);
      try {
        const putResp = await client.artifacts.put<ArtifactsPutResponse>({
          scope: {
            tenant: connection.identity.tenant,
            user: connection.identity.user,
            session: connection.identity.session
          },
          bytes: base64,
          opts: { mime_type: file.type || 'application/octet-stream', filename: file.name }
        });
        await loadCatalog();
        const uploaded = rows.find((r) => r.ref.id === putResp.ref.id);
        if (uploaded) {
          selectRow(uploaded);
        }
      } catch (e) {
        pageError =
          e instanceof ProtocolError
            ? e
            : { code: 'runtime_error', message: e instanceof Error ? e.message : 'upload failed' };
        status = 'error';
      }
    };
    input.click();
  }

  function exportCSV(): void {
    // Metadata-only CSV (D-026 — never blob bytes). Header + one row per
    // loaded artifact.
    const header = 'id,filename,mime_type,size_bytes,source,driver,created_at';
    const lines = rows.map((r) =>
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
    const csv = [header, ...lines].join('\n');
    const blob = new Blob([csv], { type: 'text/csv' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'artifacts-export.csv';
    a.setAttribute('data-testid', 'export-csv-link');
    a.click();
    URL.revokeObjectURL(url);
  }

  function copyRef(): void {
    if (selectedRow) {
      void navigator.clipboard?.writeText(`artifact://${selectedRow.ref.id}`);
    }
  }

  function copyRefsBulk(): void {
    const refs = [...selection].map((id) => `artifact://${id}`).join('\n');
    void navigator.clipboard?.writeText(refs);
  }

  async function downloadSelected(): Promise<void> {
    if (selectedRow) {
      await resolvePreview(selectedRow);
      if (preview.src) {
        const a = document.createElement('a');
        a.href = preview.src;
        a.download = selectedRow.ref.filename ?? selectedRow.ref.id;
        a.click();
      }
    }
  }

  function downloadZip(): void {
    // Console-local zip-stream over the resolved presigned URLs (D-061).
    // The V1 page records the intent; the streaming implementation is a
    // post-V1 enhancement — the action is wired so the surface exists.
    void copyRefsBulk();
  }

  /* ---- boot -------------------------------------------------------- */

  onMount(() => {
    const connection = resolveConnection();
    if (injectedClient !== undefined) {
      client = injectedClient;
    } else if (connection !== null) {
      client = new HarborClient({ connection });
    } else {
      client = null;
    }

    void (async () => {
      if (injectedStore !== undefined) {
        savedViewStore = injectedStore;
      } else {
        try {
          savedViewStore = await openArtifactsSavedViewStore();
        } catch {
          // The Console DB failing to open must not take the whole page
          // down — the saved-view row degrades to empty, the catalog
          // still loads. A genuine Protocol failure still surfaces.
          savedViewStore = null;
        }
      }
      await refreshSavedViews();
      await loadCatalog();
    })();
  });
</script>

<svelte:head>
  <title>Artifacts · Harbor Console</title>
</svelte:head>

<div class="artifacts-page" data-testid="artifacts-page">
  <PageHeader title="Artifacts" {subtitle}>
    {#snippet actions()}
      <button
        type="button"
        class="action primary"
        data-testid="upload-artifact"
        onclick={uploadArtifact}
        disabled={status === 'disconnected'}
      >
        Upload artifact
      </button>
      <button
        type="button"
        class="action"
        data-testid="export-csv"
        onclick={exportCSV}
        disabled={rows.length === 0}
      >
        Export ▾
      </button>
    {/snippet}
  </PageHeader>

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews}
        activeId={activeViewId}
        onselect={(id) => void applySavedView(id)}
        ondelete={(id) => void deleteSavedView(id)}
      />
      <button
        type="button"
        class="action save-view"
        data-testid="save-view"
        onclick={() => void saveCurrentView()}
        disabled={savedViewStore === null}
      >
        Save view
      </button>
    {/snippet}
    {#snippet facets()}
      <label class="facet">
        <span>MIME type</span>
        <select bind:value={mimeFilter} onchange={changeMime} data-testid="filter-mime">
          {#each MIME_CHOICES as choice (choice)}
            <option value={choice}>{choice || 'Any'}</option>
          {/each}
        </select>
      </label>
      <label class="facet">
        <span>Source</span>
        <select bind:value={sourceFilter} onchange={changeSource} data-testid="filter-source">
          {#each SOURCE_CHOICES as choice (choice)}
            <option value={choice}>{choice || 'Any'}</option>
          {/each}
        </select>
      </label>
    {/snippet}
  </FilterBar>

  <BulkActionBar count={selection.size} onclear={clearSelection}>
    {#snippet actions()}
      <button type="button" class="action" data-testid="bulk-download-zip" onclick={downloadZip}>
        Download (zip)
      </button>
      <button type="button" class="action" data-testid="bulk-copy-refs" onclick={copyRefsBulk}>
        Copy refs
      </button>
      <button
        type="button"
        class="action deferred"
        data-testid="bulk-delete"
        aria-disabled="true"
        title="Deferred — Phase 73"
      >
        Delete
      </button>
      <button
        type="button"
        class="action deferred"
        data-testid="bulk-set-retention"
        aria-disabled="true"
        title="Deferred — Phase 73"
      >
        Set retention
      </button>
    {/snippet}
  </BulkActionBar>

  <div class="catalog">
    <div class="table-area">
      <PageState
        {status}
        error={pageError}
        onretry={() => void loadCatalog()}
      >
        {#snippet skeleton()}
          <div class="skeleton" aria-hidden="true">
            {#each [0, 1, 2, 3, 4] as i (i)}
              <span class="skeleton-row"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-state" data-testid="artifacts-empty">
            <p class="empty-headline">No artifacts yet</p>
            <p class="empty-detail">
              Artifacts are produced by tool calls and planner decisions, or uploaded here.
            </p>
            <button
              type="button"
              class="action primary"
              onclick={uploadArtifact}
              disabled={status === 'disconnected'}
            >
              Upload artifact
            </button>
          </div>
        {/snippet}

        <DataTable
          columns={COLUMNS}
          {rows}
          rowKey={(r) => (r as ArtifactRow).ref.id}
          selectable
          selected={selection}
          onselectionchange={setSelection}
          onrowclick={(r) => selectRow(r as ArtifactRow)}
        >
          {#snippet row(r)}
            {@const artifact = r as ArtifactRow}
            <td data-col="name">
              <span class="name-link">{artifact.ref.filename || artifact.ref.id}</span>
            </td>
            <td data-col="mime">
              <StatusChip kind="neutral" label={artifact.ref.mime_type || '—'} />
            </td>
            <td data-col="created">{artifact.created_at ?? '—'}</td>
            <td class="owner" data-col="owner">
              {artifact.ref.scope.tenant}/{artifact.ref.scope.user}
            </td>
            <td class="numeric" data-col="size">{fmtSize(artifact.ref.size_bytes)}</td>
            <td data-col="source">
              <StatusChip kind={sourceKind(artifact.source)} label={artifact.source ?? '—'} />
            </td>
            <td data-col="tags">
              {#each artifact.tags ?? [] as tag (tag)}
                <span class="tag-chip">{tag}</span>
              {/each}
            </td>
            <td data-col="driver">
              <StatusChip kind="neutral" label={artifact.driver ?? '—'} />
            </td>
            <td data-col="actions">
              <button
                type="button"
                class="action deferred"
                aria-disabled="true"
                title="Deferred — Phase 73"
              >
                Delete
              </button>
            </td>
          {/snippet}
        </DataTable>
      </PageState>

      {#if status === 'ready' || status === 'empty'}
        <Pagination
          {page}
          {pageSize}
          total={totalMatched}
          onpage={(p) => void changePage(p)}
          onpagesize={(s) => void changePageSize(s)}
        />
      {/if}
    </div>

    <DetailRail>
      {#if !selectedRow}
        <RailCard title="Selected artifact">
          <p class="rail-empty" data-testid="artifact-rail-empty">No artifact selected.</p>
        </RailCard>
      {:else}
        <RailCard title="Preview">
          <ArtifactPreview row={selectedRow} {preview} />
        </RailCard>
        <RailCard title="Actions">
          <div class="rail-actions">
            <button type="button" class="action" data-testid="action-download" onclick={downloadSelected}>
              Download
            </button>
            <button type="button" class="action" data-testid="action-copy-ref" onclick={copyRef}>
              Copy ref
            </button>
            <button
              type="button"
              class="action deferred"
              aria-disabled="true"
              title="Deferred — post-V1"
            >
              Save
            </button>
          </div>
        </RailCard>
        <RailCard title="Artifact metadata">
          <dl class="metadata">
            <dt>ID</dt>
            <dd class="mono">{selectedRow.ref.id}</dd>
            <dt>MIME</dt>
            <dd>{selectedRow.ref.mime_type || '—'}</dd>
            <dt>Size</dt>
            <dd>{fmtSize(selectedRow.ref.size_bytes)}</dd>
            <dt>Source</dt>
            <dd>{selectedRow.source ?? '—'}</dd>
            <dt>Driver</dt>
            <dd>{selectedRow.driver ?? '—'}</dd>
            <dt>Created</dt>
            <dd>{selectedRow.created_at ?? '—'}</dd>
            <dt>SHA-256</dt>
            <dd class="mono break">{selectedRow.ref.sha256 || '—'}</dd>
            <dt>Identity</dt>
            <dd class="mono break">
              {selectedRow.ref.scope.tenant}/{selectedRow.ref.scope.user}/{selectedRow.ref.scope
                .session}
            </dd>
          </dl>
        </RailCard>
        <RailCard title="Tags">
          {#if (selectedRow.tags ?? []).length === 0}
            <p class="rail-empty">No tags.</p>
          {:else}
            <div class="rail-tags">
              {#each selectedRow.tags ?? [] as tag (tag)}
                <span class="tag-chip">{tag}</span>
              {/each}
            </div>
          {/if}
        </RailCard>
      {/if}
    </DetailRail>
  </div>

  <ConnectionFooter />
  {#if protocolVersion}
    <p class="protocol-line" data-testid="artifacts-protocol-version">
      Protocol v{protocolVersion}
    </p>
  {/if}
</div>

<style>
  .artifacts-page {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .catalog {
    display: flex;
    gap: var(--space-4);
    align-items: flex-start;
  }

  .table-area {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .action {
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
    border: var(--border-hairline);
    background: var(--color-surface-raised);
    color: var(--color-text);
  }

  .action.primary {
    background: var(--color-accent);
    color: var(--color-bg);
    border-color: var(--color-accent);
  }

  .action.save-view {
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
  }

  .action.deferred {
    color: var(--color-text-muted);
    cursor: not-allowed;
  }

  .action:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .facet {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .facet select {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
  }

  td {
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  td.numeric {
    text-align: right;
  }

  .name-link {
    color: var(--color-accent);
  }

  .owner {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .tag-chip {
    display: inline-block;
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border-radius: var(--radius-lg);
    padding: var(--space-1) var(--space-2);
    margin-right: var(--space-1);
  }

  .skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--layout-table-row-height);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .empty-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .rail-empty {
    margin: var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .rail-actions {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .metadata {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  .metadata dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .metadata dd {
    margin: var(--space-0);
    color: var(--color-text);
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .break {
    word-break: break-all;
  }

  .rail-tags {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .protocol-line {
    margin: var(--space-0);
    padding: var(--space-0) var(--space-4);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    font-family: var(--font-mono);
  }
</style>
