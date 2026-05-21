<script lang="ts">
  // Harbor Console — Overview health-chip strip (Phase 73a / D-127).
  //
  // The sub-header strip (page-overview.md §12 — "Refinements to §4")
  // between the counter row and the rest of the canvas. It renders one
  // chip per subsystem from the SHIPPED `runtime.health` snapshot
  // (Phase 72f / D-111) — chip-shape, not banner-shape. Each chip's
  // colour maps over the StatusChip status-token scale.
  //
  // This strip is fed its own nested `<PageState>` by the page so a
  // health-load failure surfaces in the strip, not the whole page
  // (CONVENTIONS.md §4 — nested PageState).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import StatusChip, { type StatusKind } from '$lib/components/ui/StatusChip.svelte';
  import type { SubsystemHealth } from '$lib/protocol/posture.js';

  let {
    subsystems
  }: {
    /** The per-subsystem readiness rollup from `runtime.health`. */
    subsystems: SubsystemHealth[];
  } = $props();

  // Map the closed three-value health status onto the StatusChip kinds.
  function kindOf(status: SubsystemHealth['status']): StatusKind {
    switch (status) {
      case 'ready':
        return 'success';
      case 'degraded':
        return 'warning';
      case 'unavailable':
        return 'danger';
      default:
        return 'neutral';
    }
  }
</script>

<div class="health-strip" data-testid="health-chip-strip">
  {#each subsystems as sub (sub.subsystem)}
    <span class="chip-wrap" title={sub.detail ?? sub.status}>
      <StatusChip kind={kindOf(sub.status)} label={`${sub.subsystem}: ${sub.status}`} />
    </span>
  {/each}
</div>

<style>
  .health-strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-0);
  }

  .chip-wrap {
    display: inline-flex;
  }
</style>
