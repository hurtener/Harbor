/**
 * Unit tests for the Live Runtime cockpit panel registry (Phase 108e / D-177).
 *
 * `resolvePanels` is a pure function of the advertised capability set: the
 * spine is always present; topology is gated on `topology_snapshot`; unknown
 * capabilities are ignored without crashing; ordering is stable.
 */
import { describe, it, expect } from 'vitest';
import {
	resolvePanels,
	COCKPIT_CAPABILITY_KEYS,
	CAP_TOPOLOGY_SNAPSHOT,
	CAP_RUNTIME_HEALTH,
	CAP_GOVERNANCE_POSTURE,
	CAP_METRICS_SNAPSHOT,
	type CockpitPanel
} from '../panels.js';

const SPINE_IDS = [
	'posture',
	'counters',
	'needs-attention',
	'live-events',
	'active-sessions',
	'health',
	'cost'
];

function ids(panels: CockpitPanel[]): string[] {
	return panels.map((p) => p.id);
}

describe('resolvePanels', () => {
	it('returns the full spine on a planner/RunLoop runtime (no gated caps)', () => {
		const panels = resolvePanels(new Set());
		// Every spine panel is present.
		for (const id of SPINE_IDS) {
			expect(ids(panels)).toContain(id);
		}
		// Topology is absent without the capability.
		expect(ids(panels)).not.toContain('topology');
		// Every returned spine panel declares capability null.
		for (const p of panels) {
			expect(p.spine).toBe(true);
			expect(p.capability).toBeNull();
		}
	});

	it('includes topology iff topology_snapshot is advertised', () => {
		const without = resolvePanels(new Set([CAP_RUNTIME_HEALTH]));
		expect(ids(without)).not.toContain('topology');

		const withTopo = resolvePanels(new Set([CAP_TOPOLOGY_SNAPSHOT]));
		expect(ids(withTopo)).toContain('topology');
		const topo = withTopo.find((p) => p.id === 'topology');
		expect(topo?.spine).toBe(false);
		expect(topo?.capability).toBe(CAP_TOPOLOGY_SNAPSHOT);
	});

	it('ignores unknown advertised capabilities (no crash, spine intact)', () => {
		const panels = resolvePanels(new Set(['some_future_capability', 'another_one']));
		for (const id of SPINE_IDS) {
			expect(ids(panels)).toContain(id);
		}
		// No phantom panel from the unknown capabilities.
		expect(panels.length).toBe(SPINE_IDS.length);
	});

	it('renders an engine+posture runtime with spine + topology', () => {
		const panels = resolvePanels(
			new Set([CAP_TOPOLOGY_SNAPSHOT, CAP_RUNTIME_HEALTH, CAP_GOVERNANCE_POSTURE])
		);
		for (const id of [...SPINE_IDS, 'topology']) {
			expect(ids(panels)).toContain(id);
		}
	});

	it('preserves a stable ordering (catalog order)', () => {
		const a = ids(resolvePanels(new Set([CAP_TOPOLOGY_SNAPSHOT])));
		const b = ids(resolvePanels(new Set([CAP_TOPOLOGY_SNAPSHOT])));
		expect(a).toEqual(b);
		// posture (header) leads. Topology is the gated LEFT-column HERO: when
		// advertised it renders AHEAD of the left spine (needs-attention /
		// live-events) so it leads the column on an engine runtime.
		expect(a[0]).toBe('posture');
		expect(a.indexOf('topology')).toBeLessThan(a.indexOf('needs-attention'));
		expect(a.indexOf('topology')).toBeLessThan(a.indexOf('live-events'));
	});

	it('declares the documented capability vocabulary', () => {
		expect(COCKPIT_CAPABILITY_KEYS).toContain(CAP_TOPOLOGY_SNAPSHOT);
		expect(COCKPIT_CAPABILITY_KEYS).toContain(CAP_RUNTIME_HEALTH);
		expect(COCKPIT_CAPABILITY_KEYS).toContain(CAP_GOVERNANCE_POSTURE);
		expect(COCKPIT_CAPABILITY_KEYS).toContain(CAP_METRICS_SNAPSHOT);
	});
});
