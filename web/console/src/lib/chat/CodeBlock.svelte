<script lang="ts">
  // Chat module — code block (Phase 73n / D-130).
  //
  // Renders a fenced code block inside a chat bubble. V1 renders the
  // source in a safe monospace block — the same posture the Phase 73l
  // `code.svelte` MIME renderer takes. Syntax highlighting is a post-V1
  // enhancement: it needs a vetted highlighter dependency (Shiki), and
  // adding a heavy frontend dependency is an RFC change (CLAUDE.md §13).
  // The chat module ships the safe-text V1 form; the renderer registry
  // seam means a highlighter slots in later without a chat-module
  // reshape.
  let {
    code,
    lang = ''
  }: {
    code: string;
    lang?: string;
  } = $props();
</script>

<div class="code-block" data-testid="chat-code-block" data-lang={lang}>
  {#if lang !== ''}
    <span class="code-lang">{lang}</span>
  {/if}
  <pre class="code-body">{code}</pre>
</div>

<style>
  .code-block {
    position: relative;
    width: 100%;
  }

  .code-lang {
    position: absolute;
    top: var(--space-1);
    right: var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    font-family: var(--font-mono);
    text-transform: uppercase;
  }

  .code-body {
    margin: var(--space-0);
    padding: var(--space-3);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    overflow: auto;
    max-height: 60vh;
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--color-text);
    white-space: pre-wrap;
  }
</style>
