<script lang="ts">
  // Chat-bubble artifact-reference renderer — Phase 73n / D-130.
  //
  // Registered into the Phase 73l canonical renderer registry under the
  // synthetic MIME `application/vnd.harbor.artifact-reference`. It
  // renders an artifact-reference card inside a chat bubble — the
  // by-reference UI for heavy content (D-026). The card NEVER inlines
  // the artifact bytes; it shows the metadata and a Download link to
  // the resolved presigned `src`.
  import type { RendererProps } from './index.js';

  let { mime, src, filename }: RendererProps = $props();
</script>

<div class="artifact-reference" data-renderer-source="artifact-reference" data-mime={mime}>
  <div class="artifact-icon" aria-hidden="true">⎙</div>
  <div class="artifact-meta">
    <span class="artifact-name">{filename ?? 'artifact'}</span>
    <span class="artifact-hint">Heavy content — referenced, not inlined (D-026)</span>
  </div>
  <a class="artifact-download" href={src} target="_blank" rel="noopener noreferrer">
    Download
  </a>
</div>

<style>
  .artifact-reference {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-3);
    background: var(--color-surface);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  .artifact-icon {
    font-size: var(--text-lg);
    color: var(--color-text-muted);
  }

  .artifact-meta {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex: 1;
    min-width: var(--space-0);
  }

  .artifact-name {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .artifact-hint {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .artifact-download {
    font-size: var(--text-sm);
    color: var(--color-accent);
    text-decoration: none;
    white-space: nowrap;
  }
</style>
