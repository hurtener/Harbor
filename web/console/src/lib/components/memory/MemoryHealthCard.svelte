<script lang="ts">
  // Memory-health card body — the right-rail content rendered inside a
  // shared `RailCard` (D-121, CONVENTIONS.md §3). It renders the aggregate
  // counters from `memory.health`. Read-only (page-memory.md §12). The
  // page wraps this in `<RailCard title="Memory health">`; this component
  // owns only the card body. Svelte 5 runes mode (D-092); tokens only.
  import type { MemoryHealthAggregate } from '$lib/protocol/memory-types';

  let { aggregate }: { aggregate: MemoryHealthAggregate | null } = $props();

  const driverRows = $derived(
    aggregate ? Object.entries(aggregate.driver_by_scope) : []
  );
</script>

<div class="health-body" aria-label="Memory health">
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
      <h4>Driver by scope</h4>
      <ul class="drivers">
        {#each driverRows as [scope, driver] (scope)}
          <li><span class="scope">{scope}</span><span class="driver">{driver}</span></li>
        {/each}
      </ul>
    {/if}
  {:else}
    <p class="muted">Health counters unavailable.</p>
  {/if}
</div>

<style>
  .health-body {
    display: grid;
    gap: var(--space-2);
  }

  h4 {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    margin: var(--space-2) var(--space-0) var(--space-1);
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
