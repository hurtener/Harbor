// Harbor Console — shared engine-graph canvas types (Phase 73i / D-117).
//
// `GraphInput` is the typed contract the read-only `EngineGraphCanvas`
// renders. It is deliberately decoupled from any single Protocol wire
// type so two consumers can share the renderer (Brief 11 §LR shared
// component family):
//
//   - Phase 73i Flows page — feeds a flow's `FlowDescription` graph.
//   - Phase 73b Live Runtime — feeds the engine `TopologyProjection`.
//
// The Flows page establishes this interface; 73b consumes it. The
// shape is intentionally minimal — node id + kind + label, edge
// from/to — so a richer 73b node-state model (live queue depth, etc.)
// extends it additively via the optional `meta` bag rather than a
// breaking change.

/** A node's visual role in the graph. The renderer maps each kind to a
 *  distinct accent + glyph. */
export type GraphNodeKind =
  | 'subflow'
  | 'tool'
  | 'pause_point'
  | 'artifact_emitter'
  | 'inlet'
  | 'outlet'
  | 'node';

/** One vertex of the rendered graph. */
export interface GraphNodeInput {
  /** Unique id within the graph. */
  id: string;
  /** Display label. Defaults to `id` when absent. */
  label?: string;
  /** Visual role tag. */
  kind: GraphNodeKind;
  /**
   * Optional per-consumer metadata bag. The Flows page stuffs the
   * node descriptor + policy summary here; 73b will stuff live queue
   * depth. The renderer surfaces it in the node popover.
   */
  meta?: Readonly<Record<string, string>>;
}

/** One directed edge of the rendered graph. */
export interface GraphEdgeInput {
  /** Upstream node id. */
  from: string;
  /** Downstream node id. */
  to: string;
  /**
   * Optional saturation in [0,1] the renderer draws as edge thickness
   * / colour. The Flows page leaves it undefined (static graph); 73b
   * sets it from live queue depth.
   */
  saturation?: number;
}

/** The full input the `EngineGraphCanvas` renders. */
export interface GraphInput {
  /** The graph's node set. */
  nodes: GraphNodeInput[];
  /** The graph's directed edge set. */
  edges: GraphEdgeInput[];
}

/** A laid-out node — a `GraphNodeInput` plus its computed canvas
 *  coordinates. The renderer's layout pass produces these. */
export interface PlacedNode extends GraphNodeInput {
  /** Column index in the topological layering. */
  column: number;
  /** Row index within the column. */
  row: number;
}
