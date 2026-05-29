<script lang="ts">
  // Chat module — message bubble (Phase 73n / D-130, Phase 108 / D-167).
  //
  // Renders one `ChatMessage`: a role-styled bubble carrying the
  // message text (now rendered through MarkdownInline for agent
  // messages), any fenced code blocks, and the attached tool-call /
  // diff / artifact-reference cards.
  //
  // Phase 108 ships an in-house safe-subset markdown renderer
  // (MarkdownInline.svelte) for agent bubbles. User bubbles stay plain
  // text. Full CommonMark / GFM / KaTeX / Mermaid remains post-V1
  // pending an RFC-blessed dependency addition.
  import CodeBlock from './CodeBlock.svelte';
  import MarkdownInline from './MarkdownInline.svelte';
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

  // The message body, split into plain-text + fenced-code segments.
  const parts = $derived(splitChatSegments(message.text));

  // Deterministic avatar colour derived from role + agentID hash.
  const avatarInitial = $derived(
    message.role === 'user' ? 'U' : message.role === 'agent' ? 'A' : 'S'
  );

  const avatarBg = $derived(
    message.role === 'user'
      ? 'var(--color-accent-soft)'
      : message.role === 'agent'
        ? 'var(--color-success-soft)'
        : 'var(--color-surface-raised)'
  );

  const avatarFg = $derived(
    message.role === 'user'
      ? 'var(--color-accent)'
      : message.role === 'agent'
        ? 'var(--color-success)'
        : 'var(--color-text-muted)'
  );

  function formatTime(iso: string): string {
    try {
      const d = new Date(iso);
      return new Intl.DateTimeFormat(undefined, {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        timeZoneName: 'short'
      }).format(d);
    } catch {
      return iso;
    }
  }
</script>

<div
  class="message-bubble"
  data-testid="chat-message-bubble"
  data-role={message.role}
  data-message-id={message.id}
>
  <div class="bubble-row">
    <div
      class="avatar"
      style:background={avatarBg}
      style:color={avatarFg}
      aria-hidden="true"
    >
      {avatarInitial}
    </div>

    <div class="bubble-content">
      <div class="bubble-head">
        <span class="agent-name">
          {message.role === 'user'
            ? 'You'
            : message.role === 'agent'
              ? 'Assistant'
              : 'System'}
        </span>
        <span class="sep">·</span>
        <time class="bubble-time" datetime={message.at}>
          {formatTime(message.at)}
        </time>
        {#if message.role === 'agent'}
          <span class="sep">·</span>
          <span class="planner-phase">Idle</span>
        {/if}
      </div>

      <div class="bubble-body">
        {#if message.reasoningSteps}
          <ReasoningAccordion steps={message.reasoningSteps} />
        {/if}

        {#if message.role === 'agent'}
          <!-- Phase 108: agent messages render through MarkdownInline. -->
          <MarkdownInline source={message.text} />
        {:else}
          {#each parts as part, i (i)}
            {#if part.kind === 'text'}
              {#if part.value.trim() !== ''}
                <p class="bubble-text">{part.value}</p>
              {/if}
            {:else}
              <CodeBlock code={part.value} lang={part.lang} />
            {/if}
          {/each}
        {/if}

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

  .bubble-row {
    display: flex;
    gap: var(--space-2);
    align-items: flex-start;
  }

  .avatar {
    width: var(--size-avatar-md);
    height: var(--size-avatar-md);
    border-radius: 50%;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: var(--text-xs);
    font-weight: 600;
    flex-shrink: 0;
    text-transform: uppercase;
  }

  .bubble-content {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex: 1;
    min-width: 0;
  }

  .bubble-head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .agent-name {
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-text);
  }

  .sep {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .bubble-time {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    font-family: var(--font-mono);
  }

  .planner-phase {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    font-style: italic;
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
