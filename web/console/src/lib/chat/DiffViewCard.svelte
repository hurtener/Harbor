<script lang="ts">
  // Chat module — diff-view card (Phase 73n / D-130).
  //
  // Renders a `ChatDiff` attached to an agent message as a
  // line-classified unified-diff card. The matching registry renderer
  // (`renderers/DiffView.svelte`, registered under
  // `application/vnd.harbor.diff`) is the preview-pane dispatch target;
  // both share the line-classification logic.
  import type { ChatDiff } from './types.js';

  let { diff }: { diff: ChatDiff } = $props();

  let lines = $derived(diff.patch.split('\n'));

  function lineKind(line: string): 'add' | 'remove' | 'hunk' | 'context' {
    if (line.startsWith('@@')) return 'hunk';
    if (line.startsWith('+')) return 'add';
    if (line.startsWith('-')) return 'remove';
    return 'context';
  }
</script>

<div class="diff-card" data-testid="diff-view-card">
  <span class="card-label">{diff.path}</span>
  <pre class="diff-body">{#each lines as line, i (i)}<span
        class="diff-line"
        data-kind={lineKind(line)}>{line}{'\n'}</span>{/each}</pre>
</div>

<style>
  .diff-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .card-label {
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--color-text-muted);
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
</style>
