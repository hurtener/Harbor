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

  // Phase 108d (Live Runtime): an OPTIONAL, additive per-node run state +
  // failure code carried in the shared `meta` bag (`meta.state` /
  // `meta.failure_code`). The Live Runtime topology canvas sets it from
  // the live event stream; the Flows page sets no `meta.state`, so
  // `dataStatus` is undefined there and the original kind-based styling
  // is unchanged. A `failed` node renders with a red border + its
  // failure-code tag.
  const dataStatus = $derived(node.meta?.['state']);
  const failureCode = $derived(node.meta?.['failure_code']);
  const isError = $derived(dataStatus === 'failed');
</script>

<g
  class="graph-node"
  class:selected
  class:error={isError}
  data-testid="graph-node"
  data-node-id={node.id}
  data-node-kind={node.kind}
  data-status={dataStatus}
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
  {#if isError && failureCode}
    <text x={x + width / 2} y={y + height - 6} class="node-failure">{failureCode}</text>
  {:else}
    <text x={x + width / 2} y={y + height - 6} class="node-kind">{node.kind}</text>
  {/if}
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

  /* Phase 108d — a failed / reject node renders with a red border. The
     selector is scoped to the error class, so non-Live-Runtime consumers
     (Flows, which sets no `meta.state`) are unaffected. */
  .graph-node.error .node-rect {
    stroke: var(--color-danger);
    stroke-width: 2;
  }

  .node-failure {
    fill: var(--color-danger);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    text-anchor: middle;
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
