<script lang="ts">
  // Events page — per-tenant attribution breakdown (D-307 / HA-17).
  //
  // On a cross-tenant (admin-widened) event-rate read, the runtime returns
  // per-tenant attribution (`EventBucket.counts_by_tenant`) alongside the
  // totals. This card renders that breakdown so a fleet operator can see
  // WHICH tenants contributed and independently VERIFY the tenant boundary
  // the runtime enforced — the defence-in-depth an aggregate otherwise
  // lacks (a bag of scalars with no per-row tenant to post-filter against,
  // unlike a row read). The projection (`attribution.ts`) also checks the
  // per-bucket reconciliation invariant `Σ counts_by_tenant == counts` and
  // flags any tenant outside the entitled set — both surfaced loudly here,
  // never silently dropped (CLAUDE.md §13).
  //
  // Page-specific component (`components/events/` per CONVENTIONS.md §3).
  // Svelte 5 runes (D-092); design tokens only; a PURE read — no Protocol
  // mutation fires from this component.
  import type { TenantAttribution } from '$lib/events/attribution.js';

  let {
    attribution
  }: {
    /** The projected per-tenant breakdown, or null when absent. */
    attribution: TenantAttribution | null;
  } = $props();

  /** The per-tenant rows to render (empty when attribution is null). */
  const rows = $derived(attribution?.rows ?? []);
</script>

{#if attribution}
  <section
    class="panel card attribution-card"
    data-testid="events-tenant-attribution"
  >
    <header class="attribution-head">
      <h2 class="panel-title">Per-tenant attribution</h2>
      <span class="attribution-total" data-testid="attribution-total">
        {attribution.total} event{attribution.total === 1 ? '' : 's'}
      </span>
    </header>

    <p class="attribution-note">
      The widened read fans in across the tenants you asked for; these counts
      re-project the same events by tenant so you can verify the boundary the
      runtime enforced.
    </p>

    {#if !attribution.reconciles}
      <p class="attribution-warn" data-testid="attribution-reconcile-warn">
        Attribution did not reconcile with the totals — the per-tenant counts
        do not sum to the bucket totals. Do not trust this breakdown; report a
        runtime defect.
      </p>
    {/if}
    {#if attribution.hasUnexpected}
      <p class="attribution-warn" data-testid="attribution-unexpected-warn">
        A tenant outside your entitled set appeared in the attribution
        (flagged below). This is a boundary violation — report a runtime
        defect.
      </p>
    {/if}

    <table class="attribution-table">
      <thead>
        <tr>
          <th scope="col">Tenant</th>
          <th scope="col" class="num">Events</th>
          <th scope="col" class="num">Share</th>
        </tr>
      </thead>
      <tbody>
        {#each rows as row (row.tenant)}
          <tr class:unexpected={row.unexpected} data-testid="attribution-row">
            <td class="tenant-cell">
              <code>{row.tenant}</code>
              {#if row.unexpected}
                <span class="unexpected-badge" title="Outside your entitled set">
                  unexpected
                </span>
              {/if}
            </td>
            <td class="num" data-testid="attribution-row-total">{row.total}</td>
            <td class="num">
              {attribution.total > 0
                ? `${Math.round((row.total / attribution.total) * 100)}%`
                : '—'}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </section>
{/if}

<style>
  .attribution-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .attribution-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .attribution-total {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    font-variant-numeric: var(--font-variant-tabular);
  }

  .attribution-note {
    margin: 0;
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .attribution-warn {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    background: var(--color-danger-soft);
    color: var(--color-danger);
    font-size: var(--text-xs);
  }

  .attribution-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  .attribution-table th,
  .attribution-table td {
    padding: var(--space-2) var(--space-3);
    text-align: left;
    border-bottom: var(--size-px) solid var(--color-border);
  }

  .attribution-table th {
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-text-muted);
  }

  .attribution-table .num {
    text-align: right;
    font-variant-numeric: var(--font-variant-tabular);
  }

  .tenant-cell {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .tenant-cell code {
    font-family: var(--font-mono);
    font-size: var(--size-code-font);
    color: var(--color-text);
  }

  tr.unexpected {
    background: var(--color-danger-soft);
  }

  .unexpected-badge {
    padding: var(--space-05) var(--space-2);
    border-radius: var(--radius-pill);
    background: var(--color-danger);
    color: var(--color-bg);
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
</style>
