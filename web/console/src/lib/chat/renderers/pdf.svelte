<script lang="ts">
  // PDF MIME renderer — Phase 73l canonical renderer registry. Embeds the
  // PDF from its resolved (presigned, D-026) src via the browser's native
  // PDF viewer. The Console never inlines the PDF bytes; the src is a
  // presigned URL the browser fetches directly.
  import type { RendererProps } from './index.js';

  let { mime, src, filename }: RendererProps = $props();
</script>

<div class="pdf-renderer" data-renderer-source="pdf" data-mime={mime}>
  <object data={src} type="application/pdf" title={filename ?? 'artifact PDF'}>
    <p class="fallback">
      Inline PDF preview is unavailable in this browser.
      <a href={src} download={filename}>Download the PDF</a> to view it.
    </p>
  </object>
</div>

<style>
  .pdf-renderer {
    width: 100%;
  }

  object {
    width: 100%;
    height: 60vh;
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  .fallback {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  a {
    color: var(--color-accent);
  }
</style>
