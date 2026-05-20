<script lang="ts">
  // Artifacts bulk-action toolbar — Phase 73l (D-120). Activates when ≥1
  // row is checked. Download (zip) and Copy refs are enabled (Console-
  // local per D-061); Delete and Set retention render disabled-with-
  // tooltip per page-artifacts.md §10 deferred list.
  let {
    selectedCount,
    onCopyRefs,
    onDownloadZip
  }: {
    selectedCount: number;
    onCopyRefs: () => void;
    onDownloadZip: () => void;
  } = $props();
</script>

{#if selectedCount > 0}
  <div class="bulk-toolbar" data-testid="bulk-toolbar">
    <span class="count">{selectedCount} selected</span>
    <button type="button" data-testid="bulk-download-zip" onclick={onDownloadZip}>
      Download (zip)
    </button>
    <button type="button" data-testid="bulk-copy-refs" onclick={onCopyRefs}>Copy refs</button>
    <button
      type="button"
      class="deferred"
      data-testid="bulk-delete"
      aria-disabled="true"
      title="Deferred — Phase 73"
    >
      Delete
    </button>
    <button
      type="button"
      class="deferred"
      data-testid="bulk-set-retention"
      aria-disabled="true"
      title="Deferred — Phase 73"
    >
      Set retention
    </button>
  </div>
{/if}

<style>
  .bulk-toolbar {
    display: flex;
    gap: var(--space-3);
    align-items: center;
    padding: var(--space-2) var(--space-3);
    background: var(--color-surface-raised);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  .count {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  button {
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
    border: var(--border-thin) solid var(--color-border);
    background: var(--color-surface);
    color: var(--color-text);
  }

  .deferred {
    color: var(--color-text-muted);
    cursor: not-allowed;
  }
</style>
