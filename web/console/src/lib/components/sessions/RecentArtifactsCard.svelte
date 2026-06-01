<script lang="ts">
  // Harbor Console — Sessions detail-view Recent Artifacts card (Phase
  // 73c / D-122). Renders the capped `recent_artifacts` slice from
  // `sessions.inspect` — mime icon + filename + size + age (mockup §12).
  // Metadata only — never inline bytes (D-026). Sessions-specific
  // component. Svelte 5 runes (D-092); design tokens only.
  import type { ArtifactRefSummary } from '$lib/sessions/types.js';
  import { formatRelative } from '$lib/sessions/format.js';

  let { artifacts }: { artifacts: ArtifactRefSummary[] } = $props();

  /** A compact, dependency-free size string. */
  function fmtSize(bytes: number): string {
    if (bytes < 1024) {
      return `${bytes} B`;
    }
    if (bytes < 1024 * 1024) {
      return `${(bytes / 1024).toFixed(1)} KiB`;
    }
    return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
  }
</script>

{#if artifacts.length === 0}
  <p class="empty" data-testid="artifacts-empty">
    No artifacts produced by this session.
  </p>
{:else}
  <ul class="list" data-testid="recent-artifacts">
    {#each artifacts as art, i (i)}
      <li class="item">
        <span class="name" title={art.filename}>{art.filename}</span>
        <span class="meta">{art.mime} · {fmtSize(art.size_bytes)}</span>
        <span class="age">{formatRelative(art.created_at)}</span>
      </li>
    {/each}
  </ul>
{/if}

<style>
  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    max-height: var(--layout-rail-list-max);
    overflow-y: auto;
  }

  .item {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
    padding-bottom: var(--space-2);
  }

  .name {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .meta,
  .age {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
