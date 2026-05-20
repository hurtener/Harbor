<script lang="ts">
  // Harbor Console — a single rendered engine-graph node (Phase 73i /
  // D-117). Read-only: it renders a placed node as an SVG group and
  // surfaces a click for the detail popover. NO edit affordances — the
  // Flows page is view-only at V1 (D-063).
  import type { PlacedNode } from './types';

  interface Props {
    node: PlacedNode;
    x: number;
    y: number;
    width: number;
    height: number;
    selected: boolean;
    onselect: (id: string) => void;
    onactivate: (id: string) => void;
  }

  const {
    node,
    x,
    y,
    width,
    height,
    selected,
    onselect,
    onactivate,
  }: Props = $props();

  const label = $derived(node.label ?? node.id);
</script>

<g
  class="graph-node"
  class:selected
  data-testid="graph-node"
  data-node-id={node.id}
  data-node-kind={node.kind}
  role="button"
  tabindex="0"
  aria-label={`Flow node ${label} (${node.kind})`}
  onclick={() => onselect(node.id)}
  ondblclick={() => onactivate(node.id)}
  onkeydown={(e) => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      onselect(node.id);
    }
  }}
>
  <rect {x} {y} {width} {height} rx="6" class={`node-rect kind-${node.kind}`} />
  <text x={x + width / 2} y={y + height / 2} class="node-label">{label}</text>
  <text x={x + width / 2} y={y + height - 6} class="node-kind">{node.kind}</text>
</g>

<style>
  .graph-node {
    cursor: pointer;
  }

  .node-rect {
    fill: var(--color-surface-raised);
    stroke: var(--color-border);
    stroke-width: 1.5;
    transition: stroke var(--motion-fast) var(--motion-ease);
  }

  .graph-node.selected .node-rect {
    stroke: var(--color-accent);
    stroke-width: 2.5;
  }

  .graph-node:focus-visible .node-rect {
    stroke: var(--color-accent);
    stroke-width: 2.5;
  }

  .kind-tool {
    stroke: var(--color-accent);
  }

  .kind-subflow {
    stroke: var(--color-success);
  }

  .kind-pause_point {
    stroke: var(--color-warning);
  }

  .kind-artifact_emitter {
    stroke: var(--color-text-muted);
  }

  .node-label {
    fill: var(--color-text);
    font-size: var(--text-sm);
    font-family: var(--font-sans);
    text-anchor: middle;
    dominant-baseline: middle;
  }

  .node-kind {
    fill: var(--color-text-muted);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    text-anchor: middle;
  }
</style>
