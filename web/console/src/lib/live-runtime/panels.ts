/**
 * Live Runtime cockpit — the capability → panel registry (Phase 108e / D-177).
 *
 * The Live Runtime page is reframed from a topology-first tabbed view into the
 * single-runtime **operations cockpit** (the Overview → runtime drill-down).
 * Its composition is a PURE function of the runtime's advertised capability set
 * (`runtime.info.capabilities`, Phase 84a): an always-present **spine** of
 * operational panels plus **capability-gated** panels that light up only when
 * the selected runtime advertises them.
 *
 * This module is the declarative registry + the pure `resolvePanels` resolver.
 * Adding a new runtime shape adds a panel entry here — never a page rewrite.
 * No `$state`, no DOM, no Protocol call: unit-tested in `tests/panels.test.ts`.
 *
 * # §4.3 deviation note — health & cost are spine self-probing, not hard-gated
 *
 * The capability map in the phase plan lists Health behind `runtime_health`
 * and Cost behind `governance_posture` / `llm.cost.recorded`. In practice
 * `runtime.health` and `runtime.counters` are SHIPPED (Phase 72f / D-111) and
 * work on every runtime WITHOUT a capability flag — the Overview page proves
 * it (it calls `client.runtime.health()` / `client.runtime.counters()`
 * unconditionally). Hard-gating Health/Cost behind a capability the dev
 * runtime does not advertise would therefore HIDE working panels and leave the
 * cockpit emptier than the data justifies. So Health and Cost are spine panels
 * (`capability: null`) that SELF-PROBE their surface and render an honest
 * "not available on this runtime" state on a throw / `unknown_method` — the
 * robust Overview pattern (CLAUDE.md §13, no fabrication). The capability keys
 * below stay declared as the documented vocabulary (used for the posture
 * header's advertised-capability chips and for the genuinely-gated Topology
 * panel), so the capability surface stays guarded even though Health/Cost do
 * not hard-gate on them.
 */

/**
 * The documented capability-key vocabulary the cockpit keys off (the advertised
 * `runtime.info.capabilities` strings). `TOPOLOGY_SNAPSHOT` is the only one that
 * HARD-GATES a panel today (the engine-graph topology view); the others are the
 * forward-looking keys the posture header renders as advertised chips and that
 * future gated panels will consume.
 */
export const CAP_TOPOLOGY_SNAPSHOT = 'topology_snapshot';
export const CAP_RUNTIME_HEALTH = 'runtime_health';
export const CAP_GOVERNANCE_POSTURE = 'governance_posture';
export const CAP_METRICS_SNAPSHOT = 'metrics_snapshot';

/**
 * The full documented capability vocabulary the Console expects in
 * `runtime.info.capabilities`. Stable, guarded by the phase-108e smoke; a
 * future generated capability enum (D-093-style) would close the drift risk
 * between the Go-advertised keys and these.
 */
export const COCKPIT_CAPABILITY_KEYS: readonly string[] = [
	CAP_TOPOLOGY_SNAPSHOT,
	CAP_RUNTIME_HEALTH,
	CAP_GOVERNANCE_POSTURE,
	CAP_METRICS_SNAPSHOT
];

/** The grid region a cockpit panel renders into. */
export type CockpitColumn = 'header' | 'strip' | 'left' | 'right';

/** One declarative cockpit-panel entry. */
export interface CockpitPanel {
	/** Stable key — also a `data-testid` anchor on the rendered panel. */
	id: string;
	/** The human-readable panel title (uppercase panel-title rendering). */
	title: string;
	/**
	 * The capability that gates the panel, or `null` for a spine panel that
	 * always renders (spine panels self-probe their surface and degrade
	 * honestly when it is absent — the Overview pattern).
	 */
	capability: string | null;
	/** True for the always-present spine; false for capability-gated panels. */
	spine: boolean;
	/** The grid region the panel renders into. */
	column: CockpitColumn;
}

/**
 * The full ordered panel catalog. Spine panels (capability `null`) render on
 * every runtime; gated panels render only when their capability is advertised.
 * Order is the render order within each column.
 */
const PANELS: readonly CockpitPanel[] = [
	// --- spine (always present) ---
	{ id: 'posture', title: 'Runtime posture', capability: null, spine: true, column: 'header' },
	{ id: 'counters', title: 'Activity', capability: null, spine: true, column: 'strip' },
	// --- capability-gated: Topology is the LEFT-column HERO when the runtime
	//     advertises the engine graph (the operator's centerpiece on an engine
	//     runtime). It is FIRST in the left spine so it leads the column; it is
	//     filtered out entirely on a planner/RunLoop runtime, leaving the left
	//     spine as just Needs attention + Live events (the common case). ---
	{
		id: 'topology',
		title: 'Topology',
		capability: CAP_TOPOLOGY_SNAPSHOT,
		spine: false,
		column: 'left'
	},
	{ id: 'needs-attention', title: 'Needs attention', capability: null, spine: true, column: 'left' },
	{ id: 'live-events', title: 'Live events', capability: null, spine: true, column: 'left' },
	{ id: 'active-sessions', title: 'Active sessions', capability: null, spine: true, column: 'right' },
	{ id: 'health', title: 'Health', capability: null, spine: true, column: 'right' },
	{ id: 'cost', title: 'Cost', capability: null, spine: true, column: 'right' }
];

/**
 * Pure: the ordered panels to render for an advertised capability set. Spine
 * panels are always included; a gated panel is included only when its
 * capability is in `capabilities`. An unknown advertised capability that maps
 * to no registered panel is ignored (no crash). Ordering is stable (the
 * catalog order).
 */
export function resolvePanels(capabilities: ReadonlySet<string>): CockpitPanel[] {
	return PANELS.filter(
		(panel) => panel.capability === null || capabilities.has(panel.capability)
	);
}
