/**
 * Events page — `taxonomy.ts` unit tests (Phase 73g / D-125).
 *
 * Pins: `categoryOf` derives the source prefix; `eventTypesByCategory`
 * groups exhaustively without dropping a type; `categoryKind` maps onto
 * the closed `StatusChip` kind set.
 */
import { describe, expect, it } from 'vitest';
import {
	EVENT_TYPES,
	categoryKind,
	categoryOf,
	eventTypesByCategory
} from '../taxonomy.js';

describe('events taxonomy: categoryOf', () => {
	it('derives the dotted-name prefix as the category', () => {
		expect(categoryOf('tool.failed')).toBe('tool');
		expect(categoryOf('planner.repair_exhausted')).toBe('planner');
		expect(categoryOf('audit.admin_scope_used')).toBe('audit');
		expect(categoryOf('dev.draft.created')).toBe('dev');
	});
});

describe('events taxonomy: eventTypesByCategory', () => {
	it('groups every canonical type without dropping any', () => {
		const grouped = eventTypesByCategory();
		const flattened = grouped.flatMap((g) => g.types);
		expect(flattened.length).toBe(EVENT_TYPES.length);
		expect(new Set(flattened)).toEqual(new Set(EVENT_TYPES));
	});

	it('types within a group are sorted', () => {
		for (const group of eventTypesByCategory()) {
			const sorted = [...group.types].sort();
			expect(group.types).toEqual(sorted);
		}
	});

	it('contains the bus + audit categories the Events page facets surface', () => {
		const categories = eventTypesByCategory().map((g) => g.category);
		expect(categories).toContain('bus');
		expect(categories).toContain('audit');
		expect(categories).toContain('tool');
	});
});

describe('events taxonomy: categoryKind', () => {
	it('maps every category onto a closed StatusChip kind', () => {
		const valid = new Set(['success', 'warning', 'danger', 'accent', 'neutral']);
		for (const group of eventTypesByCategory()) {
			expect(valid.has(categoryKind(group.category))).toBe(true);
		}
	});

	it('audit + governance are warning-coded', () => {
		expect(categoryKind('audit')).toBe('warning');
		expect(categoryKind('governance')).toBe('warning');
	});
});
