/**
 * Live Runtime — `topology-adapter.ts` unit tests (Phase 73b / D-126).
 *
 * Pins the pure mapping from a Protocol `TopologyProjection` onto the
 * shared `EngineGraphCanvas`'s `GraphInput` contract: node id/kind
 * pass-through, edge from/to, and the `queue_depth / queue_capacity`
 * saturation (including the zero-capacity and over-capacity edges).
 */
import { describe, expect, it } from 'vitest';
import {
	projectionToGraph,
	edgeSaturation,
	legendCounts,
	filterProjection,
	type NodeStateMap
} from '../topology-adapter.js';
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

/* ------------------------------------------------------------------ */
/* Phase 108d Stage 2 — node-state enrichment + legend + filtering     */
/*                                                                     */
/* The validation runtime is planner-shaped and returns               */
/* `unknown_method` for `topology.snapshot` LIVE, so the topology      */
/* graph is verified STRUCTURALLY here against the pure adapter the    */
/* `<TopologyCanvas>` wrapper + shared `<GraphNode>` render verbatim   */
/* (the repo carries no `@testing-library/svelte`, so the structural   */
/* graph assertions are made against this pure layer — the project's   */
/* existing pure-logic test pattern). The fixture carries a FAILED     */
/* terminal node + multiple edges + multiple node states.              */
/* ------------------------------------------------------------------ */

const STAGE2: TopologyProjection = {
	engine_id: 'eng-1',
	occurred_at: '2026-05-18T00:00:00Z',
	nodes: [
		{ name: 'plan', kind: 'inlet' },
		{ name: 'fetch', kind: 'node' },
		{ name: 'aggregate', kind: 'node' },
		{ name: 'queued', kind: 'node' },
		{ name: 'reject', kind: 'outlet' }
	],
	edges: [
		{ from: 'plan', to: 'fetch', queue_depth: 0, queue_capacity: 4 },
		{ from: 'fetch', to: 'aggregate', queue_depth: 2, queue_capacity: 4 },
		{ from: 'fetch', to: 'reject', queue_depth: 4, queue_capacity: 4 }
	]
};

const STATES: NodeStateMap = {
	plan: { state: 'completed' },
	fetch: { state: 'running' },
	aggregate: { state: 'paused' },
	queued: { state: 'pending' },
	reject: { state: 'failed', failureCode: 'E_TOOL' }
};

describe('topology-adapter: node-state enrichment (Stage 2)', () => {
	it('renders all nodes and all edges from the projection', () => {
		const graph = projectionToGraph(STAGE2, STATES);
		expect(graph.nodes).toHaveLength(5);
		expect(graph.edges).toHaveLength(3);
		expect(graph.edges.map((e) => `${e.from}->${e.to}`)).toEqual([
			'plan->fetch',
			'fetch->aggregate',
			'fetch->reject'
		]);
	});

	it('carries Console-derived node state into the shared meta bag', () => {
		const graph = projectionToGraph(STAGE2, STATES);
		expect(graph.nodes.find((n) => n.id === 'fetch')?.meta?.['state']).toBe('running');
	});

	it('styles the failed/reject terminal node with its failure code', () => {
		// The shared GraphNode renders `meta.state === 'failed'` as a red
		// (error) node + the failure code tag.
		const graph = projectionToGraph(STAGE2, STATES);
		const failed = graph.nodes.find((n) => n.id === 'reject');
		expect(failed?.meta?.['state']).toBe('failed');
		expect(failed?.meta?.['failure_code']).toBe('E_TOOL');
	});

	it('fabricates no state when no state map is supplied', () => {
		const graph = projectionToGraph(STAGE2);
		for (const n of graph.nodes) {
			expect(n.meta?.['state']).toBeUndefined();
		}
	});
});

describe('topology-adapter: legendCounts (Stage 2)', () => {
	it('counts each node state for the canvas-corner legend', () => {
		const c = legendCounts(STAGE2, STATES);
		expect(c.all).toBe(5);
		expect(c.running).toBe(1);
		expect(c.pending).toBe(1);
		expect(c.completed).toBe(1);
		expect(c.paused).toBe(1);
		expect(c.failed).toBe(1);
	});

	it('reports zero per-state counts on a runtime with no node states', () => {
		const c = legendCounts(STAGE2, {});
		expect(c.all).toBe(5);
		expect(c.running + c.pending + c.completed + c.paused + c.failed).toBe(0);
	});
});

describe('topology-adapter: filterProjection (Stage 2)', () => {
	it('scopes nodes by status chip and drops dangling edges', () => {
		const f = filterProjection(STAGE2, STATES, 'failed', '');
		expect(f.nodes.map((n) => n.name)).toEqual(['reject']);
		expect(f.edges).toHaveLength(0);
	});

	it('scopes nodes by tool-name substring', () => {
		const f = filterProjection(STAGE2, STATES, 'all', 'fetch');
		expect(f.nodes.map((n) => n.name)).toEqual(['fetch']);
	});

	it('returns the full projection for All + empty query', () => {
		const f = filterProjection(STAGE2, STATES, 'all', '');
		expect(f.nodes).toHaveLength(5);
		expect(f.edges).toHaveLength(3);
	});
});
