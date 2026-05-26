<script lang="ts">
  // Chat module — message bubble (Phase 73n / D-130).
  //
  // Renders one `ChatMessage`: a role-styled bubble carrying the
  // message text, any fenced code blocks, and the attached tool-call /
  // diff / artifact-reference cards. While `streaming` is true the
  // bubble shows the streaming indicator.
  //
  // The text body is split into plain-text + fenced-code segments. V1
  // renders plain text verbatim and fenced code via `<CodeBlock>`; a
  // full markdown/KaTeX/Mermaid render is post-V1 (it needs vetted
  // renderer dependencies — an RFC change per CLAUDE.md §13). The
  // renderer-registry seam means those slot in later without a bubble
  // reshape.
  import CodeBlock from './CodeBlock.svelte';
  import ToolCallTraceCard from './ToolCallTraceCard.svelte';
  import DiffViewCard from './DiffViewCard.svelte';
  import ArtifactReferenceCard from './ArtifactReferenceCard.svelte';
  import ReasoningAccordion from './ReasoningAccordion.svelte';
  import StreamingIndicator from './StreamingIndicator.svelte';
  import { splitChatSegments } from './segments.js';
  import type { ChatMessage, ChatProtocolClient } from './types.js';

  let {
    message,
    client
  }: {
    message: ChatMessage;
    client: ChatProtocolClient;
  } = $props();

  // The message body, split into plain-text + fenced-code segments via
  // the pure `splitChatSegments` helper (Vitest-pinned).
  const parts = $derived(splitChatSegments(message.text));
</script>

<div
  class="message-bubble"
  data-testid="chat-message-bubble"
  data-role={message.role}
  data-message-id={message.id}
>
  <div class="bubble-head">
    <span class="role-tag">{message.role}</span>
    <time class="bubble-time" datetime={message.at}>{message.at}</time>
  </div>

  <div class="bubble-body">
    {#if message.reasoningSteps}
      <ReasoningAccordion steps={message.reasoningSteps} />
    {/if}

    {#each parts as part, i (i)}
      {#if part.kind === 'text'}
        {#if part.value.trim() !== ''}
          <p class="bubble-text">{part.value}</p>
        {/if}
      {:else}
        <CodeBlock code={part.value} lang={part.lang} />
      {/if}
    {/each}

    {#if message.toolCalls && message.toolCalls.length > 0}
      <ToolCallTraceCard toolCalls={message.toolCalls} />
    {/if}

    {#if message.diffs}
      {#each message.diffs as diff, i (i)}
        <DiffViewCard {diff} />
      {/each}
    {/if}

    {#if message.artifacts}
      {#each message.artifacts as artifact (artifact.id)}
        <ArtifactReferenceCard {artifact} {client} preview />
      {/each}
    {/if}

    {#if message.streaming}
      <StreamingIndicator />
    {/if}
  </div>
</div>

<style>
  .message-bubble {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-3);
    border-radius: var(--radius-md);
    border: var(--border-hairline);
    max-width: 80%;
  }

  .message-bubble[data-role='user'] {
    align-self: flex-end;
    background: var(--color-accent-soft);
  }

  .message-bubble[data-role='agent'] {
    align-self: flex-start;
    background: var(--color-surface-raised);
  }

  .message-bubble[data-role='system'] {
    align-self: center;
    background: var(--color-surface);
    max-width: 90%;
  }

  .bubble-head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .role-tag {
    font-size: var(--text-xs);
    text-transform: uppercase;
    font-weight: 600;
    color: var(--color-text-muted);
  }

  .bubble-time {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    font-family: var(--font-mono);
  }

  .bubble-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .bubble-text {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
    white-space: pre-wrap;
    word-break: break-word;
  }
</style>
