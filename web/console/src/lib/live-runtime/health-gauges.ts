// Harbor Console — runtime-gauge projection for the Live Runtime Health
// panel (Phase 121). Pure logic, unit-tested; the .svelte component only
// renders the result (matching the derive.ts convention).
//
// The runtime observability gauges (Phase 120) reach the Console through
// the SHIPPED metrics.snapshot. We surface ONLY the `harbor_runtime_*`
// family — the bounded, unlabelled internal-size gauges (active runs,
// engine capacity-map entries, governance cache entries, bus messages
// dropped). Raw Go/process collectors (go_goroutines, heap, GC) stay on
// the operator /metrics scrape, OFF the Protocol — so they never appear
// here, by design.

import type { MetricsSnapshot } from '$lib/protocol/posture.js';

/** The metric-name prefix marking a Harbor runtime observability gauge. */
export const RUNTIME_GAUGE_PREFIX = 'harbor_runtime_';

/** One rendered gauge row: the raw metric name (a stable, unique render
 * key — humanized labels could collide if a future gauge in the family is
 * labelled), a humanized label, and its current value. */
export interface RuntimeGaugeRow {
	name: string;
	label: string;
	value: number;
}

/**
 * Humanize a `harbor_runtime_*` metric name into a Title-case label
 * (e.g. `harbor_runtime_active_runs` → `Active runs`).
 */
export function humanizeGauge(name: string): string {
	const bare = name.slice(RUNTIME_GAUGE_PREFIX.length).replace(/_/g, ' ');
	return bare.length === 0 ? bare : bare.charAt(0).toUpperCase() + bare.slice(1);
}

/**
 * Project the runtime observability gauges out of a metrics.snapshot,
 * sorted by label for stable rendering. A null/absent snapshot — or one
 * carrying no `harbor_runtime_*` gauge — yields an empty list (the panel
 * then omits the gauges section; no fabrication).
 */
export function runtimeGaugesFrom(metrics: MetricsSnapshot | null): RuntimeGaugeRow[] {
	return (metrics?.gauges ?? [])
		.filter((g) => g.name.startsWith(RUNTIME_GAUGE_PREFIX))
		.map((g) => ({ name: g.name, label: humanizeGauge(g.name), value: g.value }))
		.sort((a, b) => a.label.localeCompare(b.label));
}

/** Format a gauge value for display; a non-finite reading renders as an em dash. */
export function formatGaugeValue(v: number): string {
	return Number.isFinite(v) ? v.toLocaleString() : '—';
}
