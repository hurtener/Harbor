<script lang="ts">
  // JSON MIME renderer — Phase 73l canonical renderer registry. Fetches
  // the artifact JSON from its resolved (presigned, D-026) src and
  // renders it pretty-printed. A collapsible tree view is a post-V1
  // enhancement; V1 shows the indented JSON safely.
  import type { RendererProps } from './index.js';

  let { mime, src, filename }: RendererProps = $props();

  let pretty = $state('');
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
      const raw = await resp.text();
      try {
        pretty = JSON.stringify(JSON.parse(raw), null, 2);
      } catch {
        // Not valid JSON — show the raw text rather than failing the
        // whole preview (fail visible, not silent).
        pretty = raw;
      }
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

<div class="json-renderer" data-renderer-source="json" data-mime={mime}>
  {#if loading}
    <p class="status">Loading {filename ?? 'content'}…</p>
  {:else if error}
    <p class="status status-error">Could not load content: {error}</p>
  {:else}
    <pre><code>{pretty}</code></pre>
  {/if}
</div>

<style>
  .json-renderer {
    width: 100%;
  }

  pre {
    margin: var(--space-0);
    padding: var(--space-3);
    background: var(--color-bg);
    border: var(--border-hairline);
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
