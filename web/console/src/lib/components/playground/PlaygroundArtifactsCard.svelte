<script lang="ts">
  // Harbor Console — Playground Recent Artifacts card (Phase 73n /
  // D-130).
  //
  // The right-rail card listing artifacts produced in the session.
  // Heavy content flows by reference (D-026) — the card shows metadata
  // + a preview affordance, never inline bytes. Selecting an artifact
  // fires `onpreview` so the page can open its preview.
  //
  // Design tokens only.

  /** A recent artifact reference shown in the rail. */
  export interface RecentArtifactEntry {
    id: string;
    filename: string;
    mime: string;
    sizeBytes?: number;
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
            <span class="artifact-name">{artifact.filename}</span>
            <span class="artifact-mime">{artifact.mime}</span>
            {#if artifact.sizeBytes !== undefined}
              <span class="artifact-size">{artifact.sizeBytes} B</span>
            {/if}
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
    align-items: baseline;
    gap: var(--space-2);
    padding: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    cursor: pointer;
    text-align: left;
  }

  .artifact-name {
    font-size: var(--text-sm);
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .artifact-mime {
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--color-text-muted);
  }

  .artifact-size {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    margin-left: auto;
  }
</style>
