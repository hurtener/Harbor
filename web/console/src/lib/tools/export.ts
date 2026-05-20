/**
 * Tools-page Export helper (Phase 73f / D-116).
 *
 * The Tools-page `Export ▾` control writes the currently-filtered
 * catalog rows to a CSV / JSON file. This is a CONSOLE-LOCAL action
 * (D-061): it serialises rows the Console already loaded via
 * `tools.list`; it never mutates a runtime entity and never round-trips
 * to the Protocol.
 */

import type { Tool } from '../protocol/tools.js';

/** The column order the CSV export emits — mirrors the catalog table. */
const CSV_COLUMNS: { header: string; pick: (t: Tool) => string }[] = [
  { header: 'id', pick: (t) => t.id },
  { header: 'name', pick: (t) => t.name },
  { header: 'version', pick: (t) => t.version },
  { header: 'scope', pick: (t) => t.scope },
  { header: 'transport', pick: (t) => t.transport },
  { header: 'oauth_status', pick: (t) => t.oauth_status },
  { header: 'approval_policy', pick: (t) => t.approval_policy },
  { header: 'reliability_tier', pick: (t) => t.reliability_tier },
  { header: 'owner', pick: (t) => t.owner },
  { header: 'last_used_at', pick: (t) => t.last_used_at }
];

/** RFC-4180-style CSV cell quoting. */
function csvCell(value: string): string {
  if (/[",\r\n]/.test(value)) {
    return `"${value.replace(/"/g, '""')}"`;
  }
  return value;
}

/** Serialises the catalog rows to a CSV document. */
export function exportToolsCSV(tools: Tool[]): string {
  const header = CSV_COLUMNS.map((c) => c.header).join(',');
  const rows = tools.map((t) =>
    CSV_COLUMNS.map((c) => csvCell(c.pick(t))).join(',')
  );
  return [header, ...rows].join('\r\n');
}

/** Serialises the catalog rows to a pretty-printed JSON document. */
export function exportToolsJSON(tools: Tool[]): string {
  return JSON.stringify(tools, null, 2);
}

/**
 * Triggers a browser download of `content` as `filename`. A no-op when
 * not running in a browser (SSR-safe — though the Console is SPA-only).
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
