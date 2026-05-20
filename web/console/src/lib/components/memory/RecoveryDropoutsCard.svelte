<script lang="ts" module>
  // One rendered recovery-dropout — the Console projection of a
  // `memory.recovery_dropped` event (D-035). The shipped runtime wire
  // string is `memory.recovery_dropped` (page-memory.md §12 names a
  // mockup-refinement `memory.overflow_drop_oldest` — that naming drift
  // is a docs(design) follow-up; the runtime + this card use the
  // shipped constant).
  export interface RecoveryDropout {
    reason: string;
    occurredAt: string;
  }
</script>

<script lang="ts">
  // Recovery-dropouts status card — the right-rail card that surfaces
  // `memory.recovery_dropped` events (D-035). Read-only. Svelte 5 runes.
  let { dropouts }: { dropouts: RecoveryDropout[] } = $props();
</script>

<section class="card" aria-label="Recovery dropouts">
  <h2>Recovery dropouts</h2>
  {#if dropouts.length === 0}
    <p class="muted">No recovery dropouts in this scope.</p>
  {:else}
    <ul class="dropouts">
      {#each dropouts as d, i (i)}
        <li>
          <span class="reason">{d.reason}</span>
          <time>{d.occurredAt}</time>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .card {
    background: var(--color-surface);
    border: var(--border-width-hairline) solid var(--color-border);
    border-radius: var(--radius-md);
    padding: var(--space-4);
  }

  h2 {
    font-size: var(--text-base);
    margin: var(--space-0) var(--space-0) var(--space-3);
  }

  .dropouts {
    list-style: none;
    padding: var(--space-0);
    margin: var(--space-0);
    display: grid;
    gap: var(--space-2);
  }

  .dropouts li {
    display: flex;
    justify-content: space-between;
    font-size: var(--text-sm);
  }

  .reason {
    font-family: var(--font-mono);
    color: var(--color-warning);
  }

  time {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
