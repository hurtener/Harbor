/**
 * Live Runtime topology adapter — the pure mapping from a Protocol
 * `TopologyProjection` onto the shared `EngineGraphCanvas`'s typed
 * `GraphInput` contract (Phase 73b / D-126; extended Phase 108d Stage 2).
 *
 * # Why a separate TS module
 *
 * The mapping is pure logic — no DOM, no reactivity — so it lives in a
 * `.ts` module a Vitest unit test imports directly (the repo's
 * pure-logic test pattern; `@testing-library/svelte` is NOT a dependency,
 * so the structural graph assertions are made against these functions,
 * not a rendered component). The `<TopologyCanvas>` wrapper calls them;
 * the renderer itself is the shared `<EngineGraphCanvas>` (Phase 73i),
 * NOT forked.
 *
 * # Node STATE is Console-side enrichment (Phase 108d)
 *
 * The Protocol `TopologyProjection` (mirroring `types.TopologyProjection`)
 * carries node identity + kind + edge queue depth — it does NOT carry a
 * per-node run STATE. The mock's status legend (Running / Queued /
 * Completed / Paused / Failed) and its red failed/reject terminal nodes
 * are therefore driven by a Console-side `NodeStateMap` the Live Runtime
 * page derives from the live event stream (`task.*` / `planner.*` keyed
 * by node name). This keeps the Console an honest Protocol client
 * (CLAUDE.md §13 — no fabricated wire fields): when the map is empty
 * (today's planner/RunLoop runtime, which returns `unknown_method` for
 * `topology.snapshot` and emits no node states), the legend reads all
 * zeros and no node is styled failed — nothing is invented. When the map
 * is populated (a real engine run, or a structural test fixture), the
 * legend + failed styling light up. The state is carried to the shared
 * `GraphNode` via the additive `meta` bag (`meta.state` / `meta.failure_code`),
 * which the shared types already reserve for "a richer 73b node-state
 * model" — Flows sets no `meta.state`, so it is unaffected.
 *
 * # Composition-only on the topology WIRE surface
 *
 * No new topology Protocol type — `projectionToGraph` consumes the
 * already-shipped Phase 74 `TopologyProjection` and emits the
 * already-shipped Phase 73i `GraphInput`. The `inlet` / `node` / `outlet`
 * topology kinds are valid `GraphNodeKind`s, so the node-kind passes
 * through unchanged.
 */

import type { GraphInput } from '$lib/components/graph/types';
import type { TopologyProjection } from '$lib/protocol/topology.js';

/**
 * A node's live run state — the closed set the status legend categorises.
 * Console-side enrichment derived from the event stream; NOT a Protocol
 * wire field (see the module doc).
 */
export type TopologyNodeState = 'pending' | 'running' | 'completed' | 'paused' | 'failed';

/**
 * One node's Console-derived run state. `failureCode` is set only for a
 * `failed` node and renders as the node's failure-code tag.
 */
export interface NodeState {
  /** The node's live run state. */
  state: TopologyNodeState;
  /** The failure code/class for a `failed` node (renders as a tag). */
  failureCode?: string;
}

/** Node-name → Console-derived run state. Empty on runtimes that emit none. */
export type NodeStateMap = Readonly<Record<string, NodeState>>;

/**
 * Maps a Protocol {@link TopologyProjection} onto the shared
 * {@link GraphInput}. A topology node → a graph node (kind passed
 * through); a topology edge → a graph edge with
 * `saturation = queue_depth / queue_capacity` clamped to [0,1].
 *
 * When a {@link NodeStateMap} is supplied, each node's Console-derived
 * state is carried into the additive `meta` bag (`meta.state` +, for a
 * failed node, `meta.failure_code`) so the shared `GraphNode` can style a
 * failed node red + show its failure-code tag. Omitting the map (the
 * default) reproduces the original Phase 73b behaviour exactly.
 */
export function projectionToGraph(
  projection: TopologyProjection,
  states: NodeStateMap = {}
): GraphInput {
  return {
    nodes: projection.nodes.map((n) => {
      const st = states[n.name];
      const meta: Record<string, string> = {};
      if (st) {
        meta.state = st.state;
        if (st.failureCode) {
          meta.failure_code = st.failureCode;
        }
      }
      return {
        id: n.name,
        label: n.name,
        kind: n.kind,
        meta
      };
    }),
    edges: projection.edges.map((e) => ({
      from: e.from,
      to: e.to,
      saturation: edgeSaturation(e.queue_depth, e.queue_capacity)
    }))
  };
}

/**
 * Computes a bounded-channel saturation in [0,1] from the live queue
 * depth + capacity. A zero capacity yields 0 (no division-by-zero); a
 * depth above capacity is clamped to 1.
 */
export function edgeSaturation(queueDepth: number, queueCapacity: number): number {
  if (queueCapacity <= 0) {
    return 0;
  }
  return Math.min(1, Math.max(0, queueDepth / queueCapacity));
}

/** The per-state legend counts the canvas-corner legend renders. */
export interface LegendCounts {
  /** Total node count (the `All` filter target). */
  all: number;
  pending: number;
  running: number;
  completed: number;
  paused: number;
  failed: number;
}

/**
 * Counts the projection's nodes per Console-derived run state. A node
 * with no entry in the {@link NodeStateMap} contributes only to `all`
 * (it is un-categorised — honest, not fabricated as any state).
 */
export function legendCounts(
  projection: TopologyProjection,
  states: NodeStateMap = {}
): LegendCounts {
  const c: LegendCounts = {
    all: projection.nodes.length,
    pending: 0,
    running: 0,
    completed: 0,
    paused: 0,
    failed: 0
  };
  for (const n of projection.nodes) {
    const st = states[n.name];
    if (!st) {
      continue;
    }
    c[st.state] += 1;
  }
  return c;
}

/** The status-chip filter selection. `'all'` renders every node. */
export type TopologyStatusFilter = 'all' | TopologyNodeState;

/**
 * Re-scopes a projection to the nodes matching the active status chip +
 * the tool-name substring (case-insensitive over the node name) — purely
 * local UI state, no Protocol call. An edge survives only when BOTH of
 * its endpoints survive.
 */
export function filterProjection(
  projection: TopologyProjection,
  states: NodeStateMap,
  status: TopologyStatusFilter,
  toolQuery: string
): TopologyProjection {
  const tool = toolQuery.trim().toLowerCase();
  const nodes = projection.nodes.filter((n) => {
    const st = states[n.name];
    const statusOK = status === 'all' || (st?.state ?? undefined) === status;
    const toolOK =
      tool === '' || n.name.toLowerCase().includes(tool) || n.kind.toLowerCase().includes(tool);
    return statusOK && toolOK;
  });
  const ids = new Set(nodes.map((n) => n.name));
  const edges = projection.edges.filter((e) => ids.has(e.from) && ids.has(e.to));
  return {
    engine_id: projection.engine_id,
    occurred_at: projection.occurred_at,
    nodes,
    edges
  };
}
