// Harbor Console — Flows page pure projections (Phase 108p / D-188).
//
// The king-file refactor pulls every pure, side-effect-free projection the
// Flows page renders into this unit-testable module — the same split the
// Tools-108k / MCP-108m / Memory-108n / Artifacts-108o rebuilds used. Nothing
// here touches the Protocol client, `localStorage`, the DOM, or any reactive
// state; the `FlowsListState` / `FlowDetailState` controllers and the views
// import these helpers and `tests/derive.test.ts` pins them.
//
// The Flows page already shipped a pure `format.ts` (Phase 73i) consumed by the
// page-specific components (`BudgetMeter`, `RunHistoryTable`, …). Rather than
// churn those imports, `derive.ts` RE-EXPORTS the formatting helpers and adds
// the page-level projections (`toPageError` / `displayStatus` / `successKind` /
// `health`). New controller / view code imports everything from here.
//
// No Svelte runes here (a plain `.ts` module); design-token concerns live in
// the `.svelte` views, not in these projections.

import type { StatusKind } from '$lib/components/ui/StatusChip.svelte';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { FlowDescription } from './types.js';

// Re-export the shipped pure formatting helpers so callers have a single
// `derive.ts` entry point (the page-specific components keep importing
// `format.ts` directly — no churn).
export {
  formatDurationMS,
  formatRate,
  formatCost,
  formatRelative,
  budgetFraction,
  shortRunID
} from './format.js';

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
 * The status the catalog `<PageState>` boundary renders. `loading` / `error` /
 * `disconnected` pass through; the ready/empty split is derived LIVE from the
 * loaded-row count so a catalog whose rows all dropped under a tight filter
 * still reads "empty" (the Events-page D-180 lesson).
 */
export function displayStatus(status: PageStatus, rowCount: number): PageStatus {
  if (status === 'loading' || status === 'error' || status === 'disconnected') {
    return status;
  }
  return rowCount > 0 ? 'ready' : 'empty';
}

/**
 * Maps a flow's success rate + run count onto the shared StatusChip kind scale,
 * for the catalog's per-row Success chip. A flow with zero runs in the window is
 * `neutral` — an honest "no signal", never a fabricated green.
 */
export function successKind(rate: number, runs: number): StatusKind {
  if (runs === 0) return 'neutral';
  if (rate >= 0.95) return 'success';
  if (rate >= 0.7) return 'warning';
  return 'danger';
}

/** A flow's health pill (detail header) — same thresholds as {@link successKind}. */
export function health(description: FlowDescription | null): {
  label: string;
  kind: StatusKind;
} {
  if (!description) return { label: 'Unknown', kind: 'neutral' };
  const flow = description.flow;
  if (flow.runs_24h === 0) return { label: 'No runs', kind: 'neutral' };
  if (flow.success_rate >= 0.95) return { label: 'Healthy', kind: 'success' };
  if (flow.success_rate >= 0.7) return { label: 'Degraded', kind: 'warning' };
  return { label: 'Errored', kind: 'danger' };
}
