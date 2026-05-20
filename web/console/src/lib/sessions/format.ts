// Harbor Console — Sessions-page formatting helpers (Phase 73c / D-122).
//
// Pure, DOM-free formatting functions shared by the Sessions
// components. Extracted so they are unit-testable with Vitest and so no
// `.svelte` component re-implements duration / cost / id formatting.

import type { SessionStatus } from './types.js';
import type { StatusKind } from '../components/ui/index.js';

/** Format a Go `time.Duration` (nanoseconds) as a compact human string. */
export function formatDurationNS(ns: number | undefined): string {
  if (!ns || ns <= 0) {
    return '—';
  }
  const ms = ns / 1_000_000;
  if (ms < 1000) {
    return `${Math.round(ms)} ms`;
  }
  const s = ms / 1000;
  if (s < 60) {
    return `${s.toFixed(1)} s`;
  }
  const min = s / 60;
  if (min < 60) {
    return `${min.toFixed(1)} min`;
  }
  return `${(min / 60).toFixed(1)} h`;
}

/** Format an integer US-cent amount as a USD string. */
export function formatCostCents(cents: number | undefined): string {
  if (!cents || cents <= 0) {
    return '$0.00';
  }
  return `$${(cents / 100).toFixed(2)}`;
}

/** Format an integer token count with thousands separators. */
export function formatTokens(tokens: number | undefined): string {
  if (!tokens || tokens <= 0) {
    return '0';
  }
  return tokens.toLocaleString('en-US');
}

/** Format an ISO timestamp as a relative "time ago" string. */
export function formatRelative(
  iso: string | undefined,
  now: Date = new Date()
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

/** Truncate a session id for compact display (hover reveals the full id). */
export function shortSessionID(id: string): string {
  return id.length > 16 ? `${id.slice(0, 16)}…` : id;
}

/** Map a session status onto the shared `StatusChip` token scale. */
export function statusKind(status: SessionStatus): StatusKind {
  switch (status) {
    case 'running':
      return 'success';
    case 'paused':
      return 'warning';
    case 'failed':
      return 'danger';
    default:
      return 'neutral';
  }
}
