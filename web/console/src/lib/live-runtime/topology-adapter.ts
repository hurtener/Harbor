/**
 * Live Runtime topology adapter — the pure mapping from a Protocol
 * `TopologyProjection` onto the shared `EngineGraphCanvas`'s typed
 * `GraphInput` contract (Phase 73b / D-126).
 *
 * # Why a separate TS module
 *
 * The mapping is pure logic — no DOM, no reactivity — so it lives in a
 * `.ts` module a Vitest unit test imports directly. The
 * `<TopologyCanvas>` component re-exports + calls it; the renderer
 * itself is the shared `<EngineGraphCanvas>` (Phase 73i), NOT forked.
 *
 * # Composition-only on the topology surface
 *
 * Phase 73b ships NO new topology Protocol type — `projectionToGraph`
 * consumes the already-shipped Phase 74 `TopologyProjection` and emits
 * the already-shipped Phase 73i `GraphInput`. The `inlet` / `node` /
 * `outlet` topology kinds are valid `GraphNodeKind`s, so the node-kind
 * passes through unchanged.
 */

import type { GraphInput } from '$lib/components/graph/types';
import type { TopologyProjection } from '$lib/protocol/topology.js';

/**
 * Maps a Protocol {@link TopologyProjection} onto the shared
 * {@link GraphInput}. A topology node → a graph node (kind passed
 * through); a topology edge → a graph edge with
 * `saturation = queue_depth / queue_capacity` clamped to [0,1].
 */
export function projectionToGraph(projection: TopologyProjection): GraphInput {
	return {
		nodes: projection.nodes.map((n) => ({
			id: n.name,
			label: n.name,
			kind: n.kind,
			meta: {}
		})),
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
