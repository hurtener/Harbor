// Harbor Console — Sessions detail Trajectory projection (Phase 108g /
// D-179).
//
// Pure, DOM-free projection of the session-filtered `events.subscribe`
// stream into a chronological planner-step timeline (decision → tool →
// result → decision …). The Trajectory tab is a READ-ONLY projection of
// the SAME shipped event stream the other dock tabs read — there is no
// new Protocol method (the Phase 73 `state.list_trajectories` /
// `state.history` surface is still `Pending`; this reconstructs the
// trajectory from the lifecycle events the runtime already emits).
//
// Extracted as a pure module so it is unit-testable against captured
// real wire frames (the SSE payload is PascalCase Go field names, not
// json tags — PAGE-POLISH §3.3).

import type { Event } from '../protocol/events.js';

/** The kind of trajectory step — drives the timeline marker colour. */
export type TrajectoryKind = 'planner' | 'tool' | 'task' | 'other';

/** One step in the reconstructed planner trajectory. */
export interface TrajectoryStep {
  /** The per-bus monotonic sequence — the chronological sort key. */
  sequence: number;
  /** The wall-clock instant (RFC-3339 UTC). */
  occurred_at: string;
  /** The dotted canonical event type (e.g. `planner.decision`). */
  type: string;
  /** The step kind — `planner` / `tool` / `task` / `other`. */
  kind: TrajectoryKind;
  /** A short human label (`Planner decision`, `Tool completed`, …). */
  label: string;
  /** A one-line detail extracted from the payload, or '' when none. */
  detail: string;
}

/** Maps an event type onto its trajectory kind by dotted prefix. */
export function trajectoryKind(type: string): TrajectoryKind {
  if (type.startsWith('planner.')) return 'planner';
  if (type.startsWith('tool.')) return 'tool';
  if (type.startsWith('task.')) return 'task';
  return 'other';
}

/** The lifecycle event types that compose a planner trajectory. */
function isTrajectoryType(type: string): boolean {
  return (
    type.startsWith('planner.') ||
    type.startsWith('tool.') ||
    type.startsWith('task.')
  );
}

/** Title-cases a dotted event type into a readable label. */
function labelFor(type: string): string {
  const parts = type.split('.');
  const tail = parts
    .slice(1)
    .join(' ')
    .replace(/_/g, ' ');
  const head = parts[0];
  const pretty = `${head.charAt(0).toUpperCase()}${head.slice(1)} ${tail}`.trim();
  return pretty;
}

/** Reads the first present string field (PascalCase or snake_case). */
function readString(
  obj: Record<string, unknown>,
  keys: string[]
): string | null {
  for (const k of keys) {
    const v = obj[k];
    if (typeof v === 'string' && v.length > 0) {
      return v;
    }
  }
  return null;
}

/**
 * Extracts a one-line detail from a lifecycle event payload. The SSE
 * `wireEvent` payload is the Go struct marshalled WITHOUT json tags, so
 * the keys are PascalCase field names; this reads both casings so a
 * future json-tag projection still works. Returns '' when nothing
 * human-meaningful is present (never a fabricated string — §13).
 */
export function trajectoryDetail(ev: Event): string {
  const payload = ev.payload;
  if (payload === null || typeof payload !== 'object') {
    return '';
  }
  const p = payload as Record<string, unknown>;
  // Tool lifecycle: name the tool; on failure, the error.
  const tool = readString(p, ['Tool', 'tool', 'Name', 'name', 'ToolID', 'tool_id']);
  const err = readString(p, ['Error', 'error', 'Message', 'message', 'Reason', 'reason']);
  const action = readString(p, ['Action', 'action', 'Decision', 'decision']);
  const thought = readString(p, ['Thought', 'thought', 'Summary', 'summary']);
  if (ev.type.startsWith('tool.')) {
    if (tool && err) return `${tool} — ${err}`;
    if (tool) return tool;
    if (err) return err;
  }
  if (ev.type.startsWith('planner.')) {
    if (action) return action;
    if (thought) return thought;
    if (err) return err;
  }
  // Generic fallback — any of the common descriptive fields.
  return action ?? tool ?? thought ?? err ?? '';
}

/**
 * `projectTrajectory` folds a session-filtered event page into a
 * chronological (oldest-first) planner-step timeline. Only planner /
 * tool / task lifecycle events compose a step; everything else is
 * excluded (it lives in the Events tab). Sorted by `sequence` so the
 * timeline reads decision → tool → result regardless of the stream's
 * newest-first buffer order.
 */
export function projectTrajectory(events: readonly Event[]): TrajectoryStep[] {
  return events
    .filter((ev) => isTrajectoryType(ev.type))
    .slice()
    .sort((a, b) => a.sequence - b.sequence)
    .map((ev) => ({
      sequence: ev.sequence,
      occurred_at: ev.occurred_at,
      type: ev.type,
      kind: trajectoryKind(ev.type),
      label: labelFor(ev.type),
      detail: trajectoryDetail(ev)
    }));
}
