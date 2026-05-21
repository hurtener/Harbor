<script lang="ts">
  // Chat-bubble diff-view renderer — Phase 73n / D-130.
  //
  // Registered into the Phase 73l canonical renderer registry under the
  // synthetic MIME `application/vnd.harbor.diff`. It renders a
  // unified-diff card inside an agent chat bubble, line-classifying the
  // patch (added / removed / context / hunk header).
  //
  // It fetches the patch text from its resolved (presigned, D-026)
  // `src` — the same pattern the Phase 73l MIME renderers use.
  import type { RendererProps } from './index.js';

  let { mime, src, filename }: RendererProps = $props();

  let lines = $state<string[]>([]);
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
      const text = await resp.text();
      lines = text.split('\n');
    } catch (e) {
      error = e instanceof Error ? e.message : 'failed to load diff';
    } finally {
      loading = false;
    }
  }

  function lineKind(line: string): 'add' | 'remove' | 'hunk' | 'context' {
    if (line.startsWith('@@')) return 'hunk';
    if (line.startsWith('+')) return 'add';
    if (line.startsWith('-')) return 'remove';
    return 'context';
  }

  $effect(() => {
    void load(src);
  });
</script>

<div class="diff-view" data-renderer-source="diff" data-mime={mime}>
  {#if loading}
    <p class="status">Loading {filename ?? 'diff'}…</p>
  {:else if error}
    <p class="status status-error">Could not load diff: {error}</p>
  {:else}
    <pre class="diff-body">{#each lines as line, i (i)}<span
          class="diff-line"
          data-kind={lineKind(line)}>{line}{'\n'}</span>{/each}</pre>
  {/if}
</div>

<style>
  .diff-view {
    width: 100%;
  }

  .diff-body {
    margin: var(--space-0);
    padding: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    overflow: auto;
    max-height: 60vh;
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .diff-line {
    display: block;
    color: var(--color-text);
  }

  .diff-line[data-kind='add'] {
    background: var(--color-success-soft);
    color: var(--color-success);
  }

  .diff-line[data-kind='remove'] {
    background: var(--color-danger-soft);
    color: var(--color-danger);
  }

  .diff-line[data-kind='hunk'] {
    color: var(--color-accent);
  }

  .status {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .status-error {
    color: var(--color-danger);
  }
</style>
