/**
 * Overview page — cost-rollup projection (Phase 73a / D-127).
 *
 * The cost-rollup card (page-overview.md §3 + §5 + §12) renders a
 * per-agent cost breakdown by default; a per-tenant breakdown is the
 * admin elevation. Its data source is the SHIPPED `llm.cost.recorded`
 * event topic, aggregated CLIENT-SIDE off the `events.subscribe`
 * cursor — page-overview.md §5 tags this `[shipped]`; Phase 73a ships
 * NO new Protocol method for it.
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

import type { Event } from '$lib/protocol/harbor.js';

/** The closed set of cost-rollup grouping axes. */
export type CostBreakdown = 'agent' | 'tenant';

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
 * key is the event's agent label (from the payload `Model` field as a
 * proxy when no agent label is present) for the `agent` breakdown, or
 * the event's `tenant` for the `tenant` breakdown.
 *
 * An event whose payload does not yield a parseable cost is skipped —
 * it never deflates a row to a misleading zero (CLAUDE.md §13).
 */
export function projectCost(events: readonly Event[], breakdown: CostBreakdown): CostRollup {
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
		if (breakdown === 'tenant') {
			key = ev.tenant !== '' ? ev.tenant : 'unknown';
		} else {
			const p = (ev.payload ?? {}) as Record<string, unknown>;
			key = readString(p, ['Model', 'model', 'agent', 'Agent']) ?? 'unknown';
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
