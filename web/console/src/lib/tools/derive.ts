// Harbor Console — Tools page pure projections (Phase 108k / D-183).
//
// The king-file refactor pulls every pure, side-effect-free projection
// the Tools page renders into this unit-testable module — the same split
// the Background-Jobs-108j rebuild used (`background-jobs/derive.ts`).
// Nothing here touches the Protocol client, `localStorage`, the DOM, or
// any reactive state; the `ToolsPageState` controller and the view import
// these helpers and the `derive.test.ts` suite pins them against the real
// wire shapes the live probe captured.
//
// No Svelte runes here (a plain `.ts` module); design-token concerns live
// in the `.svelte` views, not in these projections.

import type { StatusKind } from '$lib/components/ui/StatusChip.svelte';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import { ProtocolError } from '$lib/protocol/errors.js';
import type {
  ToolOAuthStatus,
  ToolApprovalPolicy,
  ToolStatus
} from '$lib/protocol/tools.js';

/** A page-friendly `{code, message}` projection of a thrown error. */
export type PageError = { code: string; message: string };

/**
 * Normalises any thrown value into a `{code, message}` the four-state
 * `<PageState>` boundary renders. A `ProtocolError` keeps its wire code
 * (`not_found` / `identity_required` / …); anything else collapses to
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
 * The relative "last used" label for a catalog row. The Runtime returns
 * the Go zero time (`0001-01-01T00:00:00Z`) for a never-invoked tool —
 * the live probe confirmed every youtube_* tool carries it — so that case
 * renders an honest `never`, never a fabricated "just now".
 */
export function lastUsed(iso: string, now: number = Date.now()): string {
  if (!iso || iso.startsWith('0001-01-01')) {
    return 'never';
  }
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) {
    return 'never';
  }
  const deltaMin = Math.round((now - then) / 60000);
  if (deltaMin < 1) return 'just now';
  if (deltaMin < 60) return `${deltaMin}m ago`;
  const deltaHr = Math.round(deltaMin / 60);
  if (deltaHr < 24) return `${deltaHr}h ago`;
  return `${Math.round(deltaHr / 24)}d ago`;
}

/** Maps a tool's OAuth binding status onto the shared StatusChip scale. */
export function oauthKind(s: ToolOAuthStatus): StatusKind {
  if (s === 'Bound') return 'success';
  if (s === 'Required') return 'accent';
  if (s === 'Expired') return 'danger';
  return 'neutral';
}

/** Maps a tool's approval policy onto the shared StatusChip scale. */
export function approvalKind(p: ToolApprovalPolicy): StatusKind {
  if (p === 'auto') return 'success';
  if (p === 'gated') return 'warning';
  return 'danger';
}

/** Maps a tool's health pill onto the shared StatusChip scale. */
export function statusKind(s: ToolStatus): StatusKind {
  if (s === 'Healthy') return 'success';
  if (s === 'Degraded') return 'warning';
  return 'danger';
}

/**
 * The status the `<PageState>` boundary renders. `loading` / `error` /
 * `disconnected` pass through; the ready/empty split is derived LIVE from
 * the loaded-row count so a catalog whose rows all dropped under a tight
 * facet still reads "empty" (the Events-page D-180 lesson — derive the
 * display state, never a `status` value set once at load that won't flip
 * when data streams in).
 */
export function displayStatus(status: PageStatus, rowCount: number): PageStatus {
  if (status === 'loading' || status === 'error' || status === 'disconnected') {
    return status;
  }
  return rowCount > 0 ? 'ready' : 'empty';
}
