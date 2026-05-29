<script lang="ts">
  // Chat module — composer (Phase 73n / D-130).
  //
  // The bottom-of-stream input: a multimodal attach button, a textarea,
  // a voice-input button, a Send button (Cmd/Ctrl-Enter shortcut), and
  // a token-count preview. Attachments upload through the injected
  // `ChatProtocolClient.uploadArtifact` (→ `artifacts.put`); the
  // resulting `ChatArtifactRef`s ride the next message by reference
  // (D-026 — never inline bytes).
  //
  // Voice input uses the browser SpeechRecognition API when available;
  // when it is not, the button renders disabled-with-tooltip (no
  // stubbed action presented as done — CONVENTIONS.md §5 / CLAUDE.md
  // §13).
  import type { ChatArtifactRef, ChatProtocolClient } from './types.js';

  let {
    client,
    sending = false,
    running = false,
    onsend
  }: {
    client: ChatProtocolClient;
    /** True while a send round-trip is in flight — disables the composer. */
    sending?: boolean;
    /**
     * Round-6 F10 — true when the parent session has a non-terminal task
     * the operator has not yet acknowledged. The composer stays usable;
     * pressing Send chooses between "queue this message until the
     * current run finishes" (default) and "steer the current run with
     * this message as a `user_message` inject." When `running` is
     * `false`, the mode picker is hidden and Send always starts a
     * fresh task.
     */
    running?: boolean;
    /**
     * Invoked with the composed text + attached artifact ids. `mode`
     * is set when `running` is true: `'queue'` is the default; `'steer'`
     * is the explicit "inject into the current run" path. When `running`
     * is false, `mode` is undefined and the caller routes to `start`.
     */
    onsend: (text: string, artifactIDs: string[], mode?: 'queue' | 'steer') => void;
  } = $props();

  // The send-mode picker is hidden when no run is in flight. When a
  // run is active, the operator can choose between queueing the next
  // message (sent automatically after the current task completes) and
  // steering the current run via the SHIPPED `user_message` control
  // verb.
  let mode = $state<'queue' | 'steer'>('queue');

  let text = $state('');
  let attachments = $state<ChatArtifactRef[]>([]);
  let uploading = $state(false);
  let uploadError = $state('');
  let listening = $state(false);

  // A rough token-count preview — ~4 chars per token is the common
  // heuristic. It is a PREVIEW (the runtime is authoritative); the
  // estimate keeps the operator oriented without a round-trip.
  const tokenEstimate = $derived(Math.ceil(text.length / 4));

  // SpeechRecognition is vendor-prefixed in some browsers; resolve it
  // once. When absent, the voice button is disabled-with-tooltip.
  const speechSupported =
    typeof window !== 'undefined' &&
    ((window as unknown as { SpeechRecognition?: unknown }).SpeechRecognition !== undefined ||
      (window as unknown as { webkitSpeechRecognition?: unknown }).webkitSpeechRecognition !==
        undefined);

  const canSend = $derived(
    !sending && !uploading && (text.trim() !== '' || attachments.length > 0)
  );

  async function handleFiles(files: FileList | null): Promise<void> {
    if (files === null || files.length === 0) {
      return;
    }
    uploading = true;
    uploadError = '';
    try {
      for (const file of Array.from(files)) {
        const ref = await client.uploadArtifact(file);
        attachments = [...attachments, ref];
      }
    } catch (e) {
      uploadError = e instanceof Error ? e.message : 'upload failed';
    } finally {
      uploading = false;
    }
  }

  function removeAttachment(id: string): void {
    attachments = attachments.filter((a) => a.id !== id);
  }

  function send(): void {
    if (!canSend) {
      return;
    }
    onsend(
      text.trim(),
      attachments.map((a) => a.id),
      running ? mode : undefined
    );
    text = '';
    attachments = [];
  }

  function onKeydown(e: KeyboardEvent): void {
    // Cmd-Enter (macOS) / Ctrl-Enter (other) sends.
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
      e.preventDefault();
      send();
    }
  }

  function toggleVoice(): void {
    if (!speechSupported) {
      return;
    }
    const Ctor =
      (window as unknown as { SpeechRecognition?: new () => SpeechRecognitionLike })
        .SpeechRecognition ??
      (window as unknown as { webkitSpeechRecognition?: new () => SpeechRecognitionLike })
        .webkitSpeechRecognition;
    if (Ctor === undefined) {
      return;
    }
    if (listening) {
      listening = false;
      return;
    }
    const recog = new Ctor();
    recog.onresult = (ev: SpeechRecognitionEventLike) => {
      const transcript = ev.results?.[0]?.[0]?.transcript ?? '';
      text = text === '' ? transcript : `${text} ${transcript}`;
    };
    recog.onend = () => {
      listening = false;
    };
    listening = true;
    recog.start();
  }

  // Minimal structural types for the SpeechRecognition API — kept local
  // so the chat module pulls in no ambient lib dependency.
  interface SpeechRecognitionEventLike {
    results?: Array<Array<{ transcript: string }>>;
  }
  interface SpeechRecognitionLike {
    onresult: (ev: SpeechRecognitionEventLike) => void;
    onend: () => void;
    start: () => void;
  }
</script>

