/**
 * Overview page — cost-rollup projection (Phase 73a / 108c — Phase 108c
 * reworked the axes to model | runtime).
 *
 * The cost-rollup card (page-overview.md §3 + §5 + §12) groups cost by
 * **model** (the inference endpoint) by default, or by **runtime/agent** (the
 * connected agent-registry name) as the secondary axis. A per-tenant breakdown
 * is deferred to multi-runtime support (D-091 / spec §10). Its data source is
 * the SHIPPED `llm.cost.recorded` event topic, aggregated CLIENT-SIDE off the
 * `events.subscribe` cursor — page-overview.md §5 tags this `[shipped]`; no new
 * Protocol method.
 *
 * This module is the PURE projection layer: it folds an `Event[]` slice
 * into per-key cost rows. No `$state`, no Protocol call — unit-testable.
 *
 * # Defensive payload parsing (CLAUDE.md §13 — fail loudly, never mis-read)
 *
 * `llm.cost.recorded` carries a typed runtime payload that crosses the
 * SSE wire as JSON. The runtime structs (`llm.CostRecordedPayload`,
 * `llm.Cost`) carry no explicit json tags, so the wire field names are
 * the Go field names. This module reads the payload defensively — an
 * event whose payload does not parse to a usable cost is SKIPPED (not
 * counted as zero), so a malformed event never silently deflates the
 * rollup.
 */

import type { Event } from '$lib/protocol/events.js';

/** The closed set of cost-rollup grouping axes (Phase 108c).
 *
 * `llm.cost.recorded` carries `Model` + the identity quadruple
 * (tenant/user/session/run) — verified live — but NO agent attribution, so a
 * per-agent breakdown keyed off the event has no source.
 *
 * - **`model`** (default): group by the inference endpoint (`openai/gpt-5.4`,
 *   …) — real, multi-row, and the dimension that answers rate-limit / routing
 *   questions.
 * - **`runtime`**: group all in-scope cost under the connected runtime's
 *   AGENT label (resolved from the agent registry, passed in by the caller).
 *   "Each runtime is an agent" — so in single-runtime V1 this is ONE row (the
 *   connected agent's spend); it fans out to one row per runtime once the
 *   multi-runtime "All Runtimes" mode lands (deferred, D-091 / spec §10).
 *
 * Genuine per-agent-WITHIN-a-runtime cost needs an `agent_id` on the cost
 * event — a tracked Runtime/Protocol follow-up. */
export type CostBreakdown = 'model' | 'runtime';

/** One row of the cost-rollup card. */
export interface CostRow {
	/** The grouping key — an agent label or a tenant id. */
	key: string;
	/** The summed cost in USD over the window. */
	costUSD: number;
	/** The count of `llm.cost.recorded` events folded into this row. */
	events: number;
}

/** The fully-projected cost rollup the card renders. */
export interface CostRollup {
	/** The per-key rows, descending by cost. */
	rows: CostRow[];
	/** The grand total across every row, USD. */
	totalUSD: number;
	/** The breakdown axis this rollup was grouped by. */
	breakdown: CostBreakdown;
}

/** Reads a number from a record under any of the candidate keys. */
function readNumber(obj: Record<string, unknown>, keys: string[]): number | null {
	for (const k of keys) {
		const v = obj[k];
		if (typeof v === 'number' && Number.isFinite(v)) {
			return v;
		}
	}
	return null;
}

/** Reads a string from a record under any of the candidate keys. */
function readString(obj: Record<string, unknown>, keys: string[]): string | null {
	for (const k of keys) {
		const v = obj[k];
		if (typeof v === 'string' && v.length > 0) {
			return v;
		}
	}
	return null;
}

/**
 * `extractCostUSD` pulls the total USD cost out of an
 * `llm.cost.recorded` event payload, or `null` when the payload does
 * not carry a parseable cost. The cost lives in a nested `Cost`
 * (`cost`) object's total field. Exported for the unit test.
 */
export function extractCostUSD(ev: Event): number | null {
	const payload = ev.payload;
	if (payload === null || typeof payload !== 'object') {
		return null;
	}
	const p = payload as Record<string, unknown>;
	const costObj = (p.Cost ?? p.cost) as Record<string, unknown> | undefined;
	if (costObj === undefined || costObj === null || typeof costObj !== 'object') {
		return null;
	}
	return readNumber(costObj, ['TotalCost', 'total_cost', 'totalCost']);
}

/**
 * `projectCost` folds the `events.subscribe` cursor into the cost
 * rollup. Only `llm.cost.recorded` events are considered; the grouping
 * key is the payload `Model` field for the `model` breakdown, or the
 * caller-supplied `runtimeLabel` (the agent-registry name) for the `runtime`
 * breakdown.
 *
 * An event whose payload does not yield a parseable cost is skipped —
 * it never deflates a row to a misleading zero (CLAUDE.md §13).
 */
export function projectCost(
	events: readonly Event[],
	breakdown: CostBreakdown,
	runtimeLabel = 'This runtime'
): CostRollup {
	const byKey = new Map<string, CostRow>();
	let totalUSD = 0;
	for (const ev of events) {
		if (ev.type !== 'llm.cost.recorded') {
			continue;
		}
		const costUSD = extractCostUSD(ev);
		if (costUSD === null) {
			continue;
		}
		let key: string;
		if (breakdown === 'runtime') {
			// All in-scope cost belongs to the one connected runtime/agent in
			// V1 (single-runtime); labelled with the agent-registry name.
			key = runtimeLabel;
		} else {
			const p = (ev.payload ?? {}) as Record<string, unknown>;
			key = readString(p, ['Model', 'model']) ?? 'unknown';
		}
		const row = byKey.get(key) ?? { key, costUSD: 0, events: 0 };
		row.costUSD += costUSD;
		row.events += 1;
		byKey.set(key, row);
		totalUSD += costUSD;
	}
	const rows = [...byKey.values()].sort((a, b) => b.costUSD - a.costUSD);
	return { rows, totalUSD, breakdown };
}
