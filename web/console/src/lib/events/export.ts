/**
 * Events page — pure NDJSON / CSV export serialisers
 * (Phase 73g / D-125).
 *
 * The Events page `Export ▾` control writes the currently-filtered,
 * already-loaded page of events to an NDJSON (default) or CSV file.
 * This is a CONSOLE-LOCAL action (D-061): it serialises events the page
 * already received over the `events.subscribe` SSE stream; it never
 * mutates a runtime entity and never round-trips to the Protocol.
 *
 * # Heavy payloads are NEVER inlined (D-026)
 *
 * A truncated-payload event carries an `artifact_ref` rather than
 * inline bytes (RFC §6.5 / D-026). The serialisers emit the
 * `artifact_ref` object as-is — they NEVER dereference it or inline
 * artifact bytes. A CSV `payload` cell for a truncated event is the
 * JSON-encoded `artifact_ref` shape; the operator opens the artifact
 * via the page's `Open artifact` link. The unit test asserts no heavy
 * bytes leak into either format.
 */

import type { Event } from '$lib/protocol/events.js';

/** The export format the `Export ▾` menu offers. */
export type ExportFormat = 'ndjson' | 'csv';

/**
 * Serialises the loaded events to NDJSON — one JSON object per line,
 * newest-first in the order the page holds them. The `payload` field
 * is emitted verbatim: a truncated-payload event's `artifact_ref` is
 * preserved as a reference, never expanded to bytes (D-026).
 */
export function exportEventsNDJSON(events: Event[]): string {
	return events.map((e) => JSON.stringify(e)).join('\n');
}

/** The CSV column order — mirrors the on-page table (page-events.md §12). */
const CSV_COLUMNS: { header: string; pick: (e: Event) => string }[] = [
	{ header: 'occurred_at', pick: (e) => e.occurred_at },
	{ header: 'sequence', pick: (e) => String(e.sequence) },
	{ header: 'type', pick: (e) => e.type },
	{ header: 'tenant', pick: (e) => e.tenant },
	{ header: 'user', pick: (e) => e.user },
	{ header: 'session', pick: (e) => e.session },
	{ header: 'run', pick: (e) => e.run ?? '' },
	{ header: 'source', pick: (e) => e.extra?.source ?? '' },
	{ header: 'severity', pick: (e) => e.extra?.severity ?? '' },
	// `payload` carries the JSON-encoded payload OR, for a truncated
	// event, the `artifact_ref` object — never inline heavy bytes (D-026).
	{ header: 'payload', pick: (e) => (e.payload === undefined ? '' : JSON.stringify(e.payload)) }
];

/** RFC-4180-style CSV cell quoting. */
function csvCell(value: string): string {
	if (/[",\r\n]/.test(value)) {
		return `"${value.replace(/"/g, '""')}"`;
	}
	return value;
}

/** Serialises the loaded events to an RFC-4180 CSV document. */
export function exportEventsCSV(events: Event[]): string {
	const header = CSV_COLUMNS.map((c) => c.header).join(',');
	const rows = events.map((e) => CSV_COLUMNS.map((c) => csvCell(c.pick(e))).join(','));
	return [header, ...rows].join('\r\n');
}

/** The MIME type + file extension for an export format. */
export function exportMeta(format: ExportFormat): { mime: string; ext: string } {
	return format === 'csv'
		? { mime: 'text/csv', ext: 'csv' }
		: { mime: 'application/x-ndjson', ext: 'ndjson' };
}

/**
 * Triggers a browser download of `content` as `filename`. A no-op when
 * not running in a browser (SSR-safe — the Console is SPA-only, but the
 * guard keeps the module test-importable under jsdom-less Vitest).
 */
export function triggerDownload(filename: string, mime: string, content: string): void {
	if (typeof document === 'undefined' || typeof URL.createObjectURL !== 'function') {
		return;
	}
	const blob = new Blob([content], { type: mime });
	const url = URL.createObjectURL(blob);
	const anchor = document.createElement('a');
	anchor.href = url;
	anchor.download = filename;
	document.body.appendChild(anchor);
	anchor.click();
	document.body.removeChild(anchor);
	URL.revokeObjectURL(url);
}
