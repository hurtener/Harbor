// Harbor Console — Artifacts page pure projections (Phase 108o / D-187).
//
// The king-file refactor pulls every pure, side-effect-free projection the
// Artifacts page renders into this unit-testable module — the same split the
// Tools-108k / Agents-108l / MCP-108m / Memory-108n rebuilds used. Nothing
// here touches the Protocol client, `localStorage`, the DOM, or any reactive
// state; the `ArtifactsPageState` controller and the views import these
// helpers and `derive.test.ts` pins them against the real wire shapes.
//
// No Svelte runes here (a plain `.ts` module); design-token concerns live in
// the `.svelte` views, not in these projections.

import type { StatusKind } from '$lib/components/ui/StatusChip.svelte';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { ArtifactSource } from '$lib/protocol/artifacts.js';

/** A page-friendly `{code, message}` projection of a thrown error. */
export type PageError = { code: string; message: string };

/**
 * Normalises any thrown value into a `{code, message}` the four-state
 * `<PageState>` boundary renders. A `ProtocolError` keeps its wire code
 * (`not_found` / `scope_mismatch` / …); anything else collapses to
 * `runtime_error` — never a silent swallow (CLAUDE.md §13).
 */
export function toPageError(err: unknown): PageError {
  if (err instanceof ProtocolError) {
    return { code: err.code, message: err.message };
  }
  return {
    code: 'runtime_error',
    message: err instanceof Error ? err.message : 'unknown error'
  };
}

/**
 * The status the list `<PageState>` boundary renders. `loading` / `error` /
 * `disconnected` pass through; the ready/empty split is derived LIVE from the
 * loaded-row count so a store whose rows all dropped under a tight facet still
 * reads "empty" (the Events-page D-180 lesson).
 */
export function displayStatus(status: PageStatus, rowCount: number): PageStatus {
  if (status === 'loading' || status === 'error' || status === 'disconnected') {
    return status;
  }
  return rowCount > 0 ? 'ready' : 'empty';
}

/** Human-readable byte size — B / KB / MB. */
export function fmtSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** Maps an artifact source onto the shared StatusChip kind scale. */
export function sourceKind(source: ArtifactSource | undefined): StatusKind {
  if (source === 'user_upload') return 'accent';
  if (source === 'system') return 'warning';
  if (source === 'tool') return 'success';
  return 'neutral';
}

/**
 * A short relative "created" label. The Runtime returns the Go zero time
 * (`0001-01-01T00:00:00Z`) for an artifact with no recorded timestamp — that
 * renders an honest `—`, never a literal year-1 date (CLAUDE.md §13).
 */
export function relativeTime(iso: string | undefined, now: number = Date.now()): string {
  if (!iso || iso.startsWith('0001-01-01')) return '—';
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return '—';
  const deltaSec = Math.round((now - then) / 1000);
  if (deltaSec < 60) return 'just now';
  const deltaMin = Math.round(deltaSec / 60);
  if (deltaMin < 60) return `${deltaMin}m ago`;
  const deltaHr = Math.round(deltaMin / 60);
  if (deltaHr < 24) return `${deltaHr}h ago`;
  return `${Math.round(deltaHr / 24)}d ago`;
}

/**
 * A coarse renderable-family classification of a mime type, for the preview
 * pane's "can the canonical registry render this inline?" hint. NOT a renderer
 * itself (the registry at `$lib/chat/renderers` owns rendering) — just a hint
 * the rail uses to pick between an inline preview and the Download fallback.
 */
export function previewFamily(mime: string | undefined): 'image' | 'text' | 'pdf' | 'audio' | 'other' {
  if (!mime) return 'other';
  if (mime.startsWith('image/')) return 'image';
  if (mime.startsWith('audio/')) return 'audio';
  if (mime === 'application/pdf') return 'pdf';
  if (mime.startsWith('text/') || mime === 'application/json') return 'text';
  return 'other';
}
