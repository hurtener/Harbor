<script lang="ts">
  // Chat module — chat panel (Phase 73n / D-130).
  //
  // The chat panel composes the message stream (`<MessageBubble>` per
  // `ChatMessage`) and the `<ChatComposer>`. It is the chat module's
  // top-level component; the Playground page mounts it and injects a
  // `ChatProtocolClient`.
  //
  // The panel is PRESENTATIONAL: it renders the `messages` prop and
  // forwards composer events. It owns no Protocol calls of its own —
  // the host (Playground) does the `sendMessage` round-trip and pushes
  // the resulting messages back in via the `messages` prop. This keeps
  // the chat module a pure, injectable consumer (no Console singleton,
  // no `connection.ts`, no `fetch`).
  import MessageBubble from './MessageBubble.svelte';
  import ChatComposer from './ChatComposer.svelte';
  import type { ChatMessage, ChatProtocolClient } from './types.js';

  let {
    messages,
    client,
    sending = false,
    onsend
  }: {
    messages: ChatMessage[];
    client: ChatProtocolClient;
    /** True while a send is in flight. */
    sending?: boolean;
    /** Forwarded from the composer — the host does the Protocol round-trip. */
    onsend: (text: string, artifactIDs: string[]) => void;
  } = $props();
</script>

<div class="chat-panel" data-testid="chat-panel">
  <div class="chat-stream" data-testid="chat-stream">
    {#if messages.length === 0}
      <p class="stream-empty" data-testid="chat-stream-empty">
        No messages yet — send one to start the conversation.
      </p>
    {:else}
      {#each messages as message (message.id)}
        <MessageBubble {message} {client} />
      {/each}
    {/if}
  </div>

  <ChatComposer {client} {sending} {onsend} />
</div>

<style>
  .chat-panel {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: var(--layout-detail-min-height);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    background: var(--color-bg);
    overflow: hidden;
  }

  .chat-stream {
    flex: 1;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4);
    overflow-y: auto;
  }

  .stream-empty {
    margin: auto;
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
