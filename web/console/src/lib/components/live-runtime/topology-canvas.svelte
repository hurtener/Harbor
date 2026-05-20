<script lang="ts">
  // Harbor Console — Live Runtime topology canvas adapter (Phase 73b /
  // D-126).
  //
  // The Topology tab's primary view. It does NOT fork the engine-graph
  // renderer: it REUSES the shared `<EngineGraphCanvas>` from
  // `components/graph/` (shipped by Phase 73i Flows) — this component is
  // a thin adapter that maps a Protocol `TopologyProjection` onto the
  // canvas's typed `GraphInput` contract (via the pure
  // `projectionToGraph` helper in `$lib/live-runtime/topology-adapter`)
  // and re-emits the node-click.
  //
  // Svelte 5 runes mode (D-092); design tokens only.
  import EngineGraphCanvas from '$lib/components/graph/EngineGraphCanvas.svelte';
  import type { TopologyProjection } from '$lib/protocol/harbor.js';
  import { projectionToGraph } from '$lib/live-runtime/topology-adapter.js';

  let {
    projection,
    selectedNode,
    onnodeclick
  }: {
    /** The engine topology projection from `topology.snapshot`. */
    projection: TopologyProjection;
    /** The currently-selected node name, if any. */
    selectedNode: string | null;
    /** Emitted with a node name when a canvas node is clicked. */
    onnodeclick: (node: string) => void;
  } = $props();

  const graph = $derived(projectionToGraph(projection));
</script>

<div class="topology-canvas" data-testid="topology-canvas">
  <EngineGraphCanvas
    {graph}
    selectedNodeID={selectedNode}
    onselect={(id) => onnodeclick(id)}
  />
</div>

<style>
  .topology-canvas {
    width: 100%;
  }
</style>
