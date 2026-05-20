/**
 * Live Runtime — `topology-adapter.ts` unit tests (Phase 73b / D-126).
 *
 * Pins the pure mapping from a Protocol `TopologyProjection` onto the
 * shared `EngineGraphCanvas`'s `GraphInput` contract: node id/kind
 * pass-through, edge from/to, and the `queue_depth / queue_capacity`
 * saturation (including the zero-capacity and over-capacity edges).
 */
import { describe, expect, it } from 'vitest';
import { projectionToGraph, edgeSaturation } from '../topology-adapter.js';
import type { TopologyProjection } from '../../protocol/topology.js';

const SAMPLE: TopologyProjection = {
	engine_id: 'engine-1',
	occurred_at: '2026-05-20T12:00:00Z',
	nodes: [
		{ name: 'ingress', kind: 'inlet' },
		{ name: 'router', kind: 'node' },
		{ name: 'egress', kind: 'outlet' }
	],
	edges: [
		{ from: 'ingress', to: 'router', queue_depth: 2, queue_capacity: 8 },
		{ from: 'router', to: 'egress', queue_depth: 0, queue_capacity: 0 }
	]
};

describe('topology-adapter: projectionToGraph', () => {
	it('maps every topology node to a graph node with kind passed through', () => {
		const graph = projectionToGraph(SAMPLE);
		expect(graph.nodes).toHaveLength(3);
		expect(graph.nodes[0]).toMatchObject({ id: 'ingress', label: 'ingress', kind: 'inlet' });
		expect(graph.nodes[2]).toMatchObject({ id: 'egress', kind: 'outlet' });
	});

	it('maps every topology edge to a graph edge with from/to and saturation', () => {
		const graph = projectionToGraph(SAMPLE);
		expect(graph.edges).toHaveLength(2);
		expect(graph.edges[0]).toMatchObject({ from: 'ingress', to: 'router' });
		// 2 / 8 = 0.25
		expect(graph.edges[0].saturation).toBeCloseTo(0.25);
		// zero-capacity edge → 0 saturation, no division-by-zero
		expect(graph.edges[1].saturation).toBe(0);
	});

	it('an empty projection maps to an empty graph', () => {
		const graph = projectionToGraph({
			engine_id: 'e',
			occurred_at: '2026-05-20T12:00:00Z',
			nodes: [],
			edges: []
		});
		expect(graph.nodes).toHaveLength(0);
		expect(graph.edges).toHaveLength(0);
	});
});

describe('topology-adapter: edgeSaturation', () => {
	it('computes depth / capacity', () => {
		expect(edgeSaturation(4, 8)).toBe(0.5);
	});

	it('clamps an over-capacity depth to 1', () => {
		expect(edgeSaturation(20, 8)).toBe(1);
	});

	it('returns 0 for a zero or negative capacity', () => {
		expect(edgeSaturation(5, 0)).toBe(0);
		expect(edgeSaturation(5, -1)).toBe(0);
	});
});
