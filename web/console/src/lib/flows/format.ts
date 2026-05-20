// Harbor Console — Flows-page formatting helpers (Phase 73i / D-117).
//
// Pure, DOM-free formatting functions shared by the Flows components.
// Extracted so they are unit-testable with Vitest and so no `.svelte`
// component re-implements duration / percentage formatting.

/** Format a millisecond duration as a compact human string. */
export function formatDurationMS(ms: number | undefined): string {
  if (!ms || ms <= 0) {
    return '—';
  }
  if (ms < 1000) {
    return `${Math.round(ms)} ms`;
  }
  if (ms < 60_000) {
    return `${(ms / 1000).toFixed(1)} s`;
  }
  return `${(ms / 60_000).toFixed(1)} min`;
}

/** Format a [0,1] success rate as a percentage. */
export function formatRate(rate: number | undefined): string {
  if (rate === undefined || Number.isNaN(rate)) {
    return '—';
  }
  return `${Math.round(rate * 100)}%`;
}

/** Format a USD cost. */
export function formatCost(usd: number | undefined): string {
  if (!usd || usd <= 0) {
    return '$0.00';
  }
  return `$${usd.toFixed(2)}`;
}

/** Format an ISO timestamp as a relative "time ago" string. */
export function formatRelative(
  iso: string | undefined,
  now: Date = new Date(),
): string {
  if (!iso) {
    return 'never';
  }
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) {
    return 'never';
  }
  const deltaS = Math.max(0, Math.round((now.getTime() - then) / 1000));
  if (deltaS < 60) {
    return `${deltaS}s ago`;
  }
  if (deltaS < 3600) {
    return `${Math.round(deltaS / 60)}m ago`;
  }
  if (deltaS < 86_400) {
    return `${Math.round(deltaS / 3600)}h ago`;
  }
  return `${Math.round(deltaS / 86_400)}d ago`;
}

/** Compute a [0,1] budget-meter fill fraction, clamped. */
export function budgetFraction(used: number, cap: number): number {
  if (cap <= 0) {
    return 0;
  }
  return Math.min(1, Math.max(0, used / cap));
}

/** Short-hash a run id for compact display. */
export function shortRunID(runID: string): string {
  return runID.length > 12 ? `${runID.slice(0, 12)}…` : runID;
}
