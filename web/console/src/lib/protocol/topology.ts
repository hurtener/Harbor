/**
 * Topology wire types — the `topology.snapshot` Protocol shape the
 * Console Live Runtime page's topology canvas consumes (Phase 74 /
 * D-114; consumed by Phase 73b / D-126).
 *
 * # Wire types only — the client lives in `client.ts`
 *
 * This module is the wire-type surface only: the request / response
 * shapes the `TopologyNamespace` method (in `client.ts`) consumes and
 * returns. They mirror `internal/protocol/types/topology.go`
 * field-for-field — the Go side is the single source (D-002 / D-093).
 * When `cmd/harbor-gen-protocol-ts` (D-093) ships, these fold into the
 * generated `protocol.ts`.
 *
 * # Phase 73b is composition-only on this surface
 *
 * Phase 73b ships NO new topology Protocol method or wire type — it
 * consumes the already-shipped Phase 74 `topology.snapshot` surface.
 * The Live Runtime topology canvas maps a `TopologyProjection` onto the
 * shared `EngineGraphCanvas`'s `GraphInput` contract.
 */

/** A node's role tag in the engine graph — the closed V1 set. */
export type TopologyNodeKind = 'inlet' | 'node' | 'outlet';

/** One vertex of the projected engine graph. Mirrors `types.TopologyNode`. */
export interface TopologyNode {
  /** The node's unique identifier within the engine. */
  name: string;
  /** The node's role tag. */
  kind: TopologyNodeKind;
}

/**
 * One directed edge of the projected graph plus its live bounded-channel
 * state. Mirrors `types.TopologyEdge`. The Live Runtime canvas renders
 * `queue_depth / queue_capacity` as an edge saturation indicator.
 */
export interface TopologyEdge {
  /** Upstream node name. */
  from: string;
  /** Downstream node name. */
  to: string;
  /** Envelopes currently buffered on the (from → to) channel. */
  queue_depth: number;
  /** The bounded buffer size of the (from → to) channel. */
  queue_capacity: number;
}

/**
 * The canonical Protocol projection of an engine's node graph. Mirrors
 * `types.TopologyProjection`. The `topology.snapshot` method returns one.
 */
export interface TopologyProjection {
  /** The engine the projection describes. */
  engine_id: string;
  /** The wall-clock instant the projection was built (RFC-3339 UTC). */
  occurred_at: string;
  /** The engine's node set, sorted lexicographically by `name`. */
  nodes: TopologyNode[];
  /** The engine's directed edge set, sorted by `(from, to)`. */
  edges: TopologyEdge[];
}
