<script lang="ts">
  // Artifacts filter bar — Phase 73l (D-120). Faceted filter chips per
  // page-artifacts.md §12: MIME / Source. Plus the saved-view chip row
  // (Console-local per D-061), the Upload artifact button, and Export ▾.
  // The full faceted set (Size / Tenant / Session / Task / Created /
  // More filters) is stubbed as disabled-with-tooltip chips — the V1
  // page ships the MIME + Source facets live; the rest are post-V1 per
  // the spec §10 deferred posture.
  import type { ArtifactSource } from '$lib/protocol';

  let {
    mimeFilter = $bindable(''),
    sourceFilter = $bindable<ArtifactSource | ''>(''),
    onUpload,
    onExport,
    uploadDisabled = false
  }: {
    mimeFilter: string;
    sourceFilter: ArtifactSource | '';
    onUpload: () => void;
    onExport: () => void;
    uploadDisabled?: boolean;
  } = $props();

  const mimeChoices = ['', 'image/png', 'application/pdf', 'text/plain', 'application/json'];
  const sourceChoices: Array<ArtifactSource | ''> = ['', 'tool', 'planner', 'user_upload', 'system'];
  const savedViews = ['Large > 10 MB', 'Stale > 7d', 'User uploads', 'Tool outputs'];
</script>

<div class="filter-bar" data-testid="artifacts-filter-bar">
  <div class="facets">
    <label>
      <span>MIME type</span>
      <select bind:value={mimeFilter} data-testid="filter-mime">
        {#each mimeChoices as choice (choice)}
          <option value={choice}>{choice || 'Any'}</option>
        {/each}
      </select>
    </label>
    <label>
      <span>Source</span>
      <select bind:value={sourceFilter} data-testid="filter-source">
        {#each sourceChoices as choice (choice)}
          <option value={choice}>{choice || 'Any'}</option>
        {/each}
      </select>
    </label>
    <button class="facet-deferred" type="button" aria-disabled="true" title="Deferred — post-V1">
      Size ▾
    </button>
    <button class="facet-deferred" type="button" aria-disabled="true" title="Deferred — post-V1">
      Created ▾
    </button>
  </div>

  <div class="actions">
    <button
      class="primary"
      type="button"
      data-testid="upload-artifact"
      onclick={onUpload}
      disabled={uploadDisabled}
    >
      Upload artifact
    </button>
    <button class="secondary" type="button" data-testid="export-csv" onclick={onExport}>
      Export ▾
    </button>
  </div>
</div>

<div class="saved-views" data-testid="saved-views">
  <span class="saved-views-label">Saved views</span>
  {#each savedViews as view (view)}
    <span class="saved-view-chip">{view}</span>
  {/each}
</div>

<style>
  .filter-bar {
    display: flex;
    justify-content: space-between;
    gap: var(--space-4);
    flex-wrap: wrap;
    padding: var(--space-3);
    background: var(--color-surface);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  .facets,
  .actions {
    display: flex;
    gap: var(--space-3);
    align-items: flex-end;
    flex-wrap: wrap;
  }

  label {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  select {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
  }

  button {
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
    border: var(--border-thin) solid var(--color-border);
  }

  .primary {
    background: var(--color-accent);
    color: var(--color-text);
  }

  .secondary {
    background: var(--color-surface-raised);
    color: var(--color-text);
  }

  .facet-deferred {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    cursor: not-allowed;
  }

  .saved-views {
    display: flex;
    gap: var(--space-2);
    align-items: center;
    flex-wrap: wrap;
    padding: var(--space-2) var(--space-1);
  }

  .saved-views-label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .saved-view-chip {
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-lg);
    padding: var(--space-1) var(--space-3);
  }
</style>
