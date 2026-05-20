<script lang="ts">
  // Markdown MIME renderer — Phase 73l canonical renderer registry.
  // Fetches the artifact markdown from its resolved (presigned, D-026)
  // src. V1 renders the raw markdown source in a readable block; a
  // sanitised HTML render is a post-V1 enhancement (it needs a vetted
  // sanitiser dependency — out of the Phase 73l scope).
  import type { RendererProps } from './index.js';

  let { mime, src, filename }: RendererProps = $props();

  let text = $state('');
  let error = $state('');
  let loading = $state(true);

  async function load(url: string): Promise<void> {
    loading = true;
    error = '';
    try {
      const resp = await fetch(url);
      if (!resp.ok) {
        throw new Error(`HTTP ${resp.status}`);
      }
      text = await resp.text();
    } catch (e) {
      error = e instanceof Error ? e.message : 'failed to load content';
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void load(src);
  });
</script>

<div class="markdown-renderer" data-renderer-source="markdown" data-mime={mime}>
  {#if loading}
    <p class="status">Loading {filename ?? 'content'}…</p>
  {:else if error}
    <p class="status status-error">Could not load content: {error}</p>
  {:else}
    <pre class="markdown-source">{text}</pre>
  {/if}
</div>

<style>
  .markdown-renderer {
    width: 100%;
  }

  .markdown-source {
    margin: var(--space-0);
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-md);
    overflow: auto;
    max-height: 60vh;
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--color-text);
    white-space: pre-wrap;
  }

  .status {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .status-error {
    color: var(--color-danger);
  }
</style>
