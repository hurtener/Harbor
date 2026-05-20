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
  // Recovery-dropouts card body — the right-rail content rendered inside
  // a shared `RailCard` (D-121, CONVENTIONS.md §3). It surfaces
  // `memory.recovery_dropped` events (D-035). The page wraps this in
  // `<RailCard title="Recovery dropouts">`; this component owns only the
  // card body. Read-only. Svelte 5 runes mode (D-092).
  let { dropouts }: { dropouts: RecoveryDropout[] } = $props();
</script>

<div class="dropouts-body" aria-label="Recovery dropouts">
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
</div>

<style>
  .dropouts-body {
    display: grid;
    gap: var(--space-2);
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
    gap: var(--space-3);
    font-size: var(--text-sm);
  }

  .reason {
    color: var(--color-warning);
    overflow-wrap: anywhere;
  }

  time {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    flex-shrink: 0;
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
