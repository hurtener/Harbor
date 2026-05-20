<script lang="ts">
  // Harbor Console — a single rendered engine-graph edge (Phase 73i /
  // D-117). A directed connector between two placed nodes, drawn as a
  // simple cubic curve. The optional `saturation` (set by the future
  // 73b Live Runtime consumer) thickens + tints the stroke.

  interface Props {
    x1: number;
    y1: number;
    x2: number;
    y2: number;
    saturation?: number;
  }

  const { x1, y1, x2, y2, saturation }: Props = $props();

  // Cubic control points: pull horizontally toward the midpoint so the
  // edge reads as a left-to-right flow.
  const midX = $derived((x1 + x2) / 2);
  const path = $derived(`M ${x1} ${y1} C ${midX} ${y1}, ${midX} ${y2}, ${x2} ${y2}`);
  const strokeWidth = $derived(1.5 + (saturation ?? 0) * 3);
</script>

<path
  class="graph-edge"
  class:saturated={saturation !== undefined && saturation > 0.66}
  data-testid="graph-edge"
  d={path}
  fill="none"
  stroke-width={strokeWidth}
  marker-end="url(#graph-arrow)"
/>

<style>
  .graph-edge {
    stroke: var(--color-border);
    transition: stroke var(--motion-fast) var(--motion-ease);
  }

  .graph-edge.saturated {
    stroke: var(--color-warning);
  }
</style>
