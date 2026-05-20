<script lang="ts">
  // Memory-health status card — the right-rail card that renders the
  // aggregate counters from `memory.health` (Phase 73j / D-118).
  // Read-only (page-memory.md §12). Svelte 5 runes mode (D-092).
  import type { MemoryHealthAggregate } from '$lib/protocol-memory';

  let { aggregate }: { aggregate: MemoryHealthAggregate | null } = $props();

  const driverRows = $derived(
    aggregate ? Object.entries(aggregate.driver_by_scope) : []
  );
</script>

<section class="card" aria-label="Memory health">
  <h2>Memory health</h2>
  {#if aggregate}
    <dl class="counters">
      <div><dt>Total records</dt><dd>{aggregate.total}</dd></div>
      <div><dt>Expiring in 1h</dt><dd>{aggregate.expiring_in_1h}</dd></div>
      <div>
        <dt>Identity-rejected (24h)</dt>
        <dd class:warn={aggregate.identity_rejected_24h > 0}>
          {aggregate.identity_rejected_24h}
        </dd>
      </div>
      <div>
        <dt>Recovery-dropped (24h)</dt>
        <dd class:warn={aggregate.recovery_dropped_24h > 0}>
          {aggregate.recovery_dropped_24h}
        </dd>
      </div>
    </dl>
    {#if driverRows.length > 0}
      <h3>Driver by scope</h3>
      <ul class="drivers">
        {#each driverRows as [scope, driver] (scope)}
          <li><span class="scope">{scope}</span><span class="driver">{driver}</span></li>
        {/each}
      </ul>
    {/if}
  {:else}
    <p class="muted">Health counters unavailable.</p>
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

  h3 {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    margin: var(--space-4) var(--space-0) var(--space-2);
  }

  .counters {
    display: grid;
    gap: var(--space-2);
    margin: var(--space-0);
  }

  .counters div {
    display: flex;
    justify-content: space-between;
    font-size: var(--text-sm);
  }

  dt {
    color: var(--color-text-muted);
  }

  dd {
    margin: var(--space-0);
    font-variant-numeric: tabular-nums;
  }

  dd.warn {
    color: var(--color-warning);
  }

  .drivers {
    list-style: none;
    padding: var(--space-0);
    margin: var(--space-0);
    display: grid;
    gap: var(--space-1);
  }

  .drivers li {
    display: flex;
    justify-content: space-between;
    font-size: var(--text-xs);
  }

  .scope {
    color: var(--color-text-muted);
  }

  .driver {
    font-family: var(--font-mono);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