<div class="chat-composer" data-testid="chat-composer">
  {#if attachments.length > 0}
    <ul class="attachment-list" data-testid="chat-attachments">
      {#each attachments as a (a.id)}
        <li class="attachment">
          <span class="attachment-name">{a.filename}</span>
          <span class="attachment-mime">{a.mime}</span>
          <button
            type="button"
            class="attachment-remove"
            data-testid="chat-attachment-remove"
            onclick={() => removeAttachment(a.id)}
            aria-label="Remove attachment"
          >
            ×
          </button>
        </li>
      {/each}
    </ul>
  {/if}

  {#if uploadError !== ''}
    <p class="composer-error" role="alert">Attachment failed: {uploadError}</p>
  {/if}

  <div class="composer-row">
    <label class="attach-button" title="Attach a file">
      <input
        type="file"
        multiple
        data-testid="chat-attach-input"
        onchange={(e) => void handleFiles((e.currentTarget as HTMLInputElement).files)}
        disabled={sending}
      />
      <span aria-hidden="true">＋</span>
      <span class="sr-only">Attach a file</span>
    </label>

    <textarea
      class="composer-input"
      data-testid="chat-composer-input"
      placeholder="Send a message — Cmd/Ctrl-Enter to send"
      bind:value={text}
      onkeydown={onKeydown}
      disabled={sending}
      rows="2"
    ></textarea>

    <button
      type="button"
      class="voice-button"
      data-testid="chat-voice-button"
      data-listening={listening}
      onclick={toggleVoice}
      disabled={!speechSupported || sending}
      title={speechSupported
        ? 'Voice input'
        : 'Voice input unavailable — this browser has no SpeechRecognition API'}
    >
      <span aria-hidden="true">🎤</span>
      <span class="sr-only">Voice input</span>
    </button>

    <button
      type="button"
      class="send-button"
      data-testid="chat-send-button"
      onclick={send}
      disabled={!canSend}
    >
      {sending ? 'Sending…' : running ? (mode === 'steer' ? 'Steer' : 'Queue') : 'Send'}
    </button>
  </div>

  {#if running}
    <fieldset class="mode-picker" data-testid="chat-mode-picker">
      <legend class="sr-only">Send mode while a run is active</legend>
      <label class="mode-option">
        <input
          type="radio"
          name="chat-send-mode"
          value="queue"
          checked={mode === 'queue'}
          onchange={() => (mode = 'queue')}
          data-testid="chat-mode-queue"
        />
        <span class="mode-label">Queue after current run</span>
      </label>
      <label class="mode-option">
        <input
          type="radio"
          name="chat-send-mode"
          value="steer"
          checked={mode === 'steer'}
          onchange={() => (mode = 'steer')}
          data-testid="chat-mode-steer"
        />
        <span class="mode-label">Steer current run</span>
      </label>
    </fieldset>
  {/if}

  <div class="composer-foot">
    <span class="token-count" data-testid="chat-token-count">~{tokenEstimate} tokens</span>
    {#if uploading}
      <span class="uploading">Uploading…</span>
    {/if}
  </div>
</div>

<style>
  .chat-composer {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    border-top: var(--border-hairline);
    background: var(--color-bg);
  }

  .composer-row {
    display: flex;
    align-items: flex-end;
    gap: var(--space-2);
    padding: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .attach-button {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    padding: var(--space-2);
    background: transparent;
    border: none;
    border-right: var(--border-hairline);
    cursor: pointer;
    font-size: var(--text-base);
    color: var(--color-text);
    padding-right: var(--space-3);
    margin-right: var(--space-1);
  }

  .attachment-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .attachment {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-1) var(--space-2);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .attachment-name {
    font-size: var(--text-xs);
    color: var(--color-text);
  }

  .attachment-mime {
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--color-text-muted);
  }

  .attachment-remove {
    background: none;
    border: none;
    color: var(--color-text-muted);
    cursor: pointer;
    font-size: var(--text-sm);
  }

  .composer-error {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-danger);
  }

  .attach-button input {
    display: none;
  }

  .composer-input {
    flex: 1;
    resize: vertical;
    padding: var(--space-2);
    background: var(--color-bg);
    border: none;
    font-family: var(--font-sans);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .voice-button,
  .send-button {
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    border: var(--border-hairline);
    cursor: pointer;
    font-size: var(--text-sm);
  }

  .voice-button {
    background: var(--color-surface-raised);
    color: var(--color-text);
  }

  .voice-button[data-listening='true'] {
    background: var(--color-danger-soft);
    color: var(--color-danger);
  }

  .send-button {
    background: var(--color-accent);
    color: var(--color-bg);
    font-weight: 600;
    border-color: var(--color-accent);
  }

  .voice-button:disabled,
  .send-button:disabled,
  .attach-button:has(input:disabled) {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .composer-foot {
    display: flex;
    gap: var(--space-3);
  }

  .mode-picker {
    display: flex;
    gap: var(--space-3);
    border: none;
    margin: var(--space-0);
    padding: var(--space-0);
  }

  .mode-option {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    cursor: pointer;
  }

  .mode-label {
    font-family: var(--font-sans);
  }

  .token-count,
  .uploading {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .sr-only {
    position: absolute;
    width: var(--size-sr-square);
    height: var(--size-sr-square);
    margin: calc(-1 * var(--size-sr-square));
    overflow: hidden;
    clip: rect(0, 0, 0, 0);
    white-space: nowrap;
    border: 0;
  }
</style>
