<script lang="ts">
  // Harbor Console — Live Runtime header status-counter strip
  // (Phase 73b / D-126).
  //
  // The page's header-level five-chip aggregate: pending / running /
  // completed / paused / failed counts for the page's session. Fed by
  // the `tasks.list` status-counter-strip aggregate (identity-scoped,
  // computed server-side) and updated live as the SSE stream delivers
  // `task.*` events. Distinct from the canvas-level status legend.
  //
  // The strip shape + the live-delta reducer are pure logic in
  // `$lib/live-runtime/strip.ts` (Vitest-tested); this component is the
  // render surface.
  //
  // Svelte 5 runes mode (D-092); design tokens only — no raw literals.
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import type { StatusKind } from '$lib/components/ui/StatusChip.svelte';
  import type { StatusCounterStrip } from '$lib/live-runtime/strip.js';

  let { strip }: { strip: StatusCounterStrip } = $props();

  // The five chips, in the page-spec mockup order, each mapped to a
  // StatusChip kind off the shared status token scale.
  //
  // N14 (Phase 83x): the legacy labels (`Pending` / `Running` / ...)
  // hid the fact that these are currently-in-status counts, not
  // historical totals. An operator who watched a task complete and
  // then read `Completed 0` mis-read the strip as broken — the
  // initial-load aggregate IS a point-in-time snapshot. Suffix each
  // label with "(now)" so the semantics are explicit.
  const chips = $derived<{ key: keyof StatusCounterStrip; label: string; kind: StatusKind }[]>([
    { key: 'pending', label: 'Pending (now)', kind: 'neutral' },
    { key: 'running', label: 'Running (now)', kind: 'accent' },
    { key: 'completed', label: 'Completed (now)', kind: 'success' },
    { key: 'paused', label: 'Paused (now)', kind: 'warning' },
    { key: 'failed', label: 'Failed (now)', kind: 'danger' }
  ]);
</script>

<div class="counter-strip" data-testid="status-counter-strip">
  {#each chips as chip (chip.key)}
    <div class="counter-chip" data-testid={`counter-${chip.key}`}>
      <StatusChip kind={chip.kind} label={chip.label} />
      <span class="count" data-testid={`counter-${chip.key}-count`}>{strip[chip.key]}</span>
    </div>
  {/each}
</div>

<style>
  .counter-strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
    align-items: center;
  }

  .counter-chip {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
  }

  .count {
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }
</style>
