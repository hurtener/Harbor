<script lang="ts">
  // Console Artifacts page — Phase 73l (D-120).
  //
  // The operator's catalog + preview surface over the runtime's content-
  // addressed artifact store. Anatomy per page-artifacts.md §12: filter
  // bar + saved-view chips + Upload + Export ▾ + virtualised table +
  // selected-artifact right rail (Preview / Actions / Metadata / Tags) +
  // bulk-action toolbar + footer.
  //
  // The page talks to the Runtime ONLY through the typed Protocol client
  // `$lib/protocol` (CLAUDE.md §4.5 #5/#11, §13) — there are ZERO hand-
  // rolled `fetch` calls in this component. Heavy bytes never cross
  // inline: artifacts.list returns metadata rows, the preview src is a
  // presigned URL from artifacts.get_ref (D-022 / D-026).
  import {
    HTTPProtocolClient,
    ProtocolRequestError,
    type ArtifactRow,
    type ArtifactSource,
    type ArtifactsListRequest,
    type ProtocolClient
  } from '$lib/protocol';
  import ArtifactsTable from './artifacts_table.svelte';
  import BulkToolbar from './bulk_toolbar.svelte';
  import FilterBar from './filter_bar.svelte';
  import RightRail from './right_rail.svelte';

  // The Runtime base URL + dev identity. A real deployment resolves these
  // from the Console DB profile (Phase 72h); for V1 the page reads them
  // from window globals a host harness can set (the Playwright spec does
  // exactly this), defaulting to the local dev Runtime.
  interface ArtifactsPageGlobals {
    __HARBOR_RUNTIME_URL__?: string;
    __HARBOR_IDENTITY__?: { tenant: string; user: string; session: string };
    __HARBOR_PROTOCOL_CLIENT__?: ProtocolClient;
  }
  const g = globalThis as unknown as ArtifactsPageGlobals;
  const runtimeURL = g.__HARBOR_RUNTIME_URL__ ?? 'http://127.0.0.1:18080';
  const identity = g.__HARBOR_IDENTITY__ ?? {
    tenant: 'dev-tenant',
    user: 'dev-user',
    session: 'dev-session'
  };
  // A host harness MAY inject a ProtocolClient (the Playwright spec
  // injects a deterministic in-page client); otherwise the page builds
  // the production HTTP client.
  const client: ProtocolClient = g.__HARBOR_PROTOCOL_CLIENT__ ?? new HTTPProtocolClient(runtimeURL);

  let rows = $state<ArtifactRow[]>([]);
  let totalMatched = $state(0);
  let listError = $state('');
  let listLoading = $state(true);

  let mimeFilter = $state('');
  let sourceFilter = $state<ArtifactSource | ''>('');

  let selectedRow = $state<ArtifactRow | null>(null);
  let selection = $state<Set<string>>(new Set());

  let preview = $state<{ src: string; errorCode: string; loading: boolean }>({
    src: '',
    errorCode: '',
    loading: false
  });

  const protocolVersion = $state('');
  let footerProtocolVersion = $state('');

  async function loadCatalog(): Promise<void> {
    listLoading = true;
    listError = '';
    const req: ArtifactsListRequest = {
      scope: { tenant: identity.tenant, user: identity.user, session: identity.session }
    };
    if (mimeFilter) {
      req.mime_type = [mimeFilter];
    }
    if (sourceFilter) {
      req.source = [sourceFilter];
    }
    try {
      const resp = await client.artifactsList(req);
      rows = resp.rows ?? [];
      totalMatched = resp.total_matched ?? rows.length;
      footerProtocolVersion = resp.protocol_version ?? '';
    } catch (e) {
      rows = [];
      totalMatched = 0;
      listError =
        e instanceof ProtocolRequestError
          ? `${e.code}: ${e.message}`
          : 'Failed to load artifacts';
    } finally {
      listLoading = false;
    }
  }

  async function resolvePreview(row: ArtifactRow): Promise<void> {
    preview = { src: '', errorCode: '', loading: true };
    try {
      const resp = await client.artifactsGetRef({
        scope: { tenant: identity.tenant, user: identity.user, session: identity.session },
        id: row.ref.id
      });
      preview = { src: resp.presigned_url, errorCode: '', loading: false };
    } catch (e) {
      const code = e instanceof ProtocolRequestError ? e.code : 'runtime_error';
      preview = { src: '', errorCode: code, loading: false };
    }
  }

  function selectRow(row: ArtifactRow): void {
    selectedRow = row;
    void resolvePreview(row);
  }

  async function uploadArtifact(): Promise<void> {
    const input = document.createElement('input');
    input.type = 'file';
    input.setAttribute('data-testid', 'upload-file-input');
    input.onchange = async () => {
      const file = input.files?.[0];
      if (!file) {
        return;
      }
      const buf = new Uint8Array(await file.arrayBuffer());
      let binary = '';
      for (const b of buf) {
        binary += String.fromCharCode(b);
      }
      const base64 = btoa(binary);
      try {
        const putResp = await client.artifactsPut({
          scope: { tenant: identity.tenant, user: identity.user, session: identity.session },
          bytes: base64,
          opts: { mime_type: file.type || 'application/octet-stream', filename: file.name }
        });
        await loadCatalog();
        const uploaded = rows.find((r) => r.ref.id === putResp.ref.id);
        if (uploaded) {
          selectRow(uploaded);
        }
      } catch (e) {
        listError =
          e instanceof ProtocolRequestError
            ? `upload failed — ${e.code}: ${e.message}`
            : 'upload failed';
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

  // Re-load whenever a server-side filter changes.
  $effect(() => {
    // Touch the filter state so the effect re-runs on change.
    void mimeFilter;
    void sourceFilter;
    void loadCatalog();
  });
</script>

<div class="artifacts-page" data-testid="artifacts-page">
  <header class="page-header">
    <h1>Artifacts</h1>
    <p class="subtitle">
      The runtime's content-addressed artifact store — {totalMatched} artifact{totalMatched ===
      1
        ? ''
        : 's'}.
    </p>
  </header>

  <FilterBar bind:mimeFilter bind:sourceFilter onUpload={uploadArtifact} onExport={exportCSV} />

  <BulkToolbar
    selectedCount={selection.size}
    onCopyRefs={copyRefsBulk}
    onDownloadZip={downloadZip}
  />

  <div class="catalog">
    <div class="table-area">
      {#if listError}
        <p class="error-banner" data-testid="artifacts-error">{listError}</p>
      {/if}
      {#if listLoading}
        <p class="status" data-testid="artifacts-loading">Loading artifacts…</p>
      {:else}
        <ArtifactsTable {rows} selectedId={selectedRow?.ref.id ?? null} onSelect={selectRow} bind:selection />
      {/if}
      <footer class="pagination" data-testid="artifacts-pagination">
        Rows: {rows.length} of {totalMatched}
      </footer>
    </div>

    <RightRail
      row={selectedRow}
      {preview}
      onDownload={downloadSelected}
      onCopyRef={copyRef}
    />
  </div>

  <footer class="page-footer" data-testid="artifacts-footer">
    Connected to {runtimeURL} | Protocol v{footerProtocolVersion || protocolVersion || '—'}
    | Console v0.0.0
  </footer>
</div>

<style>
  .artifacts-page {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-6);
  }

  .page-header h1 {
    margin: var(--space-0);
    font-size: var(--text-xl);
    color: var(--color-text);
  }

  .subtitle {
    margin: var(--space-1) var(--space-0) var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .catalog {
    display: grid;
    grid-template-columns: 2fr 1fr;
    gap: var(--space-4);
    align-items: start;
  }

  .table-area {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: var(--color-surface);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-md);
    padding: var(--space-3);
  }

  .error-banner {
    color: var(--color-danger);
    font-size: var(--text-sm);
    margin: var(--space-0);
  }

  .status {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .pagination {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    padding-top: var(--space-2);
  }

  .page-footer {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    border-top: var(--border-thin) solid var(--color-border);
    padding-top: var(--space-3);
  }
</style>
