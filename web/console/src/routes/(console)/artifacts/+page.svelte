<script lang="ts">
  // Harbor Console — Artifacts page (`/artifacts`) — Phase 108o rebuild
  // (D-187; supersedes the Phase 73l / D-120 pre-chrome layout).
  //
  // The browser for the runtime's content-addressed artifact store. Phase 108o
  // rethemes it to the carded, viewport-locked Tools-108k / Memory-108n
  // composition (a filter card + a TABLE-left + a stacked right rail of
  // preview / actions / metadata / tags), refactors the ~916-line king file
  // into an `ArtifactsPageState` controller + pure `derive.ts` projections + a
  // focused `ArtifactsTable` component, and drops the per-page page header (the
  // breadcrumb / ⌘K / footer are app-shell chrome, 108b).
  //
  // Every datum + action is real-wired (PAGE-POLISH §3 — live-verified against
  // the validation runtime's seeded artifact store):
  //   - catalog rows ← `artifacts.list`
  //   - preview / download ← `artifacts.get_ref` (presigned URL; heavy bytes
  //     NEVER inline — D-022 / D-026)
  //   - upload ← `artifacts.put`
  //   - row / bulk / rail Delete ← the NEW admin `artifacts.delete` (D-187) —
  //     disabled-with-tooltip without the `admin` claim (D-079), audited,
  //     never a fabricated success (§13)
  //   - saved-view chips + CSV export (metadata only) ← Console-local (D-061)
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient + connection.ts
  // only — no hand-rolled fetch (CONVENTIONS.md §6).
  import { onMount } from 'svelte';
  import {
    FilterBar,
    SavedViewChips,
    BulkActionBar,
    DetailRail,
    RailCard,
    Pagination,
    PageState
  } from '$lib/components/ui';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import ArtifactsTable from '$lib/components/artifacts/ArtifactsTable.svelte';
  import ArtifactPreview from '$lib/components/artifacts/ArtifactPreview.svelte';
  import { ArtifactsPageState } from '$lib/artifacts/state.svelte.js';
  import { fmtSize } from '$lib/artifacts/derive.js';
  import type { ArtifactSource } from '$lib/protocol/artifacts.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';
  import type { ArtifactsSavedFilters } from '$lib/db/saved_filters_artifacts.js';

  let { client: injectedClient, savedViewStore: injectedStore }: {
    client?: ProtocolClient;
    savedViewStore?: ArtifactsSavedFilters;
  } = $props();

  const art = new ArtifactsPageState();
  const disconnected = $derived(art.disconnected);

  const MIME_CHOICES = ['', 'image/png', 'application/pdf', 'text/plain', 'application/json'];
  const SOURCE_CHOICES: Array<ArtifactSource | ''> = ['', 'tool', 'planner', 'user_upload', 'system'];
  const SKELETON_ROWS = [0, 1, 2, 3, 4];

  const deleteGate = 'Requires the admin scope claim — artifacts.delete is an admin Protocol method (D-079).';

  onMount(() => {
    art.load(injectedClient, injectedStore);
  });

  /** The file picker is DOM-only; the controller does the real artifacts.put. */
  function uploadArtifact(): void {
    if (disconnected) return;
    const input = document.createElement('input');
    input.type = 'file';
    input.setAttribute('data-testid', 'upload-file-input');
    input.onchange = () => {
      const file = input.files?.[0];
      if (file) void art.uploadFile(file);
    };
    input.click();
  }

  function exportCSV(): void {
    const blob = new Blob([art.exportCSV()], { type: 'text/csv' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'artifacts-export.csv';
    a.setAttribute('data-testid', 'export-csv-link');
    a.click();
    URL.revokeObjectURL(url);
  }

  function copyRef(): void {
    if (art.selectedRow) void navigator.clipboard?.writeText(`artifact://${art.selectedRow.ref.id}`);
  }

  function copyRefsBulk(): void {
    const refs = [...art.selection].map((id) => `artifact://${id}`).join('\n');
    void navigator.clipboard?.writeText(refs);
  }

  async function downloadSelected(): Promise<void> {
    const row = art.selectedRow;
    if (!row) return;
    await art.resolvePreview(row);
    if (art.preview.src) {
      const a = document.createElement('a');
      a.href = art.preview.src;
      a.download = row.ref.filename ?? row.ref.id;
      a.click();
    }
  }

  function confirmDelete(id: string): void {
    if (!art.canAdmin) return;
    if (globalThis.confirm?.(`Evict artifact ${id}? This cannot be undone.`)) {
      void art.deleteArtifact(id);
    }
  }
</script>

<svelte:head>
  <title>Artifacts · Harbor Console</title>
</svelte:head>

<section class="artifacts-page" data-testid="artifacts-page">
  <section class="panel card filter-card">
    <FilterBar>
      {#snippet saved()}
        <SavedViewChips
          views={art.savedViews}
          activeId={art.activeViewId}
          onselect={(id) => void art.applySavedView(id)}
          ondelete={(id) => void art.deleteSavedView(id)}
        />
        <button
          type="button"
          class="bar-action"
          data-testid="save-view"
          disabled={art.savedViewStore === null || disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => void art.saveCurrentView()}
        >
          Save view
        </button>
      {/snippet}

      {#snippet facets()}
        <label class="facet">
          <span>MIME type</span>
          <select
            value={art.mimeFilter}
            data-testid="filter-mime"
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onchange={(e) => art.applyMime(e.currentTarget.value)}
          >
            {#each MIME_CHOICES as choice (choice)}<option value={choice}>{choice || 'Any'}</option>{/each}
          </select>
        </label>
        <label class="facet">
          <span>Source</span>
          <select
            value={art.sourceFilter}
            data-testid="filter-source"
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onchange={(e) => art.applySource(e.currentTarget.value as ArtifactSource | '')}
          >
            {#each SOURCE_CHOICES as choice (choice)}<option value={choice}>{choice || 'Any'}</option>{/each}
          </select>
        </label>
      {/snippet}

      {#snippet actions()}
        <button
          type="button"
          class="bar-action"
          data-testid="artifacts-clear"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => art.clearFilters()}
        >
          Clear
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="artifacts-refresh"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => art.refresh()}
        >
          Refresh
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="upload-artifact"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={uploadArtifact}
        >
          Upload artifact
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="export-csv"
          disabled={art.rows.length === 0 || disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={exportCSV}
        >
          Export CSV
        </button>
      {/snippet}
    </FilterBar>

    <BulkActionBar count={art.selection.size} onclear={() => art.setSelection(new Set())}>
      {#snippet actions()}
        <button type="button" class="bar-action" data-testid="bulk-copy-refs" onclick={copyRefsBulk}>
          Copy refs
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="bulk-delete"
          disabled={!art.canAdmin || art.mutationBusy}
          title={art.canAdmin ? 'Evict the selected artifacts (audited)' : deleteGate}
          onclick={() => void art.deleteSelected()}
        >
          {art.mutationBusy ? 'Deleting…' : 'Delete selected'}
        </button>
      {/snippet}
    </BulkActionBar>
    {#if art.mutationResult !== null}
      <p
        class="inline-result"
        class:ok={art.mutationResult.ok}
        class:err={!art.mutationResult.ok}
        data-testid="artifacts-mutation-result"
      >
        {art.mutationResult.message}
      </p>
    {/if}
  </section>

  <div class="layout">
    <section class="panel card table-card">
      <PageState status={art.displayStatus} error={art.pageError} onretry={() => art.refresh()}>
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each SKELETON_ROWS as i (i)}<span class="skeleton-row"></span>{/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="artifacts-empty">
            <p class="empty-headline">No artifacts yet</p>
            <p class="empty-detail">
              Artifacts are produced by tool calls and planner decisions, or
              uploaded here. They also appear after a run on
              <a href="/live-runtime">Live Runtime</a>.
            </p>
            <button type="button" class="bar-action" disabled={disconnected} onclick={uploadArtifact}>
              Upload artifact
            </button>
          </div>
        {/snippet}

        <div class="table-scroll">
          <ArtifactsTable
            rows={art.rows}
            selected={art.selection}
            activeId={art.selectedRow?.ref.id ?? null}
            canAdmin={art.canAdmin}
            onselect={(r) => art.selectRow(r)}
            onselectionchange={(s) => art.setSelection(s)}
            ondelete={confirmDelete}
          />
        </div>
      </PageState>

      {#if art.displayStatus === 'ready' || art.displayStatus === 'empty'}
        <Pagination
          page={art.page}
          pageSize={art.pageSize}
          total={art.totalMatched}
          onpage={(p) => art.changePage(p)}
          onpagesize={(s) => art.changePageSize(s)}
        />
      {/if}
    </section>

    <DetailRail>
      {#if !art.selectedRow}
        <RailCard title="Selected artifact">
          <p class="rail-empty" data-testid="artifact-rail-empty">
            Select an artifact to preview it and see its metadata.
          </p>
        </RailCard>
      {:else}
        <RailCard title="Preview">
          <ArtifactPreview row={art.selectedRow} preview={art.preview} />
        </RailCard>
        <RailCard title="Actions">
          <div class="rail-actions">
            <button type="button" class="bar-action" data-testid="action-download" onclick={() => void downloadSelected()}>
              Download
            </button>
            <button type="button" class="bar-action" data-testid="action-copy-ref" onclick={copyRef}>
              Copy ref
            </button>
            <button
              type="button"
              class="bar-action danger"
              data-testid="action-delete"
              disabled={!art.canAdmin || art.mutationBusy}
              title={art.canAdmin ? 'Evict this artifact (audited)' : deleteGate}
              onclick={() => confirmDelete(art.selectedRow!.ref.id)}
            >
              Delete
            </button>
          </div>
        </RailCard>
        <RailCard title="Artifact metadata">
          <dl class="metadata">
            <dt>ID</dt><dd class="mono break">{art.selectedRow.ref.id}</dd>
            <dt>MIME</dt><dd>{art.selectedRow.ref.mime_type || '—'}</dd>
            <dt>Size</dt><dd>{fmtSize(art.selectedRow.ref.size_bytes)}</dd>
            <dt>Source</dt><dd>{art.selectedRow.source ?? '—'}</dd>
            <dt>Driver</dt><dd>{art.selectedRow.driver ?? '—'}</dd>
            <dt>Created</dt><dd>{art.selectedRow.created_at ?? '—'}</dd>
            <dt>SHA-256</dt><dd class="mono break">{art.selectedRow.ref.sha256 || '—'}</dd>
            <dt>Identity</dt>
            <dd class="mono break">
              {art.selectedRow.ref.scope.tenant}/{art.selectedRow.ref.scope.user}/{art.selectedRow.ref.scope.session}
            </dd>
          </dl>
        </RailCard>
        <RailCard title="Tags">
          {#if (art.selectedRow.tags ?? []).length === 0}
            <p class="rail-empty">No tags.</p>
          {:else}
            <div class="rail-tags">
              {#each art.selectedRow.tags ?? [] as tag (tag)}<span class="tag-chip">{tag}</span>{/each}
            </div>
          {/if}
        </RailCard>
      {/if}
    </DetailRail>
  </div>
</section>

<style>
  /* Viewport-locked: only the catalog table + the right rail scroll internally
     (PAGE-POLISH §6 — the Tools / Memory pattern). */
  .artifacts-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow: hidden;
  }

  .card {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-width: 0;
  }

  .filter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  .filter-card :global(.filter-bar) {
    padding: var(--space-1) var(--space-0);
    gap: var(--space-2);
  }

  .facet {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .facet select {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
  }

  .bar-action {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
    text-decoration: none;
  }

  .bar-action:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .bar-action.danger {
    color: var(--color-danger);
  }

  .inline-result {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .inline-result.ok {
    color: var(--color-success);
  }

  .inline-result.err {
    color: var(--color-danger);
  }

  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  .table-card {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    gap: var(--space-3);
  }

  .table-card :global(.page-state) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }

  .table-scroll {
    flex: 1;
    min-height: 0;
    overflow: auto;
  }

  .layout :global(.detail-rail) {
    min-height: 0;
    overflow-y: auto;
    scrollbar-gutter: stable;
  }

  .table-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--layout-table-row-height);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .empty-block {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-8) var(--space-4);
    text-align: center;
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
    max-width: var(--size-modal-width);
  }

  .empty-detail a {
    color: var(--color-accent);
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

  .tag-chip {
    display: inline-block;
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border-radius: var(--radius-lg);
    padding: var(--space-1) var(--space-2);
  }
</style>
