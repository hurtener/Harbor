<script lang="ts">
  // Harbor Console — Live Runtime Timeline tab (Phase 73b / D-126).
  //
  // Topology + Timeline are sibling projections of the same
  // `topology.snapshot` data (Brief 11 §LR-2). The Topology tab renders
  // the engine graph via the shared `<EngineGraphCanvas>`; the Timeline
  // tab renders the same node set as a swimlane — one lane per engine
  // node, ordered by the node's graph depth — with the live per-edge
  // queue depth as a lane saturation bar.
  //
  // The Timeline does NOT introduce a parallel topology store (the page
  // plan's §13 guard): it consumes the SAME `TopologyProjection` the
  // Topology tab does, just laid out as horizontal lanes.
  //
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { TopologyProjection } from '$lib/protocol/harbor.js';

  let {
    projection,
    selectedNode,
    onselect
  }: {
    /** The engine topology projection (the Topology-tab data, re-laid). */
    projection: TopologyProjection;
    /** The currently-selected node name, if any. */
    selectedNode: string | null;
    /** Emitted with a node name when a lane is clicked. */
    onselect: (node: string) => void;
  } = $props();

  // Each lane carries the node + its peak outgoing saturation so the
  // operator scans queue pressure top-to-bottom.
  const lanes = $derived(
    projection.nodes.map((node) => {
      const outEdges = projection.edges.filter((e) => e.from === node.name);
      const peak = outEdges.reduce((max, e) => {
        const sat = e.queue_capacity > 0 ? e.queue_depth / e.queue_capacity : 0;
        return Math.max(max, sat);
      }, 0);
      return { name: node.name, kind: node.kind, saturation: peak };
    })
  );
</script>

<div class="timeline" data-testid="timeline-tab">
  {#if lanes.length === 0}
    <p class="timeline-empty" data-testid="timeline-empty">
      The engine graph has no nodes to lay out.
    </p>
  {:else}
    {#each lanes as lane (lane.name)}
      <button
        type="button"
        class="lane"
        class:on={lane.name === selectedNode}
        data-testid="timeline-lane"
        data-node={lane.name}
        onclick={() => onselect(lane.name)}
      >
        <span class="lane-name">{lane.name}</span>
        <span class="lane-kind">{lane.kind}</span>
        <span class="lane-bar" aria-hidden="true">
          <span class="lane-fill" style:width={`${Math.round(lane.saturation * 100)}%`}></span>
        </span>
        <span class="lane-sat">{Math.round(lane.saturation * 100)}%</span>
      </button>
    {/each}
  {/if}
</div>

<style>
  .timeline {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    max-height: var(--size-graph-max-height);
    overflow: auto;
  }

  .timeline-empty {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    padding: var(--space-6);
    text-align: center;
  }

  .lane {
    display: grid;
    grid-template-columns: 1fr auto 2fr auto;
    gap: var(--space-3);
    align-items: center;
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    cursor: pointer;
    text-align: left;
  }

  .lane.on {
    border-color: var(--color-accent);
  }

  .lane-name {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .lane-kind {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .lane-bar {
    display: block;
    height: var(--space-2);
    background: var(--color-surface);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .lane-fill {
    display: block;
    height: 100%;
    background: var(--color-accent);
  }

  .lane-sat {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
