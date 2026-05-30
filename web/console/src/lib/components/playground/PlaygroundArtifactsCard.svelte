<script lang="ts">
  // Harbor Console — Playground Recent Artifacts card (Phase 73n /
  // D-130, Phase 108 / D-167).
  //
  // The right-rail card listing artifacts produced in the session.
  // Phase 108 adds: kind icon, age, formatted size, mono filename.
  //
  // Design tokens only.

  /** A recent artifact reference shown in the rail. */
  export interface RecentArtifactEntry {
    id: string;
    filename: string;
    mime: string;
    sizeBytes?: number;
    /** ISO-8601 creation timestamp. */
    createdAt?: string;
  }

  function mimeIcon(mime: string): string {
    if (mime.startsWith('text/')) return '📄';
    if (mime.startsWith('image/')) return '🖼';
    if (mime === 'application/pdf') return '📕';
    if (mime.startsWith('audio/')) return '🎵';
    if (mime === 'application/json') return '📋';
    return '📦';
  }

  function formatSize(bytes: number | undefined): string {
    if (bytes === undefined) return '';
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  function formatAge(iso: string | undefined): string {
    if (!iso) return '';
    try {
      const then = new Date(iso).getTime();
      const now = Date.now();
      const diffMin = Math.floor((now - then) / 60000);
      if (diffMin < 1) return 'just now';
      if (diffMin < 60) return `${diffMin}m ago`;
      const diffH = Math.floor(diffMin / 60);
      if (diffH < 24) return `${diffH}h ago`;
      const diffD = Math.floor(diffH / 24);
      return `${diffD}d ago`;
    } catch {
      return '';
    }
  }

  let {
    artifacts,
    onpreview
  }: {
    artifacts: RecentArtifactEntry[];
    onpreview: (id: string) => void;
  } = $props();
</script>

<div class="recent-artifacts-card" data-testid="playground-recent-artifacts-card">
  {#if artifacts.length === 0}
    <p class="empty-note" data-testid="recent-artifacts-empty">
      No artifacts produced yet.
    </p>
  {:else}
    <ul class="artifact-list">
      {#each artifacts as artifact (artifact.id)}
        <li class="artifact-row">
          <button
            type="button"
            class="artifact-button"
            data-testid="recent-artifact"
            data-artifact-id={artifact.id}
            onclick={() => onpreview(artifact.id)}
          >
            <span class="artifact-icon" aria-hidden="true">
              {mimeIcon(artifact.mime)}
            </span>
            <span class="artifact-name mono">{artifact.filename}</span>
            <span class="artifact-meta">
              {#if artifact.sizeBytes !== undefined}
                <span class="artifact-size tabular">{formatSize(artifact.sizeBytes)}</span>
              {/if}
              {#if artifact.createdAt}
                <span class="artifact-age">{formatAge(artifact.createdAt)}</span>
              {/if}
            </span>
            <span class="artifact-open" aria-hidden="true">↗</span>
          </button>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .recent-artifacts-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .empty-note {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .artifact-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .artifact-button {
    width: 100%;
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    cursor: pointer;
    text-align: left;
  }

  .artifact-icon {
    font-size: var(--text-base);
    flex-shrink: 0;
  }

  .artifact-name {
    font-size: var(--text-sm);
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    flex: 1;
    min-width: 0;
  }

  .mono {
    font-family: var(--font-mono);
  }

  .tabular {
    font-variant-numeric: var(--font-variant-tabular);
  }

  .artifact-meta {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-shrink: 0;
  }

  .artifact-size {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .artifact-age {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .artifact-open {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    flex-shrink: 0;
  }
</style>
