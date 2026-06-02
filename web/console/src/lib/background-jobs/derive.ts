// Harbor Console — Background Jobs page pure projections (Phase 108j /
// D-182).
//
// Pure, DOM-free derivations the Background Jobs queue + right-rail
// render. Extracted into a unit-testable module (the §17.6 / king-file
// refactor) so no `.svelte` component re-implements ETA / type / timeline
// logic, and so each projection is locked by a Vitest against its honest
// states. None of these add a Protocol field or issue a Protocol call —
// they fold the already-shipped `tasks.list` row + the run-scoped
// `events.subscribe` stream the page already consumes.
//
// The honest-state contract (PAGE-POLISH §1, operator STEP-0 sign-off):
//   - ETA is the planner's OWN progress hint projected over elapsed wall
//     time — never a fabricated value. No hint → "Unknown".
//   - The type badge is derived from the planner-emitted tags / spawn
//     description — never a fabricated agent. No signal → "Job".
//   - The state timeline is the run's real `task.*` lifecycle events —
//     empty when the live SSE saw none (it is live-only, no backlog).

import type { TaskRow } from '../protocol/tasks.js';
import type { Event } from '../protocol/events.js';

/* ================================================================== */
/* ETA — derived from the planner's progress hint                      */
/* ================================================================== */

/** The derived ETA estimate the queue + Progress tab render. */
export interface ETAEstimate {
  /** False when no honest estimate exists — render "Unknown". */
  known: boolean;
  /** The display label: "Unknown" / "Done" / "~3m" / "~2h 10m". */
  label: string;
  /** The estimated remaining wall-clock ms, or null when unknown. */
  remainingMs: number | null;
}

/** Compact "~3m" / "~45s" / "~2h 10m" rendering of a positive ms span. */
function humanizeMs(ms: number): string {
  const s = Math.round(ms / 1000);
  if (s < 60) return `~${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) {
    const rem = s % 60;
    return rem > 0 ? `~${m}m ${rem}s` : `~${m}m`;
  }
  const h = Math.floor(m / 60);
  const remM = m % 60;
  return remM > 0 ? `~${h}h ${remM}m` : `~${h}h`;
}

/**
 * Derive an ETA from the planner's numeric progress hint, projected over
 * elapsed wall time: `remaining = elapsed × (1 − progress) / progress`.
 * This is the planner's OWN progress projected forward — it is labelled an
 * estimate and is NEVER a fabricated value:
 *   - no hint (`progress` undefined) or a non-finite / ≤0 hint → "Unknown";
 *   - a hint ≥1 (the planner reported done) → "Done" (0 remaining);
 *   - an unparseable `started_at` → "Unknown";
 *   - otherwise the projected remaining span, prefixed "~".
 *
 * @param row    the queue row (its `progress` hint + `started_at`).
 * @param nowMs  the current epoch ms (injected for deterministic tests).
 */
export function deriveETA(
  row: Pick<TaskRow, 'progress' | 'started_at'>,
  nowMs: number
): ETAEstimate {
  const p = row.progress;
  if (p === undefined || !Number.isFinite(p) || p <= 0) {
    return { known: false, label: 'Unknown', remainingMs: null };
  }
  if (p >= 1) {
    return { known: true, label: 'Done', remainingMs: 0 };
  }
  const started = Date.parse(row.started_at);
  if (Number.isNaN(started)) {
    return { known: false, label: 'Unknown', remainingMs: null };
  }
  const elapsed = Math.max(0, nowMs - started);
  const remaining = (elapsed * (1 - p)) / p;
  return { known: true, label: humanizeMs(remaining), remainingMs: remaining };
}

/* ================================================================== */
/* Type badge — derived from tags / description                        */
/* ================================================================== */

/** Keyword → type-label rules, applied in order over tags + description. */
const TYPE_KEYWORDS: [RegExp, string][] = [
  [/index|crawl|embed|rebuild/i, 'Indexer'],
  [/report|summar|digest/i, 'Report'],
  [/poll|wait|watch|stream/i, 'Long Poll']
];

/**
 * Derive a human "type" label for a background job. There is NO dedicated
 * wire field for a job type, so this is a Console-local derivation
 * (honest, never a fabricated agent):
 *   - the first non-empty planner-emitted tag wins (the planner's own
 *     label);
 *   - else a keyword match over the spawn description / query maps to
 *     Indexer / Report / Long Poll;
 *   - else the generic "Job".
 */
export function deriveJobType(
  row: Pick<TaskRow, 'tags' | 'description' | 'query'>
): string {
  const tag = (row.tags ?? []).find((t) => t.trim() !== '');
  if (tag !== undefined) {
    return tag;
  }
  const hay = `${row.description ?? ''} ${row.query ?? ''}`;
  for (const [re, label] of TYPE_KEYWORDS) {
    if (re.test(hay)) {
      return label;
    }
  }
  return 'Job';
}

/* ================================================================== */
/* State-transition timeline — from the run's task.* lifecycle events  */
/* ================================================================== */

/** One state-transition step the Progress tab timeline renders. */
export interface TimelineStep {
  /** The lifecycle status, e.g. "spawned" / "started" / "completed". */
  status: string;
  /** The transition timestamp (RFC-3339), straight off the event. */
  at: string;
}

/**
 * Project the run's `task.*` lifecycle events into an ordered (oldest-
 * first) state-transition timeline. The TaskGroup lifecycle events
 * (`task.group_*`) are NOT transitions of this task, so they are excluded.
 * The input may be newest-first (the subscription page order); the output
 * is always sorted oldest-first by timestamp. Returns `[]` when the live
 * stream saw no lifecycle events (it is live-only — no backlog on a fresh
 * connect), which the Progress tab renders as an honest empty timeline.
 */
export function projectStateTimeline(runEvents: readonly Event[]): TimelineStep[] {
  const steps: TimelineStep[] = [];
  for (const e of runEvents) {
    if (!e.type.startsWith('task.') || e.type.startsWith('task.group_')) {
      continue;
    }
    steps.push({ status: e.type.slice('task.'.length), at: e.occurred_at });
  }
  steps.sort((a, b) => {
    const ta = Date.parse(a.at);
    const tb = Date.parse(b.at);
    if (Number.isNaN(ta) || Number.isNaN(tb)) return 0;
    return ta - tb;
  });
  return steps;
}
