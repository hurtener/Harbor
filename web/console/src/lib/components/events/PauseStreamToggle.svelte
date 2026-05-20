<script lang="ts">
  // Events page — Pause-stream toggle (Phase 73g / D-125).
  //
  // A Console-LOCAL view toggle that freezes the event table's render
  // without closing the SSE subscription. It is a DISTINCT primitive
  // from the runtime `pause` Protocol method (which is task-scoped — RFC
  // §5.2): this toggle fires NO Protocol call. While paused, the
  // underlying `events.Cursor` keeps advancing per D-029 and incoming
  // events buffer; resuming flushes them in cursor order. The footer
  // chip flips to `Events Stream: PAUSED` (amber) — see ConnectionFooter
  // wiring on the page. Svelte 5 runes (D-092); tokens only.

  let {
    paused,
    ontoggle
  }: {
    /** True while the table render is frozen. */
    paused: boolean;
    /** Emitted on click — the page calls subscription.pause()/resume(). */
    ontoggle: () => void;
  } = $props();
</script>

<button
  type="button"
  class="pause-toggle"
  class:paused
  data-testid="pause-stream-toggle"
  aria-pressed={paused}
  onclick={ontoggle}
>
  {paused ? '▶ Resume stream' : '⏸ Pause stream'}
</button>

<style>
  .pause-toggle {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .pause-toggle.paused {
    border-color: var(--color-warning);
    color: var(--color-warning);
  }

  .pause-toggle:hover {
    border-color: var(--color-accent);
  }
</style>
