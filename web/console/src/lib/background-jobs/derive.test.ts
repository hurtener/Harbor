// Harbor Console — Background Jobs derive() projection unit tests
// (Phase 108j / D-182).
//
// Locks the pure ETA / type / timeline projections against their honest
// states. The load-bearing cases: a row with NO progress hint derives an
// honest "Unknown" ETA (never a fabricated value); a job with no tag /
// keyword signal derives the generic "Job" (never a fabricated agent); a
// newest-first run-event page projects an oldest-first timeline.

import { describe, it, expect } from 'vitest';
import { deriveETA, deriveJobType, projectStateTimeline } from './derive.js';
import type { TaskRow } from '$lib/protocol/tasks.js';
import type { Event } from '$lib/protocol/events.js';

const NOW = Date.parse('2026-06-02T12:00:00Z');

function row(partial: Partial<TaskRow>): TaskRow {
  return {
    id: 'job-1',
    kind: 'background',
    status: 'running',
    priority: 0,
    identity: { tenant: 'dev', user: 'dev', session: 'dev' },
    parent_session_id: 'dev',
    description: '',
    query: '',
    started_at: '2026-06-02T11:00:00Z',
    updated_at: '2026-06-02T11:30:00Z',
    duration_ms: 0,
    tool_count: 0,
    background_acknowledged: true,
    last_activity_at: '2026-06-02T11:30:00Z',
    is_background: true,
    has_pending_approval: false,
    ...partial
  };
}

describe('deriveETA', () => {
  it('returns Unknown when the planner emitted no progress hint', () => {
    const eta = deriveETA(row({ progress: undefined }), NOW);
    expect(eta.known).toBe(false);
    expect(eta.label).toBe('Unknown');
    expect(eta.remainingMs).toBeNull();
  });

  it('returns Unknown for a non-finite or zero progress (never a fabricated value)', () => {
    expect(deriveETA(row({ progress: 0 }), NOW).label).toBe('Unknown');
    expect(deriveETA(row({ progress: Number.NaN }), NOW).label).toBe('Unknown');
    expect(deriveETA(row({ progress: -0.2 }), NOW).label).toBe('Unknown');
  });

  it('returns Done when the planner reported progress >= 1', () => {
    const eta = deriveETA(row({ progress: 1 }), NOW);
    expect(eta.known).toBe(true);
    expect(eta.label).toBe('Done');
    expect(eta.remainingMs).toBe(0);
  });

  it('projects remaining = elapsed * (1-p)/p over wall time', () => {
    // started 60 min ago, 25% done → remaining = 60m * 0.75/0.25 = 180m = 3h.
    const eta = deriveETA(row({ progress: 0.25 }), NOW);
    expect(eta.known).toBe(true);
    expect(eta.remainingMs).toBeCloseTo(3 * 60 * 60 * 1000, -2);
    expect(eta.label).toBe('~3h');
  });

  it('formats sub-minute and minute+second spans', () => {
    // 60 min elapsed, 99% done → remaining ≈ 36s.
    expect(deriveETA(row({ progress: 0.99 }), NOW).label).toBe('~36s');
    // 60 min elapsed, 80% done → remaining = 15m.
    expect(deriveETA(row({ progress: 0.8 }), NOW).label).toBe('~15m');
  });

  it('returns Unknown when started_at is unparseable', () => {
    expect(deriveETA(row({ progress: 0.5, started_at: 'not-a-date' }), NOW).label).toBe(
      'Unknown'
    );
  });
});

describe('deriveJobType', () => {
  it('prefers the first non-empty planner tag', () => {
    expect(deriveJobType(row({ tags: ['', 'crawler'], description: 'report' }))).toBe(
      'crawler'
    );
  });

  it('maps keywords in the description / query to a type label', () => {
    expect(deriveJobType(row({ description: 'Rebuild the memory index' }))).toBe('Indexer');
    expect(deriveJobType(row({ description: 'Summarise the archived threads' }))).toBe(
      'Report'
    );
    expect(deriveJobType(row({ query: 'long-poll an external service' }))).toBe('Long Poll');
  });

  it('falls back to the generic Job (never a fabricated agent)', () => {
    expect(deriveJobType(row({ tags: [], description: '', query: '' }))).toBe('Job');
  });
});

describe('projectStateTimeline', () => {
  function ev(type: string, at: string, seq: number): Event {
    return {
      type,
      sequence: seq,
      occurred_at: at,
      tenant: 'dev',
      user: 'dev',
      session: 'dev',
      run: undefined,
      payload: {}
    };
  }

  it('projects task.* lifecycle events oldest-first', () => {
    // Page order is newest-first (the subscription order).
    const events = [
      ev('task.completed', '2026-06-02T11:30:00Z', 3),
      ev('task.started', '2026-06-02T11:05:00Z', 2),
      ev('task.spawned', '2026-06-02T11:00:00Z', 1)
    ];
    const timeline = projectStateTimeline(events);
    expect(timeline.map((s) => s.status)).toEqual(['spawned', 'started', 'completed']);
  });

  it('excludes task.group_* events (not this task transitions)', () => {
    const events = [
      ev('task.group_resolved', '2026-06-02T11:10:00Z', 2),
      ev('task.started', '2026-06-02T11:05:00Z', 1)
    ];
    expect(projectStateTimeline(events).map((s) => s.status)).toEqual(['started']);
  });

  it('returns an empty timeline when the live stream saw no lifecycle events', () => {
    const events = [ev('llm.cost.recorded', '2026-06-02T11:05:00Z', 1)];
    expect(projectStateTimeline(events)).toEqual([]);
  });
});
