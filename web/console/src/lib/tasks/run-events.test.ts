// Harbor Console — run-events projection unit tests (Phase 108i / D-181).
//
// Locks the per-task run-match + cost projection against CAPTURED REAL
// SSE frames (probed live from the YouTube validation agent, 2026-06-01).
// The load-bearing case: a `task.completed` whose top-level `run` is null
// but whose `payload.TaskID` matches MUST be included — a naive
// `e.run === taskID` filter would drop it (PAGE-POLISH §3.3).

import { describe, it, expect } from 'vitest';
import {
  eventBelongsToRun,
  filterRunEvents,
  projectRunCost,
  filterControlEvents,
  filterInterventionEvents,
  filterGroupEvents
} from './run-events.js';
import type { Event } from '$lib/protocol/events.js';

const RUN = '01KT2HDK9S1AEXD0FKH3PKV7CC';
const OTHER = '01KT2HCGG89HDSDRJVTZR7VRMZ';

// A captured `task.completed` frame — top-level `run` is NULL; the id is
// in the PascalCase payload (the live-wire finding D-181 records).
const taskCompleted: Event = {
  type: 'task.completed',
  sequence: 42,
  occurred_at: '2026-06-01T21:28:17Z',
  tenant: 'dev',
  user: 'dev',
  session: 'tasks-sse-1780349322',
  run: undefined,
  payload: { TaskID: RUN }
};

// A captured `task.started` frame — same null-run / payload.TaskID shape.
const taskStarted: Event = {
  type: 'task.started',
  sequence: 40,
  occurred_at: '2026-06-01T21:28:08Z',
  tenant: 'dev',
  user: 'dev',
  session: 'tasks-sse-1780349322',
  run: undefined,
  payload: { TaskID: RUN, PriorState: 'pending' }
};

// A captured `llm.cost.recorded` frame — top-level `run` IS populated;
// payload is PascalCase with nested Cost + Usage + Identity.RunID.
const costRecorded: Event = {
  type: 'llm.cost.recorded',
  sequence: 41,
  occurred_at: '2026-06-01T21:28:46Z',
  tenant: 'dev',
  user: 'dev',
  session: 'tasks-sse-1780349322',
  run: RUN,
  payload: {
    Identity: { TenantID: 'dev', UserID: 'dev', SessionID: 'tasks-sse-1780349322', RunID: RUN },
    Model: 'openai/gpt-5.4',
    Cost: {
      InputTokensCost: 0.0011,
      OutputTokensCost: 0.0005,
      ReasoningTokensCost: 0.0002,
      TotalCost: 0.001794,
      Currency: 'USD'
    },
    Usage: { PromptTokens: 2286, CompletionTokens: 65, ReasoningTokens: 27, TotalTokens: 2351 }
  }
};

// A foreign-run cost event — must be excluded from the task's view.
const foreignCost: Event = {
  type: 'llm.cost.recorded',
  sequence: 99,
  occurred_at: '2026-06-01T21:30:00Z',
  tenant: 'dev',
  user: 'dev',
  session: 'tasks-sse-1780349322',
  run: OTHER,
  payload: {
    Identity: { RunID: OTHER },
    Model: 'openai/gpt-5.4',
    Cost: { TotalCost: 9.99 },
    Usage: { TotalTokens: 9999 }
  }
};

describe('eventBelongsToRun', () => {
  it('includes a lifecycle event whose run is null but payload.TaskID matches', () => {
    expect(taskCompleted.run).toBeUndefined();
    expect(eventBelongsToRun(taskCompleted, RUN)).toBe(true);
  });

  it('includes an event whose top-level run matches', () => {
    expect(eventBelongsToRun(costRecorded, RUN)).toBe(true);
  });

  it('includes an event whose payload.Identity.RunID matches', () => {
    const onlyIdentity: Event = { ...costRecorded, run: undefined, payload: { Identity: { RunID: RUN } } };
    expect(eventBelongsToRun(onlyIdentity, RUN)).toBe(true);
  });

  it('excludes a foreign run', () => {
    expect(eventBelongsToRun(foreignCost, RUN)).toBe(false);
    expect(eventBelongsToRun(taskCompleted, OTHER)).toBe(false);
  });

  it('excludes when the task id is empty', () => {
    expect(eventBelongsToRun(costRecorded, '')).toBe(false);
  });
});

describe('filterRunEvents', () => {
  it('keeps only the task run, dropping foreign-run events', () => {
    const page = [taskCompleted, costRecorded, foreignCost, taskStarted];
    const run = filterRunEvents(page, RUN);
    expect(run.map((e) => e.sequence).sort()).toEqual([40, 41, 42]);
    expect(run).not.toContain(foreignCost);
  });
});

describe('projectRunCost', () => {
  it('folds the PascalCase cost + usage by token type', () => {
    const c = projectRunCost([taskCompleted, costRecorded]);
    expect(c.events).toBe(1);
    expect(c.totalUSD).toBeCloseTo(0.001794, 6);
    expect(c.inputUSD).toBeCloseTo(0.0011, 6);
    expect(c.outputUSD).toBeCloseTo(0.0005, 6);
    expect(c.reasoningUSD).toBeCloseTo(0.0002, 6);
    expect(c.promptTokens).toBe(2286);
    expect(c.outputTokens).toBe(65);
    expect(c.reasoningTokens).toBe(27);
    expect(c.totalTokens).toBe(2351);
    expect(c.models).toEqual(['openai/gpt-5.4']);
  });

  it('returns an all-zero rollup for a stream with no cost events', () => {
    const c = projectRunCost([taskCompleted, taskStarted]);
    expect(c.events).toBe(0);
    expect(c.totalUSD).toBe(0);
    expect(c.totalTokens).toBe(0);
    expect(c.models).toEqual([]);
  });

  it('skips a malformed cost payload rather than counting it as zero', () => {
    const malformed: Event = { ...costRecorded, sequence: 50, payload: { Model: 'x' } };
    const c = projectRunCost([malformed]);
    expect(c.events).toBe(0);
  });
});

describe('event-category filters', () => {
  const control: Event = { ...taskStarted, type: 'control.applied', sequence: 60, run: RUN };
  const pause: Event = { ...taskStarted, type: 'pause.requested', sequence: 61, run: RUN };
  const group: Event = { ...taskStarted, type: 'task.group_created', sequence: 62, run: RUN };

  it('partitions control / intervention / group events', () => {
    const page = [taskCompleted, control, pause, group];
    expect(filterControlEvents(page)).toEqual([control]);
    expect(filterInterventionEvents(page)).toEqual([pause]);
    expect(filterGroupEvents(page)).toEqual([group]);
  });
});
