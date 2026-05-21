<script lang="ts">
  // Fallback renderer — Phase 73l canonical renderer registry. Used when
  // no registered rule matches the artifact's MIME type (an unrenderable
  // content shape). Shows a "Preview unavailable" placeholder plus a
  // Download link. The Console never inlines the bytes; the Download link
  // points at the resolved (presigned, D-026) src.
  import type { RendererProps } from './index.js';

  let { mime, src, filename }: RendererProps = $props();
</script>

<div class="fallback-renderer" data-renderer-source="fallback" data-mime={mime}>
  <p class="headline">Preview unavailable</p>
  <p class="detail">
    No renderer is registered for content type
    <code>{mime || 'unknown'}</code>.
  </p>
  <a class="download" href={src} download={filename}>Download to view</a>
</div>

<style>
  .fallback-renderer {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .headline {
    margin: var(--space-0);
    font-size: var(--text-base);
    color: var(--color-text);
  }

  .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .download {
    color: var(--color-accent);
    font-size: var(--text-sm);
  }
</style>
