<script lang="ts">
  // Harbor Console — read-only engine-graph canvas (Phase 73i / D-117).
  //
  // SHARED COMPONENT: this canvas is consumed by the Phase 73i Flows
  // page AND (later) the Phase 73b Live Runtime topology view (Brief 11
  // §LR shared component family). It renders a `GraphInput` — a typed
  // node/edge set decoupled from any one Protocol wire type — as a
  // layered SVG DAG.
  //
  // It is VIEW-ONLY (D-063): there are NO `Add node` / `Delete edge` /
  // `Save graph` affordances — those controls do not render at V1, by
  // construction. The only interactions are: click a node → emit a
  // selection; double-click → emit an activation (the Flows page routes
  // a tool node's activation to `/console/tools/<id>`).
  import GraphNode from './GraphNode.svelte';
  import GraphEdge from './GraphEdge.svelte';
  import { layoutGraph, resolveEdge } from './layout';
  import type { GraphInput, PlacedNode } from './types';

  interface Props {
    graph: GraphInput;
    selectedNodeID?: string | null;
    onselect?: (id: string) => void;
    onactivate?: (id: string) => void;
  }

  const {
    graph,
    selectedNodeID = null,
    onselect,
    onactivate,
  }: Props = $props();

  const NODE_W = 132;
  const NODE_H = 56;
  const COL_GAP = 80;
  const ROW_GAP = 28;
  const PAD = 24;

  const layout = $derived(layoutGraph(graph));

  const placedByID = $derived(
    new Map<string, PlacedNode>(layout.nodes.map((n) => [n.id, n])),
  );

  function nodeX(n: PlacedNode): number {
    return PAD + n.column * (NODE_W + COL_GAP);
  }

  function nodeY(n: PlacedNode): number {
    return PAD + n.row * (NODE_H + ROW_GAP);
  }

  const canvasWidth = $derived(
    PAD * 2 + Math.max(1, layout.columns) * (NODE_W + COL_GAP) - COL_GAP,
  );
  const canvasHeight = $derived(
    PAD * 2 + Math.max(1, layout.rows) * (NODE_H + ROW_GAP) - ROW_GAP,
  );

  const edgeLines = $derived(
    graph.edges
      .map((e) => {
        const r = resolveEdge(e, placedByID);
        if (!r) {
          return null;
        }
        return {
          key: `${e.from}->${e.to}`,
          x1: nodeX(r.from) + NODE_W,
          y1: nodeY(r.from) + NODE_H / 2,
          x2: nodeX(r.to),
          y2: nodeY(r.to) + NODE_H / 2,
          saturation: e.saturation,
        };
      })
      .filter((v): v is NonNullable<typeof v> => v !== null),
  );

  const isEmpty = $derived(graph.nodes.length === 0);
</script>

<div class="graph-canvas" data-testid="engine-graph-canvas">
  {#if isEmpty}
    <p class="graph-empty" data-testid="graph-empty">
      This flow has no nodes to render.
    </p>
  {:else}
    <svg
      viewBox={`0 0 ${canvasWidth} ${canvasHeight}`}
      width={canvasWidth}
      height={canvasHeight}
      role="img"
      aria-label="Engine graph"
    >
      <defs>
        <marker
          id="graph-arrow"
          viewBox="0 0 10 10"
          refX="9"
          refY="5"
          markerWidth="7"
          markerHeight="7"
          orient="auto-start-reverse"
        >
          <path d="M 0 0 L 10 5 L 0 10 z" class="arrow-head" />
        </marker>
      </defs>
      {#each edgeLines as edge (edge.key)}
        <GraphEdge
          x1={edge.x1}
          y1={edge.y1}
          x2={edge.x2}
          y2={edge.y2}
          saturation={edge.saturation}
        />
      {/each}
      {#each layout.nodes as node (node.id)}
        <GraphNode
          {node}
          x={nodeX(node)}
          y={nodeY(node)}
          width={NODE_W}
          height={NODE_H}
          selected={node.id === selectedNodeID}
          onselect={(id) => onselect?.(id)}
          onactivate={(id) => onactivate?.(id)}
        />
      {/each}
    </svg>
  {/if}
</div>

<style>
  .graph-canvas {
    overflow: auto;
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-2);
    max-height: var(--size-graph-max-height);
  }

  .graph-empty {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    padding: var(--space-6);
    text-align: center;
  }

  .arrow-head {
    fill: var(--color-border);
  }
</style>
