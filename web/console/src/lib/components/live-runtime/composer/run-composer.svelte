<script lang="ts">
  // Harbor Console — Live Runtime run composer (Phase 73b / D-126).
  //
  // The bottom-dock right pane when NO topology node is selected (Brief
  // 11 §LR): the operator's present-tense control surface — Start /
  // Redirect / Inject context / User message / Cancel / Pause / Resume.
  //
  // # NOT the chat module (D-091 + CLAUDE.md §4.5 #11)
  //
  // This composer is built with NON-CHAT Skeleton primitives — a plain
  // textarea + buttons. The canonical chat module's V1 first consumer is
  // 73n Playground; a second in-V1 consumer would force extraction to
  // `web/shared/chat/`, which is out of V1 scope (the "encapsulate
  // first, extract on second consumer" rule). This file proves no import
  // from `$lib/chat/` — it composes the Runtime's shipped Phase 54
  // control verbs through the typed Protocol client directly.
  //
  // # No stubbed action (CONVENTIONS.md §5)
  //
  // Every verb invokes the REAL shipped Phase 54 control method or — for
  // the elevated control tier — renders disabled-with-tooltip when the
  // connection lacks the admin scope claim (D-079). The composer never
  // fakes success with a feedback string.
  //
  // Svelte 5 runes mode (D-092); design tokens only.

  /** The verbs the composer dispatches — the shipped Phase 54 surface. */
  export type ComposerVerb =
    | 'start'
    | 'redirect'
    | 'inject_context'
    | 'user_message'
    | 'cancel'
    | 'pause'
    | 'resume';

  let {
    canControl,
    pending,
    result,
    disconnected = false,
    onverb
  }: {
    /** Whether the connection carries the control scope claim (D-079). */
    canControl: boolean;
    /** Whether a verb dispatch is in flight. */
    pending: boolean;
    /** The last dispatch result — inline pass/fail (never silent). */
    result: { ok: boolean; message: string } | null;
    /** True when the Console has no Runtime attached (Phase 83r / W2). When
        true, the textarea + every verb render disabled-with-tooltip — no
        composer affordance dispatches against a phantom runtime. */
    disconnected?: boolean;
    /** Emitted with (verb, text) — the page dispatches the real method. */
    onverb: (verb: ComposerVerb, text: string) => void;
  } = $props();

  let text = $state('');

  // The text-bearing verbs (start / redirect / inject / user message)
  // need the textarea content; the lifecycle verbs (cancel / pause /
  // resume) do not. `start` and `user_message` are owner-tier;
  // redirect / inject / cancel / pause / resume are the elevated
  // control tier gated on the admin claim (D-066 / D-079).
  const elevated: ReadonlySet<ComposerVerb> = new Set<ComposerVerb>([
    'redirect',
    'inject_context',
    'cancel',
    'pause',
    'resume'
  ]);

  function dispatch(verb: ComposerVerb): void {
    onverb(verb, text);
    if (verb === 'start' || verb === 'user_message' || verb === 'redirect' || verb === 'inject_context') {
      text = '';
    }
  }

  // The Phase 83r W2 tooltip — the verbatim string is the shared
  // `DISCONNECTED_TOOLTIP` from connection.ts; duplicated here only
  // because importing a TS constant from a .svelte component for a
  // single string would force a runtime read for no benefit. Tests
  // that hover-assert the tooltip use this literal.
  const DISCONNECTED_TIP = 'Attach a Runtime to enable';

  function tipFor(verb: ComposerVerb): string | undefined {
    if (disconnected) {
      return DISCONNECTED_TIP;
    }
    if (canControl || !elevated.has(verb)) {
      return undefined;
    }
    return 'Requires the control scope claim — steering is an elevated tier (D-079).';
  }
</script>

<section class="composer" data-testid="run-composer">
  <h3 class="composer-title">Compose</h3>

  <textarea
    class="composer-input"
    rows="3"
    placeholder={disconnected
      ? 'Attach a Runtime in Settings to start composing…'
      : 'Type a message, redirect, or context to inject…'}
    bind:value={text}
    data-testid="composer-textarea"
    disabled={disconnected}
    title={disconnected ? DISCONNECTED_TIP : undefined}
  ></textarea>

  <div class="verb-row">
    <button
      type="button"
      class="control primary"
      data-testid="composer-start"
      disabled={pending || disconnected}
      title={tipFor('start')}
      onclick={() => dispatch('start')}
    >
      Start
    </button>
    <button
      type="button"
      class="control"
      data-testid="composer-user-message"
      disabled={pending || disconnected}
      title={tipFor('user_message')}
      onclick={() => dispatch('user_message')}
    >
      User message
    </button>
    <button
      type="button"
      class="control"
      data-testid="composer-redirect"
      disabled={pending || disconnected || !canControl}
      title={tipFor('redirect')}
      onclick={() => dispatch('redirect')}
    >
      Redirect
    </button>
    <button
      type="button"
      class="control"
      data-testid="composer-inject"
      disabled={pending || disconnected || !canControl}
      title={tipFor('inject_context')}
      onclick={() => dispatch('inject_context')}
    >
      Inject context
    </button>
  </div>

  <div class="verb-row">
    <button
      type="button"
      class="control"
      data-testid="composer-pause"
      disabled={pending || disconnected || !canControl}
      title={tipFor('pause')}
      onclick={() => dispatch('pause')}
    >
      Pause
    </button>
    <button
      type="button"
      class="control"
      data-testid="composer-resume"
      disabled={pending || disconnected || !canControl}
      title={tipFor('resume')}
      onclick={() => dispatch('resume')}
    >
      Resume
    </button>
    <button
      type="button"
      class="control danger"
      data-testid="composer-cancel"
      disabled={pending || disconnected || !canControl}
      title={tipFor('cancel')}
      onclick={() => dispatch('cancel')}
    >
      Cancel
    </button>
  </div>

  {#if result !== null}
    <p
      class="composer-result"
      class:ok={result.ok}
      class:err={!result.ok}
      data-testid="composer-result"
    >
      {result.message}
    </p>
  {/if}
</section>

<style>
  .composer {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
  }

  .composer-title {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .composer-input {
    width: 100%;
    box-sizing: border-box;
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2);
    font-size: var(--text-sm);
    resize: vertical;
  }

  .verb-row {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .control {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .control:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .control.primary {
    border-color: var(--color-accent);
    color: var(--color-accent);
    font-weight: 600;
  }

  .control.danger {
    border-color: var(--color-danger);
    color: var(--color-danger);
  }

  .composer-result {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .composer-result.ok {
    color: var(--color-success);
  }

  .composer-result.err {
    color: var(--color-danger);
  }
</style>
