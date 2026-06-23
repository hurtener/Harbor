// Harbor Console — runtime-gauge projection unit tests (Phase 121).
//
// Pins the Health-panel gauge projection: only the harbor_runtime_*
// family is surfaced (raw Go/process collectors are off the Protocol),
// names are humanized, rows are label-sorted, and a null/absent snapshot
// yields [] (the panel then omits the section — no fabrication, §13).

import { describe, it, expect } from 'vitest';
import {
	runtimeGaugesFrom,
	humanizeGauge,
	formatGaugeValue,
	RUNTIME_GAUGE_PREFIX
} from '../health-gauges.js';
import type { MetricsSnapshot } from '$lib/protocol/posture.js';

function snap(gauges: { name: string; value: number }[]): MetricsSnapshot {
	return { counters: [], histograms: [], gauges, snapshot_at: 0 };
}

describe('runtimeGaugesFrom', () => {
	it('returns [] for a null snapshot', () => {
		expect(runtimeGaugesFrom(null)).toEqual([]);
	});

	it('returns [] when no harbor_runtime_* gauge is present', () => {
		expect(runtimeGaugesFrom(snap([{ name: 'go_goroutines', value: 42 }]))).toEqual([]);
	});

	it('surfaces ONLY the harbor_runtime_* family (Go/process collectors stay off-Protocol)', () => {
		const rows = runtimeGaugesFrom(
			snap([
				{ name: 'go_goroutines', value: 1026 },
				{ name: 'go_memstats_alloc_bytes', value: 999 },
				{ name: 'harbor_runtime_events_dropped', value: 3 }
			])
		);
		expect(rows).toEqual([
			{ name: 'harbor_runtime_events_dropped', label: 'Events dropped', value: 3 }
		]);
	});

	it('humanizes names and sorts rows by label', () => {
		const rows = runtimeGaugesFrom(
			snap([
				{ name: 'harbor_runtime_governance_cache_entries', value: 7 },
				{ name: 'harbor_runtime_active_runs', value: 2 },
				{ name: 'harbor_runtime_events_dropped', value: 0 }
			])
		);
		expect(rows).toEqual([
			{ name: 'harbor_runtime_active_runs', label: 'Active runs', value: 2 },
			{ name: 'harbor_runtime_events_dropped', label: 'Events dropped', value: 0 },
			{ name: 'harbor_runtime_governance_cache_entries', label: 'Governance cache entries', value: 7 }
		]);
	});

	it('carries the raw metric name as a unique render key (collision-proof vs humanized labels)', () => {
		const rows = runtimeGaugesFrom(
			snap([
				{ name: 'harbor_runtime_active_runs', value: 1 },
				{ name: 'harbor_runtime_events_dropped', value: 2 }
			])
		);
		const keys = rows.map((r) => r.name);
		expect(new Set(keys).size).toBe(keys.length); // all keys distinct
	});
});

describe('humanizeGauge', () => {
	it('strips the prefix, de-snakes, and Title-cases', () => {
		expect(humanizeGauge(`${RUNTIME_GAUGE_PREFIX}engine_capacity_entries`)).toBe(
			'Engine capacity entries'
		);
	});
});

describe('formatGaugeValue', () => {
	it('formats finite numbers with locale grouping', () => {
		expect(formatGaugeValue(1234)).toBe((1234).toLocaleString());
	});
	it('renders a non-finite reading as an em dash', () => {
		expect(formatGaugeValue(Number.NaN)).toBe('—');
	});
});
