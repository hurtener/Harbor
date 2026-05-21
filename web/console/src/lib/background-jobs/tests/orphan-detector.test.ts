/**
 * Background Jobs — `orphan-detector.ts` unit tests (Phase 73h /
 * D-128).
 *
 * Pins the pure `AwaitTask` orphan detector: empty input, a planted
 * orphan with an absent parent, a healthy child whose parent IS in the
 * snapshot, sort-invariance, a top-level (no-parent) row, and an
 * N=10_000 latency benchmark (the §"Risks" sub-50ms claim in the
 * phase plan).
 */
import { describe, expect, it } from 'vitest';
import { detectOrphans, isOrphan } from '../orphan-detector.js';
import type { TaskRow, TaskKind, TaskStatus } from '../../protocol/tasks.js';

/** Mints a minimal background-task row for the detector tests. */
function row(id: string, parentTaskID?: string, kind: TaskKind = 'background'): TaskRow {
  return {
    id,
    kind,
    status: 'running' as TaskStatus,
    priority: 0,
    identity: { tenant: 't', user: 'u', session: 's' },
    parent_session_id: 's',
    parent_task_id: parentTaskID,
    description: `task ${id}`,
    query: '',
    started_at: '2026-05-20T00:00:00Z',
    updated_at: '2026-05-20T00:00:00Z',
    duration_ms: 0,
    tool_count: 0,
    background_acknowledged: false,
    last_activity_at: '2026-05-20T00:00:00Z',
    is_background: kind === 'background',
    has_pending_approval: false
  };
}

describe('orphan-detector: detectOrphans', () => {
  it('an empty snapshot yields an empty orphan set', () => {
    expect(detectOrphans([]).size).toBe(0);
  });

  it('a row with no parent_task_id is never an orphan', () => {
    const orphans = detectOrphans([row('task-1'), row('task-2')]);
    expect(orphans.size).toBe(0);
  });

  it('a row whose parent is ABSENT from the snapshot is flagged', () => {
    // task-child names task-parent as its parent, but task-parent is
    // not a row in this snapshot — the parent finished / was GC'd.
    const orphans = detectOrphans([row('task-child', 'task-parent')]);
    expect(orphans.has('task-child')).toBe(true);
    expect(orphans.size).toBe(1);
  });

  it('a row whose parent IS in the snapshot is a healthy child', () => {
    const orphans = detectOrphans([row('task-parent'), row('task-child', 'task-parent')]);
    expect(orphans.has('task-child')).toBe(false);
    expect(orphans.size).toBe(0);
  });

  it('flags only the orphans in a mixed snapshot', () => {
    const rows = [
      row('top'), // top-level, no parent
      row('healthy', 'top'), // parent present
      row('orphan-a', 'gone-1'), // parent absent
      row('orphan-b', 'gone-2') // parent absent
    ];
    const orphans = detectOrphans(rows);
    expect(orphans.has('orphan-a')).toBe(true);
    expect(orphans.has('orphan-b')).toBe(true);
    expect(orphans.has('healthy')).toBe(false);
    expect(orphans.has('top')).toBe(false);
    expect(orphans.size).toBe(2);
  });

  it('an empty-string parent_task_id is treated as no parent', () => {
    const orphans = detectOrphans([row('task-1', '')]);
    expect(orphans.size).toBe(0);
  });

  it('is sort-invariant — row order does not change the result', () => {
    const a = [
      row('top'),
      row('healthy', 'top'),
      row('orphan', 'gone')
    ];
    const b = [
      row('orphan', 'gone'),
      row('healthy', 'top'),
      row('top')
    ];
    const orphansA = [...detectOrphans(a)].sort();
    const orphansB = [...detectOrphans(b)].sort();
    expect(orphansA).toEqual(orphansB);
    expect(orphansA).toEqual(['orphan']);
  });

  it('does not mutate its input', () => {
    const rows = [row('task-child', 'task-parent')];
    const before = JSON.stringify(rows);
    detectOrphans(rows);
    expect(JSON.stringify(rows)).toBe(before);
  });

  it('stays under 50ms for an N=10_000-row snapshot', () => {
    // The phase plan's "Orphan-detector latency" risk: the detector is
    // O(N) with a single hashed lookup per row. Benchmark it.
    const rows: TaskRow[] = [];
    for (let i = 0; i < 10_000; i++) {
      // Every other row is an orphan (its parent is absent).
      rows.push(i % 2 === 0 ? row(`t-${i}`) : row(`t-${i}`, `absent-${i}`));
    }
    const start = performance.now();
    const orphans = detectOrphans(rows);
    const elapsed = performance.now() - start;
    expect(orphans.size).toBe(5_000);
    expect(elapsed).toBeLessThan(50);
  });
});

describe('orphan-detector: isOrphan', () => {
  it('tests a row against a precomputed orphan set', () => {
    const rows = [row('top'), row('orphan', 'gone')];
    const orphans = detectOrphans(rows);
    expect(isOrphan(orphans, rows[0])).toBe(false);
    expect(isOrphan(orphans, rows[1])).toBe(true);
  });
});
