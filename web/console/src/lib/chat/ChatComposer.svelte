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

  // 108a-D — drag & drop file upload onto the composer.
  let dragOver = $state(false);
  function onDragOver(e: DragEvent): void {
    e.preventDefault();
    dragOver = true;
  }
  function onDragLeave(): void {
    dragOver = false;
  }
  function onDrop(e: DragEvent): void {
    e.preventDefault();
    dragOver = false;
    void handleFiles(e.dataTransfer?.files ?? null);
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

<div
  class="chat-composer"
  class:drag-over={dragOver}
  data-testid="chat-composer"
  role="group"
  aria-label="Message composer"
  ondragover={onDragOver}
  ondragleave={onDragLeave}
  ondrop={onDrop}
>
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

  <!-- 108a-D — message textarea on top (mock Image 13), then the action
       row: attach + drop-zone, voice, and the Send cluster. -->
  <textarea
    class="composer-input"
    data-testid="chat-composer-input"
    placeholder="Type your message…  (Cmd/Ctrl-Enter to send)"
    bind:value={text}
    onkeydown={onKeydown}
    disabled={sending}
    rows="2"
  ></textarea>

  <div class="composer-actions">
    <label class="drop-zone" title="Attach a file — or drag & drop">
      <input
        type="file"
        multiple
        data-testid="chat-attach-input"
        onchange={(e) => void handleFiles((e.currentTarget as HTMLInputElement).files)}
        disabled={sending}
      />
      <span class="attach-icon" aria-hidden="true">📎</span>
      <span class="drop-zone-label">
        {dragOver ? 'Drop files to upload' : 'Drag & drop files here or click to upload'}
      </span>
    </label>

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

    <!-- 108a-D — Send is the accent arrow; while a run is active the
         split mode dropdown (Queue / Steer) replaces the radio fieldset. -->
    <div class="send-cluster">
      {#if running}
        <select
          class="mode-select"
          data-testid="chat-mode-picker"
          bind:value={mode}
          title="Send mode while a run is active"
        >
          <option value="queue">Queue</option>
          <option value="steer">Steer</option>
        </select>
      {/if}
      <button
        type="button"
        class="send-button"
        data-testid="chat-send-button"
        onclick={send}
        disabled={!canSend}
        title={sending
          ? 'Sending…'
          : running
            ? mode === 'steer'
              ? 'Steer the current run'
              : 'Queue after the current run'
            : 'Send'}
      >
        <span aria-hidden="true">{sending ? '…' : '→'}</span>
        <span class="sr-only">
          {sending ? 'Sending' : running ? (mode === 'steer' ? 'Steer' : 'Queue') : 'Send'}
        </span>
      </button>
    </div>
  </div>

  <div class="composer-foot">
    <span class="token-count" data-testid="chat-token-count">~{tokenEstimate} tokens</span>
    {#if uploading}
      <span class="uploading">Uploading…</span>
    {/if}
  </div>
</div>

<style>
  /* 108a-D — recessed composer container (mock Image 13): textarea on
     top, an action row (attach/drop-zone, voice, Send) below. */
  .chat-composer {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    border-top: var(--border-hairline);
    background: var(--color-bg);
  }

  .chat-composer.drag-over {
    background: var(--color-accent-soft);
  }

  .composer-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  /* The drop-zone is the wide click-to-upload + drag target. */
  .drop-zone {
    flex: 1;
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-style: dashed;
    border-radius: var(--radius-sm);
    cursor: pointer;
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .drop-zone:hover,
  .chat-composer.drag-over .drop-zone {
    border-color: var(--color-accent);
    color: var(--color-text);
  }

  .drop-zone input {
    display: none;
  }

  .attach-icon {
    font-size: var(--text-base);
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

  .composer-input {
    width: 100%;
    box-sizing: border-box;
    resize: vertical;
    padding: var(--space-3);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    font-family: var(--font-sans);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .composer-input:focus {
    outline: none;
    border-color: var(--color-accent);
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
    font-weight: 700;
    font-size: var(--text-lg);
    border-color: var(--color-accent);
    min-width: var(--space-10);
    padding: var(--space-2) var(--space-4);
  }

  .send-button:not(:disabled):hover {
    filter: brightness(1.1);
  }

  .send-cluster {
    display: flex;
    align-items: stretch;
    gap: var(--space-1);
  }

  .mode-select {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
  }

  .voice-button:disabled,
  .send-button:disabled,
  .drop-zone:has(input:disabled) {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .composer-foot {
    display: flex;
    gap: var(--space-3);
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
