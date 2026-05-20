<script lang="ts">
  // Code / plain-text MIME renderer — Phase 73l canonical renderer
  // registry. Fetches the artifact text from its resolved (presigned,
  // D-026) src and renders it in a monospace block. Highlighting is a
  // post-V1 enhancement; the V1 renderer shows the raw text safely.
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

<div class="code-renderer" data-renderer-source="code" data-mime={mime}>
  {#if loading}
    <p class="status">Loading {filename ?? 'content'}…</p>
  {:else if error}
    <p class="status status-error">Could not load content: {error}</p>
  {:else}
    <pre><code>{text}</code></pre>
  {/if}
</div>

<style>
  .code-renderer {
    width: 100%;
  }

  pre {
    margin: var(--space-0);
    padding: var(--space-3);
    background: var(--color-bg);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-md);
    overflow: auto;
    max-height: 60vh;
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--color-text);
    white-space: pre;
  }

  .status {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .status-error {
    color: var(--color-danger);
  }
</style>
