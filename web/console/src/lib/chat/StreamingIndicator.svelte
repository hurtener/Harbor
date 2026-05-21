<script lang="ts">
  // Chat module — streaming indicator (Phase 73n / D-130).
  //
  // A small animated "agent is producing tokens" cue rendered at the
  // tail of an agent bubble while `ChatMessage.streaming` is true.
  // Design tokens only.
  let { label = 'streaming' }: { label?: string } = $props();
</script>

<span class="streaming-indicator" data-testid="streaming-indicator" aria-live="polite">
  <span class="dot"></span>
  <span class="dot"></span>
  <span class="dot"></span>
  <span class="sr-only">{label}</span>
</span>

<style>
  .streaming-indicator {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
  }

  .dot {
    width: var(--size-px);
    height: var(--size-px);
    border-radius: var(--radius-sm);
    background: var(--color-accent);
    animation: blink var(--motion-slow) ease-in-out infinite alternate;
  }

  .dot:nth-child(2) {
    animation-delay: var(--motion-fast);
  }

  .dot:nth-child(3) {
    animation-delay: var(--motion-base);
  }

  @keyframes blink {
    from {
      opacity: 0.3;
    }
    to {
      opacity: 1;
    }
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
