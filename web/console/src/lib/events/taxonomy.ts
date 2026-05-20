/**
 * Events page — the canonical event-type taxonomy (Phase 73g / D-125).
 *
 * The Events page's `Event type ▾` facet renders the canonical event
 * registry, chip-grouped by source subsystem (page-events.md §4 row 2).
 * The authoritative registry is the Go side
 * (`internal/events/events.go::EventTypes`); page-events.md §3
 * enumerates the V1 set. This module mirrors that set so the page's
 * type-multiselect is exhaustive without a Protocol round-trip — a
 * pure, static projection (the Go side gains a type by declaring it; a
 * future generator step folds this list into the generated surface).
 *
 * The `category` field drives the colour-coded category tag the event
 * table renders next to each event name (page-events.md §12).
 */

/** A source-subsystem grouping for the type-multiselect chips. */
export type EventCategory =
	| 'runtime'
	| 'bus'
	| 'audit'
	| 'governance'
	| 'task'
	| 'tool'
	| 'mcp'
	| 'llm'
	| 'memory'
	| 'distributed'
	| 'planner'
	| 'trajectory'
	| 'pause'
	| 'agent'
	| 'control'
	| 'flow'
	| 'session'
	| 'skill'
	| 'auth'
	| 'dev'
	| 'topology';

/** Derives the category of a dotted event-type name (the prefix). */
export function categoryOf(type: string): EventCategory {
	const prefix = type.split('.')[0];
	return (prefix as EventCategory) ?? 'runtime';
}

/**
 * The V1 canonical event-type registry — mirrors page-events.md §3.
 * Grouped lazily by `categoryOf` at render time.
 */
export const EVENT_TYPES: readonly string[] = [
	'runtime.error',
	'runtime.warning',
	'runtime.run_cancelled',
	'bus.dropped',
	'bus.subscription_idle_closed',
	'audit.redaction_failed',
	'audit.admin_scope_used',
	'governance.budget_exceeded',
	'governance.rate_limited',
	'governance.maxtokens_exceeded',
	'task.spawned',
	'task.started',
	'task.paused',
	'task.resumed',
	'task.completed',
	'task.failed',
	'task.cancelled',
	'task.prioritised',
	'task.group_created',
	'task.group_sealed',
	'task.group_resolved',
	'task.group_cancelled',
	'task.patch_applied',
	'task.patch_rejected',
	'task.background_acknowledged',
	'tool.invoked',
	'tool.completed',
	'tool.failed',
	'tool.invalid_args',
	'tool.policy_exhausted',
	'tool.auth_required',
	'tool.auth_completed',
	'tool.approval_requested',
	'tool.approved',
	'tool.rejected',
	'mcp.resource_updated',
	'mcp.raw_html_trust_toggled',
	'llm.image.materialized',
	'llm.context_leak',
	'llm.context_window_exceeded',
	'llm.cost.recorded',
	'llm.mode_downgraded',
	'llm.retry_with_feedback',
	'memory.identity_rejected',
	'memory.health_changed',
	'memory.recovery_dropped',
	'distributed.bus_envelope',
	'planner.decision',
	'planner.finish',
	'planner.error',
	'planner.repair_exhausted',
	'planner.max_steps_exceeded',
	'trajectory.compressed',
	'trajectory.compression_failed',
	'pause.requested',
	'pause.resumed',
	'agent.registered',
	'agent.restarted',
	'agent.health',
	'agent.drained',
	'agent.deregistered',
	'agent.paused',
	'agent.restart_requested',
	'agent.force_stopped',
	'control.received',
	'control.applied',
	'control.rejected',
	'flow.budget_exceeded',
	'session.opened',
	'session.touched',
	'session.closed',
	'session.gc_reaped',
	'skill.upserted',
	'skill.deleted',
	'skill.pack_overwrite_refused',
	'skill.search_executed',
	'skill.identity_rejected',
	'skill.proposed',
	'auth.rejected',
	'topology.changed',
	'dev.draft.created',
	'dev.draft.updated',
	'dev.draft.previewed',
	'dev.draft.saved',
	'dev.draft.discarded',
	'dev.hot_reload.triggered',
	'dev.hot_reload.completed'
];

/** Groups the canonical registry by source subsystem, sorted within group. */
export function eventTypesByCategory(): { category: EventCategory; types: string[] }[] {
	const groups = new Map<EventCategory, string[]>();
	for (const t of EVENT_TYPES) {
		const cat = categoryOf(t);
		const list = groups.get(cat) ?? [];
		list.push(t);
		groups.set(cat, list);
	}
	return [...groups.entries()]
		.map(([category, types]) => ({ category, types: [...types].sort() }))
		.sort((a, b) => a.category.localeCompare(b.category));
}

/** The `StatusChip` `kind` token a category maps onto for the table tag. */
export function categoryKind(
	category: EventCategory
): 'success' | 'warning' | 'danger' | 'accent' | 'neutral' {
	switch (category) {
		case 'audit':
		case 'governance':
			return 'warning';
		case 'runtime':
			return 'danger';
		case 'planner':
		case 'agent':
		case 'task':
			return 'accent';
		default:
			return 'neutral';
	}
}
