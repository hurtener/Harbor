<script lang="ts">
  // Artifacts right rail — Phase 73l (D-120). The selected-artifact
  // detail panel per page-artifacts.md §12: Preview / Actions / Metadata
  // / Tags. Preview renders via the canonical renderer registry
  // (preview_pane.svelte). Download routes through artifacts.get_ref
  // (D-022 / D-026); Save / Copy ref are Console-local (D-061).
  import type { ArtifactRow } from '$lib/protocol';
  import PreviewPane from './preview_pane.svelte';

  interface PreviewState {
    src: string;
    errorCode: string;
    loading: boolean;
  }

  let {
    row,
    preview,
    onDownload,
    onCopyRef
  }: {
    row: ArtifactRow | null;
    preview: PreviewState;
    onDownload: () => void;
    onCopyRef: () => void;
  } = $props();
</script>

<aside class="right-rail" data-testid="artifact-right-rail">
  {#if !row}
    <p class="empty">No artifact selected.</p>
  {:else}
    <header class="rail-header">
      <h2>{row.ref.filename || row.ref.id}</h2>
      <code class="artifact-id">{row.ref.id}</code>
    </header>

    <section class="rail-section">
      <h3>Preview</h3>
      <PreviewPane {row} {preview} />
    </section>

    <section class="rail-section">
      <h3>Actions</h3>
      <div class="actions">
        <button type="button" data-testid="action-download" onclick={onDownload}>
          Download
        </button>
        <button type="button" data-testid="action-copy-ref" onclick={onCopyRef}>
          Copy ref
        </button>
        <button
          type="button"
          class="deferred"
          aria-disabled="true"
          title="Deferred — post-V1"
        >
          Save
        </button>
      </div>
    </section>

    <section class="rail-section">
      <h3>Artifact metadata</h3>
      <dl class="metadata">
        <dt>ID</dt>
        <dd>{row.ref.id}</dd>
        <dt>MIME</dt>
        <dd>{row.ref.mime_type || '—'}</dd>
        <dt>Size</dt>
        <dd>{row.ref.size_bytes} bytes</dd>
        <dt>Source</dt>
        <dd>{row.source ?? '—'}</dd>
        <dt>Driver</dt>
        <dd>{row.driver ?? '—'}</dd>
        <dt>Created</dt>
        <dd>{row.created_at ?? '—'}</dd>
        <dt>SHA-256</dt>
        <dd class="hash">{row.ref.sha256 || '—'}</dd>
        <dt>Identity</dt>
        <dd class="hash">
          {row.ref.scope.tenant}/{row.ref.scope.user}/{row.ref.scope.session}
        </dd>
      </dl>
    </section>

    <section class="rail-section">
      <h3>Tags</h3>
      {#if (row.tags ?? []).length === 0}
        <p class="empty">No tags.</p>
      {:else}
        <div class="tags">
          {#each row.tags ?? [] as tag (tag)}
            <span class="tag-chip">{tag}</span>
          {/each}
        </div>
      {/if}
    </section>
  {/if}
</aside>

<style>
  .right-rail {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-md);
    min-width: var(--space-8);
  }

  .empty {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .rail-header h2 {
    margin: var(--space-0);
    font-size: var(--text-lg);
    color: var(--color-text);
  }

  .artifact-id {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .rail-section h3 {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    text-transform: uppercase;
    color: var(--color-text-muted);
  }

  .actions {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  button {
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
    border: var(--border-thin) solid var(--color-border);
    background: var(--color-surface-raised);
    color: var(--color-text);
  }

  .deferred {
    color: var(--color-text-muted);
    cursor: not-allowed;
  }

  .metadata {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  dd {
    margin: var(--space-0);
    color: var(--color-text);
  }

  .hash {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    word-break: break-all;
  }

  .tags {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .tag-chip {
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border-radius: var(--radius-lg);
    padding: var(--space-1) var(--space-3);
  }
</style>
