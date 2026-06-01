/**
 * Sessions detail — `trajectory.ts` unit tests (Phase 108g / D-179).
 *
 * Pins the event→step Trajectory projection: the planner / tool / task
 * lifecycle filter, the chronological (sequence-ascending) ordering
 * regardless of the stream's newest-first buffer order, the kind
 * mapping, and the PascalCase payload detail extraction (the SSE
 * `wireEvent` payload is Go field names, not json tags — PAGE-POLISH
 * §3.3). A non-lifecycle event NEVER becomes a step; an unparseable
 * payload yields an empty detail, never a fabricated one (CLAUDE.md §13).
 */
import { describe, expect, it } from 'vitest';
import { projectTrajectory, trajectoryKind, trajectoryDetail } from '../trajectory.js';
import type { Event } from '../../protocol/events.js';

function ev(type: string, sequence: number, payload?: Record<string, unknown>): Event {
  return {
    type,
    sequence,
    occurred_at: '2026-06-01T12:00:00Z',
    tenant: 'dev',
    user: 'dev',
    session: 's1',
    payload
  };
}

describe('trajectory: trajectoryKind', () => {
  it('maps the dotted prefix onto the step kind', () => {
    expect(trajectoryKind('planner.decision')).toBe('planner');
    expect(trajectoryKind('tool.completed')).toBe('tool');
    expect(trajectoryKind('task.started')).toBe('task');
    expect(trajectoryKind('llm.cost.recorded')).toBe('other');
  });
});

describe('trajectory: trajectoryDetail', () => {
  it('reads a PascalCase tool name on a tool event', () => {
    expect(trajectoryDetail(ev('tool.completed', 1, { Tool: 'youtube_get_metadata' }))).toBe(
      'youtube_get_metadata'
    );
  });

  it('joins tool + error on a tool failure', () => {
    expect(
      trajectoryDetail(ev('tool.failed', 1, { Tool: 'web_search', Error: 'timeout' }))
    ).toBe('web_search — timeout');
  });

  it('reads a planner action / thought', () => {
    expect(trajectoryDetail(ev('planner.decision', 1, { Action: 'call web_search' }))).toBe(
      'call web_search'
    );
  });

  it('returns "" when no descriptive field is present (no fabrication)', () => {
    expect(trajectoryDetail(ev('task.started', 1, { Foo: 'bar' }))).toBe('');
    expect(trajectoryDetail(ev('task.started', 1))).toBe('');
  });
});

describe('trajectory: projectTrajectory', () => {
  it('keeps only planner / tool / task lifecycle events', () => {
    const steps = projectTrajectory([
      ev('planner.decision', 1),
      ev('llm.cost.recorded', 2, { Model: 'gpt' }),
      ev('tool.completed', 3, { Tool: 't' }),
      ev('control.applied', 4),
      ev('task.started', 5)
    ]);
    expect(steps.map((s) => s.type)).toEqual([
      'planner.decision',
      'tool.completed',
      'task.started'
    ]);
  });

  it('orders steps by sequence ascending even when the input is newest-first', () => {
    // The subscription buffer is newest-first; the timeline must read
    // decision → tool → result regardless.
    const steps = projectTrajectory([
      ev('task.completed', 3),
      ev('tool.completed', 2),
      ev('planner.decision', 1)
    ]);
    expect(steps.map((s) => s.sequence)).toEqual([1, 2, 3]);
    expect(steps.map((s) => s.kind)).toEqual(['planner', 'tool', 'task']);
  });

  it('builds a readable label from the dotted type', () => {
    const [step] = projectTrajectory([ev('tool.approval_requested', 1)]);
    expect(step.label).toBe('Tool approval requested');
  });
});
