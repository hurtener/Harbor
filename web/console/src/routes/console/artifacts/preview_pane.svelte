<script lang="ts">
  // Artifacts preview pane — Phase 73l (D-120).
  //
  // This component dispatches the selected artifact's MIME type through
  // the CANONICAL renderer registry at `$lib/chat/renderers` — it does
  // NOT carry a bespoke per-mime renderer (CLAUDE.md §13; Brief 12).
  // The Playwright spec asserts no `*_renderer.svelte` lives under this
  // route directory.
  //
  // The preview `src` is a presigned URL resolved via `artifacts.get_ref`
  // (D-022 / D-026) — the Console never inlines artifact bytes. When the
  // configured artifact-store driver has no `Presigner` (the dev `inmem`
  // / `fs` drivers return CodePresignUnsupported), the pane shows the
  // typed-error placeholder + a Download fallback.
  import { dispatchRenderer } from '$lib/chat/renderers';
  import type { ArtifactRow } from '$lib/protocol';

  interface PreviewState {
    /** The resolved presigned URL, or '' when none. */
    src: string;
    /** A typed error code from artifacts.get_ref, or '' when ok. */
    errorCode: string;
    /** Whether the resolver call is in flight. */
    loading: boolean;
  }

  let {
    row,
    preview
  }: {
    row: ArtifactRow | null;
    preview: PreviewState;
  } = $props();

  // The dispatched renderer descriptor for the selected row's MIME.
  const descriptor = $derived(
    row ? dispatchRenderer(row.ref.mime_type ?? '') : null
  );
</script>

<section class="preview-pane" data-testid="artifact-preview">
  {#if !row}
    <p class="empty">Select an artifact to preview it.</p>
  {:else if preview.loading}
    <p class="status">Resolving preview link…</p>
  {:else if preview.errorCode === 'presign_unsupported'}
    <div class="unsupported" data-renderer-source="presign-unsupported">
      <p class="headline">Preview not available</p>
      <p class="detail">
        The configured artifact-store driver does not support presigned
        URLs. Use the Download action to fetch the artifact.
      </p>
    </div>
  {:else if preview.errorCode}
    <p class="status status-error">
      Could not resolve preview: <code>{preview.errorCode}</code>
    </p>
  {:else if descriptor}
    <!-- The canonical renderer registry handles the preview. The host
         re-exposes the renderer `source` so the Playwright spec can
         assert which registry renderer was dispatched. -->
    <div data-renderer-dispatched={descriptor.source}>
      <descriptor.component
        mime={row.ref.mime_type ?? ''}
        src={preview.src}
        filename={row.ref.filename}
      />
    </div>
  {/if}
</section>

<style>
  .preview-pane {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .empty,
  .status {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .status-error {
    color: var(--color-danger);
  }

  .unsupported {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-thin) solid var(--color-border);
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
</style>
