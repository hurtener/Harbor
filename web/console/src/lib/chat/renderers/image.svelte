<script lang="ts">
  // Image MIME renderer — Phase 73l canonical renderer-registry skeleton.
  // Renders an artifact image from its resolved (presigned, D-026) src.
  import type { RendererProps } from './index.js';

  let { mime, src, filename }: RendererProps = $props();
</script>

<figure class="image-renderer" data-renderer-source="image" data-mime={mime}>
  <img {src} alt={filename ?? 'artifact image'} />
  {#if filename}
    <figcaption>{filename}</figcaption>
  {/if}
</figure>

<style>
  .image-renderer {
    margin: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: center;
  }

  img {
    max-width: 100%;
    max-height: 60vh;
    border-radius: var(--radius-md);
    border: var(--border-thin) solid var(--color-border);
  }

  figcaption {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
</style>
