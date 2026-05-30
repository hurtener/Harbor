<script lang="ts">
  // Chat module — safe-subset markdown renderer (Phase 108 / D-167).
  //
  // Accepts a `source: string` prop and renders an in-house safe subset:
  // headings, lists, bold, italic, inline code, fenced code blocks, line
  // breaks. Rejects HTML — < is escaped verbatim.
  //
  // Fenced code blocks are re-routed to the existing `<CodeBlock>`
  // component. The parser lives in `markdown-parser.ts` so it can be
  // Vitest-pinned without rendering Svelte.
  import CodeBlock from './CodeBlock.svelte';
  import { parseMarkdown } from './markdown-parser.js';

  let { source }: { source: string } = $props();

  const tree = $derived(parseMarkdown(source));
</script>

{#each tree as node (node)}
  {#if node.type === 'heading'}
    {#if node.level === 1}
      <h1 class="md-h1">{node.text}</h1>
    {:else if node.level === 2}
      <h2 class="md-h2">{node.text}</h2>
    {:else}
      <h3 class="md-h3">{node.text}</h3>
    {/if}
  {:else if node.type === 'paragraph'}
    <p class="md-p">
      {#each node.children as child (child)}
        {#if child.type === 'text'}
          <span>{child.text}</span>
        {:else if child.type === 'bold'}
          <strong>{child.text}</strong>
        {:else if child.type === 'italic'}
          <em>{child.text}</em>
        {:else if child.type === 'code'}
          <code class="md-code">{child.text}</code>
        {/if}
      {/each}
    </p>
  {:else if node.type === 'list'}
    {#if node.ordered}
      <ol class="md-ol">
        {#each node.items as item, i (i)}
          <li>
            {#each item as child (child)}
              {#if child.type === 'text'}
                <span>{child.text}</span>
              {:else if child.type === 'bold'}
                <strong>{child.text}</strong>
              {:else if child.type === 'italic'}
                <em>{child.text}</em>
              {:else if child.type === 'code'}
                <code class="md-code">{child.text}</code>
              {/if}
            {/each}
          </li>
        {/each}
      </ol>
    {:else}
      <ul class="md-ul">
        {#each node.items as item, i (i)}
          <li>
            {#each item as child (child)}
              {#if child.type === 'text'}
                <span>{child.text}</span>
              {:else if child.type === 'bold'}
                <strong>{child.text}</strong>
              {:else if child.type === 'italic'}
                <em>{child.text}</em>
              {:else if child.type === 'code'}
                <code class="md-code">{child.text}</code>
              {/if}
            {/each}
          </li>
        {/each}
      </ul>
    {/if}
  {:else if node.type === 'code'}
    <CodeBlock code={node.text} lang={node.lang} />
  {:else if node.type === 'break'}
    <!-- paragraph break -->
  {/if}
{/each}

<style>
  .md-h1,
  .md-h2,
  .md-h3 {
    margin: var(--space-0);
    color: var(--color-text);
    font-weight: 600;
  }

  .md-h1 {
    font-size: var(--text-xl);
  }

  .md-h2 {
    font-size: var(--text-lg);
  }

  .md-h3 {
    font-size: var(--text-base);
  }

  .md-p {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
    line-height: 1.5;
  }

  .md-ul,
  .md-ol {
    margin: var(--space-0);
    padding-left: var(--space-5);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .md-ul li,
  .md-ol li {
    margin-bottom: var(--space-1);
  }

  .md-code {
    font-family: var(--font-mono);
    font-size: var(--size-code-font);
    background: var(--color-surface-raised);
    padding: var(--size-code-padding-y) var(--size-code-padding-x);
    border-radius: var(--radius-sm);
    color: var(--color-accent);
  }

  strong {
    font-weight: 600;
    color: var(--color-text);
  }

  em {
    font-style: italic;
    color: var(--color-text);
  }
</style>
