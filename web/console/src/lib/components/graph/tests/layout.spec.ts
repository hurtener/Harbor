// Harbor Console — engine-graph layout tests (Phase 73i / D-117).

import { describe, expect, it } from 'vitest';
import { layoutGraph, resolveEdge } from '../layout';
import type { GraphInput, PlacedNode } from '../types';

const linearGraph: GraphInput = {
  nodes: [
    { id: 'a', kind: 'inlet' },
    { id: 'b', kind: 'tool' },
    { id: 'c', kind: 'outlet' },
  ],
  edges: [
    { from: 'a', to: 'b' },
    { from: 'b', to: 'c' },
  ],
};

describe('layoutGraph', () => {
  it('places a linear chain in successive columns', () => {
    const layout = layoutGraph(linearGraph);
    expect(layout.columns).toBe(3);
    const byID = new Map(layout.nodes.map((n) => [n.id, n]));
    expect(byID.get('a')!.column).toBe(0);
    expect(byID.get('b')!.column).toBe(1);
    expect(byID.get('c')!.column).toBe(2);
  });

  it('places diverging branches in the same column', () => {
    const diamond: GraphInput = {
      nodes: [
        { id: 'a', kind: 'inlet' },
        { id: 'b', kind: 'tool' },
        { id: 'c', kind: 'tool' },
        { id: 'd', kind: 'outlet' },
      ],
      edges: [
        { from: 'a', to: 'b' },
        { from: 'a', to: 'c' },
        { from: 'b', to: 'd' },
        { from: 'c', to: 'd' },
      ],
    };
    const layout = layoutGraph(diamond);
    const byID = new Map(layout.nodes.map((n) => [n.id, n]));
    expect(byID.get('b')!.column).toBe(1);
    expect(byID.get('c')!.column).toBe(1);
    expect(byID.get('b')!.row).not.toBe(byID.get('c')!.row);
    expect(byID.get('d')!.column).toBe(2);
  });

  it('is deterministic — two calls produce identical layouts', () => {
    const a = layoutGraph(linearGraph);
    const b = layoutGraph(linearGraph);
    expect(JSON.stringify(a)).toBe(JSON.stringify(b));
  });

  it('handles an empty graph', () => {
    const layout = layoutGraph({ nodes: [], edges: [] });
    expect(layout.nodes).toHaveLength(0);
    expect(layout.columns).toBe(0);
  });

  it('ignores edges referencing missing nodes', () => {
    const layout = layoutGraph({
      nodes: [{ id: 'a', kind: 'tool' }],
      edges: [{ from: 'a', to: 'ghost' }],
    });
    expect(layout.nodes).toHaveLength(1);
    expect(layout.nodes[0].column).toBe(0);
  });
});

describe('resolveEdge', () => {
  it('resolves both endpoints', () => {
    const placed = new Map<string, PlacedNode>(
      layoutGraph(linearGraph).nodes.map((n) => [n.id, n]),
    );
    const r = resolveEdge({ from: 'a', to: 'b' }, placed);
    expect(r).not.toBeNull();
    expect(r!.from.id).toBe('a');
    expect(r!.to.id).toBe('b');
  });

  it('returns null for a dangling edge', () => {
    const placed = new Map<string, PlacedNode>(
      layoutGraph(linearGraph).nodes.map((n) => [n.id, n]),
    );
    expect(resolveEdge({ from: 'a', to: 'ghost' }, placed)).toBeNull();
  });
});
