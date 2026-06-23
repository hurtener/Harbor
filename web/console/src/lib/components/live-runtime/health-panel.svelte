<script lang="ts">
  // Harbor Console — Live Runtime cockpit Health panel (Phase 108e / D-177).
  //
  // Per-subsystem readiness pills from the SHIPPED `runtime.health` rollup
  // (72f / D-111) — the SAME surface the Overview page consumes, so on the
  // dev runtime this renders REAL data. The page self-probes the surface
  // (a spine panel, not hard-gated — see panels.ts §4.3 note); on a throw /
  // `unknown_method` the page passes `health = null` and this renders an
  // honest "health not available on this runtime" state. No fabrication
  // (CLAUDE.md §13).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import HeartPulse from '@lucide/svelte/icons/heart-pulse';
  import type { RuntimeHealth, MetricsSnapshot } from '$lib/protocol/posture.js';
  import { runtimeGaugesFrom, formatGaugeValue } from '$lib/live-runtime/health-gauges.js';

  let {
    health,
    metrics = null,
  }: { health: RuntimeHealth | null; metrics?: MetricsSnapshot | null } = $props();

  function pillKind(status: string): 'ready' | 'degraded' | 'unavailable' {
    if (status === 'ready') return 'ready';
    if (status === 'degraded') return 'degraded';
    return 'unavailable';
  }

  // Runtime observability gauges (Phase 120) projected through the SHIPPED
  // metrics.snapshot. Projection logic lives in health-gauges.ts (unit-tested);
  // this component only renders. Raw Go/process collectors stay on the
  // operator /metrics scrape, off the Protocol — so they never appear here.
  const runtimeGauges = $derived(runtimeGaugesFrom(metrics));
  const hasHealth = $derived(health !== null && health.subsystems.length > 0);
  const hasGauges = $derived(runtimeGauges.length > 0);
</script>

{#if hasHealth || hasGauges}
  {#if hasHealth}
    <ul class="health-pills" data-testid="health-pills">
      {#each health!.subsystems as sub (sub.subsystem)}
        <li class="pill-row" data-testid="health-pill" data-status={pillKind(sub.status)}>
          <span class="sub-name">{sub.subsystem}</span>
          <span class="sub-status" data-status={pillKind(sub.status)}>{sub.status}</span>
        </li>
      {/each}
    </ul>
  {/if}
  {#if hasGauges}
    <section class="gauges" data-testid="runtime-gauges" aria-label="Runtime gauges">
      <h4 class="gauges-heading">Runtime gauges</h4>
      <ul class="gauge-rows">
        {#each runtimeGauges as g (g.name)}
          <li class="gauge-row" data-testid="runtime-gauge">
            <span class="gauge-label">{g.label}</span>
            <span class="gauge-value">{formatGaugeValue(g.value)}</span>
          </li>
        {/each}
      </ul>
    </section>
  {/if}
{:else}
  <div class="health-empty" data-testid="health-panel-empty">
    <span class="empty-icon"><HeartPulse size={20} aria-hidden="true" /></span>
    <p class="empty-headline">Health not available on this runtime</p>
    <p class="empty-detail">
      This runtime does not advertise a health surface. Per-subsystem readiness
      and runtime gauges appear here on runtimes that expose them.
    </p>
  </div>
{/if}

<style>
  .health-pills {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .pill-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--text-sm);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
  }

  .sub-name {
    color: var(--color-text);
  }

  .sub-status {
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .sub-status[data-status='ready'] {
    color: var(--color-success);
  }

  .sub-status[data-status='degraded'] {
    color: var(--color-warning);
  }

  .sub-status[data-status='unavailable'] {
    color: var(--color-danger);
  }

  .gauges {
    margin-top: var(--space-2);
    padding-top: var(--space-2);
    border-top: var(--border-hairline);
  }

  .gauges-heading {
    margin: var(--space-0) var(--space-0) var(--space-1);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .gauge-rows {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .gauge-row {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }

  .gauge-label {
    color: var(--color-text-muted);
  }

  .gauge-value {
    font-variant-numeric: tabular-nums;
    font-weight: 600;
    color: var(--color-text);
  }

  .health-empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-5) var(--space-2);
    text-align: center;
  }

  .empty-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--size-avatar-md);
    height: var(--size-avatar-md);
    border-radius: var(--radius-md);
    margin-bottom: var(--space-1);
    background: var(--color-success-soft);
    color: var(--color-success);
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
