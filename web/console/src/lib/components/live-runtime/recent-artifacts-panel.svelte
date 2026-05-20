<script lang="ts" module>
  // Harbor Console — Live Runtime Recent Artifacts panel (Phase 73b /
  // D-126). The right-rail sub-panel listing the session's recent
  // artifacts by REFERENCE only (D-026 — never inline heavy bytes).
  // Each row links to the Artifacts page detail route.
  //
  // Svelte 5 runes mode (D-092); design tokens only.

  /** A by-reference artifact row — metadata only (D-026). */
  export interface RecentArtifact {
    id: string;
    mimeType?: string;
    sizeBytes: number;
  }
</script>

<script lang="ts">
  let { artifacts }: { artifacts: RecentArtifact[] } = $props();
</script>

<div class="recent-artifacts" data-testid="recent-artifacts-panel">
  {#if artifacts.length === 0}
    <p class="ra-empty">No artifacts produced this session yet.</p>
  {:else}
    <ul class="ra-list">
      {#each artifacts as art (art.id)}
        <li class="ra-row" data-testid="recent-artifact-row">
          <a class="ra-link" href={`/artifacts`}>{art.id}</a>
          <span class="ra-meta">{art.mimeType ?? 'unknown'} · {art.sizeBytes} B</span>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .recent-artifacts {
    font-size: var(--text-sm);
  }

  .ra-empty {
    margin: var(--space-0);
    color: var(--color-text-muted);
  }

  .ra-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .ra-row {
    display: flex;
    flex-direction: column;
    gap: var(--space-0);
  }

  .ra-link {
    color: var(--color-accent);
    text-decoration: none;
  }

  .ra-meta {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
</style>
