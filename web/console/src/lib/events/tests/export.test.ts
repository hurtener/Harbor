/**
 * Events page — `export.ts` unit tests (Phase 73g / D-125).
 *
 * Pins: NDJSON / CSV serialisation of a known event log; RFC-4180 cell
 * quoting; and — the D-026 gate — that a truncated-payload event's
 * `artifact_ref` is preserved as a reference, with NO heavy payload
 * bytes inlined into either format.
 */
import { describe, expect, it } from 'vitest';
import { exportEventsCSV, exportEventsNDJSON, exportMeta } from '../export.js';
import type { Event, EventArtifactRef } from '../../protocol/events.js';

function event(overrides: Partial<Event> = {}): Event {
	return {
		type: 'tool.completed',
		sequence: 1,
		occurred_at: '2026-05-20T12:00:00.000Z',
		tenant: 'tenant-a',
		user: 'user-a',
		session: 'sess-a',
		payload: { ok: true },
		extra: { source: 'tools/inproc', severity: 'info' },
		...overrides
	};
}

describe('events export: NDJSON', () => {
	it('emits one JSON object per line', () => {
		const out = exportEventsNDJSON([event({ sequence: 1 }), event({ sequence: 2 })]);
		const lines = out.split('\n');
		expect(lines).toHaveLength(2);
		expect(JSON.parse(lines[0]).sequence).toBe(1);
		expect(JSON.parse(lines[1]).sequence).toBe(2);
	});

	it('preserves a truncated-payload artifact_ref — never inlines bytes', () => {
		const heavy: EventArtifactRef = {
			artifact_ref: { id: 'art-9', mime: 'application/octet-stream', size: 9_000_000 }
		};
		const out = exportEventsNDJSON([event({ payload: heavy })]);
		const parsed = JSON.parse(out) as Event;
		// The artifact_ref reference is preserved.
		expect((parsed.payload as EventArtifactRef).artifact_ref.id).toBe('art-9');
		// No inline bytes — the reference shape carries only id/mime/size.
		expect(out).not.toContain('bytes_base64');
		expect(out).not.toContain('"data"');
	});
});

describe('events export: CSV', () => {
	it('emits a header row plus one row per event', () => {
		const out = exportEventsCSV([event({ sequence: 1 }), event({ sequence: 2 })]);
		const rows = out.split('\r\n');
		expect(rows[0]).toContain('occurred_at');
		expect(rows[0]).toContain('payload');
		expect(rows).toHaveLength(3);
	});

	it('RFC-4180-quotes cells containing commas / quotes', () => {
		const out = exportEventsCSV([
			event({ extra: { source: 'tools,mcp', severity: 'warn' } })
		]);
		expect(out).toContain('"tools,mcp"');
	});

	it('a truncated-payload row carries the artifact_ref, never bytes', () => {
		const heavy: EventArtifactRef = {
			artifact_ref: { id: 'art-7', mime: 'image/png', size: 12_000_000 }
		};
		const out = exportEventsCSV([event({ payload: heavy })]);
		expect(out).toContain('art-7');
		expect(out).not.toContain('bytes_base64');
	});
});

describe('events export: exportMeta', () => {
	it('maps ndjson and csv onto their MIME + extension', () => {
		expect(exportMeta('ndjson')).toEqual({ mime: 'application/x-ndjson', ext: 'ndjson' });
		expect(exportMeta('csv')).toEqual({ mime: 'text/csv', ext: 'csv' });
	});
});
