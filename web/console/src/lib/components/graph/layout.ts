// Harbor Console — engine-graph layered layout (Phase 73i / D-117).
//
// A pure, deterministic layout pass: it assigns each node a (column,
// row) coordinate by longest-path layering over the DAG. It is
// extracted from the `EngineGraphCanvas` component so it is unit-
// testable without a DOM (Vitest); the component is the thin SVG
// renderer over its output.
//
// The layout is byte-stable: two calls with the same `GraphInput`
// (nodes/edges in the same order) produce identical `PlacedNode`
// arrays — the property the Playwright spec's node-count assertion
// rests on.

import type { GraphEdgeInput, GraphInput, PlacedNode } from './types';

/** The computed layout: placed nodes + their bounding column/row span. */
export interface GraphLayout {
  nodes: PlacedNode[];
  /** Number of columns (the graph's longest-path depth). */
  columns: number;
  /** Tallest column's row count. */
  rows: number;
}

/**
 * Compute a layered layout for a DAG. Nodes with no incoming edge land
 * in column 0; every other node lands one column past its deepest
 * parent. A cycle (which an engine graph should never carry) is broken
 * defensively — a node already assigned keeps its column.
 */
export function layoutGraph(input: GraphInput): GraphLayout {
  const nodeIDs = input.nodes.map((n) => n.id);
  const idSet = new Set(nodeIDs);
  // Adjacency: parents per node.
  const parents = new Map<string, string[]>();
  for (const id of nodeIDs) {
    parents.set(id, []);
  }
  for (const e of input.edges) {
    if (idSet.has(e.from) && idSet.has(e.to)) {
      parents.get(e.to)!.push(e.from);
    }
  }

  // Longest-path column assignment via memoised DFS.
  const column = new Map<string, number>();
  const visiting = new Set<string>();

  function depth(id: string): number {
    const cached = column.get(id);
    if (cached !== undefined) {
      return cached;
    }
    if (visiting.has(id)) {
      // Defensive cycle break — should not happen on an engine DAG.
      return 0;
    }
    visiting.add(id);
    const ps = parents.get(id) ?? [];
    let d = 0;
    for (const p of ps) {
      d = Math.max(d, depth(p) + 1);
    }
    visiting.delete(id);
    column.set(id, d);
    return d;
  }

  for (const id of nodeIDs) {
    depth(id);
  }

  // Row assignment: stable order within a column (input order).
  const rowCursor = new Map<number, number>();
  const placed: PlacedNode[] = input.nodes.map((n) => {
    const col = column.get(n.id) ?? 0;
    const row = rowCursor.get(col) ?? 0;
    rowCursor.set(col, row + 1);
    return { ...n, column: col, row };
  });

  const columns = placed.reduce((m, n) => Math.max(m, n.column + 1), 0);
  const rows = [...rowCursor.values()].reduce((m, r) => Math.max(m, r), 0);
  return { nodes: placed, columns, rows };
}

/**
 * Resolve an edge's endpoints to their placed nodes. Returns null when
 * either endpoint is missing — the caller skips drawing a dangling
 * edge rather than crashing.
 */
export function resolveEdge(
  edge: GraphEdgeInput,
  placed: ReadonlyMap<string, PlacedNode>,
): { from: PlacedNode; to: PlacedNode } | null {
  const from = placed.get(edge.from);
  const to = placed.get(edge.to);
  if (!from || !to) {
    return null;
  }
  return { from, to };
}
