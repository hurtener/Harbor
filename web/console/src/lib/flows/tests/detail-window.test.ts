/**
 * Flow-detail run-history date-filter tests (D-295).
 *
 * Pins the pure `runWindowBounds` date→RFC-3339 conversion (inclusive
 * lower, exclusive upper made inclusive-of-the-end-day) and the
 * `FlowDetailState` fold: Apply drives `flows.runs.list` with the bounds
 * server-side; Clear restores unbounded paging.
 */
import { describe, expect, it, vi } from 'vitest';
import { FlowDetailState, runWindowBounds } from '../detail.svelte.js';
import type { FlowsProtocol } from '$lib/protocol/flows.js';
import type { FlowDescription, FlowRunsListResponse } from '../types.js';

describe('runWindowBounds', () => {
  it('is unbounded on both sides for empty inputs', () => {
    expect(runWindowBounds('', '')).toEqual({});
  });

  it('maps the lower bound to the picked day UTC midnight (inclusive)', () => {
    expect(runWindowBounds('2026-07-01', '')).toEqual({
      since: '2026-07-01T00:00:00.000Z'
    });
  });

  it('maps the upper bound to the picked day UTC midnight (inclusive — no compensation)', () => {
    expect(runWindowBounds('', '2026-07-01')).toEqual({
      until: '2026-07-01T00:00:00.000Z'
    });
  });

  it('produces a closed [since, until] range for a both-sided window', () => {
    expect(runWindowBounds('2026-07-01', '2026-07-03')).toEqual({
      since: '2026-07-01T00:00:00.000Z',
      until: '2026-07-03T00:00:00.000Z'
    });
  });

  it('treats a malformed date as unbounded rather than sending garbage', () => {
    expect(runWindowBounds('not-a-date', '2026/07/01')).toEqual({});
  });
});

/** A minimal FlowsProtocol stub recording the runsList request. */
function stubFlows(): { flows: FlowsProtocol; calls: unknown[] } {
  const calls: unknown[] = [];
  const desc = {
    flow: { id: 'f1' },
    nodes: [],
    edges: []
  } as unknown as FlowDescription;
  const resp: FlowRunsListResponse = {
    runs: [],
    page: 1,
    page_size: 50,
    page_count: 0,
    total_rows: 0
  };
  const flows = {
    describe: vi.fn(async () => desc),
    runsList: vi.fn(async (req: unknown) => {
      calls.push(req);
      return resp;
    })
  } as unknown as FlowsProtocol;
  return { flows, calls };
}

describe('FlowDetailState run-history filter fold', () => {
  it('sends no bounds on first load', async () => {
    const { flows, calls } = stubFlows();
    const state = new FlowDetailState();
    state.load('f1', flows);
    await vi.waitFor(() => expect(calls.length).toBe(1));
    expect(calls[0]).toEqual({ flow_id: 'f1', since: undefined, until: undefined });
    expect(state.runsFilterActive).toBe(false);
  });

  it('drives the bounds server-side on Apply and reflects an active filter', async () => {
    const { flows, calls } = stubFlows();
    const state = new FlowDetailState();
    state.load('f1', flows);
    await vi.waitFor(() => expect(calls.length).toBe(1));

    state.applyRunFilter('2026-07-01', '2026-07-03');
    await vi.waitFor(() => expect(calls.length).toBe(2));
    // The end-day maps directly to its UTC midnight — no compensation.
    // The server treats `until` as INCLUSIVE, so a run at exactly this
    // instant is included (the closed-window semantics matching
    // TaskFilter/sessions; the server-side inclusion is pinned by the Go
    // `until_includes_boundary` case).
    expect(calls[1]).toEqual({
      flow_id: 'f1',
      since: '2026-07-01T00:00:00.000Z',
      until: '2026-07-03T00:00:00.000Z'
    });
    expect(state.runsFilterActive).toBe(true);
  });

  it('restores unbounded paging on Clear', async () => {
    const { flows, calls } = stubFlows();
    const state = new FlowDetailState();
    state.load('f1', flows);
    await vi.waitFor(() => expect(calls.length).toBe(1));
    state.applyRunFilter('2026-07-01', '2026-07-03');
    await vi.waitFor(() => expect(calls.length).toBe(2));

    state.clearRunFilter();
    await vi.waitFor(() => expect(calls.length).toBe(3));
    expect(calls[2]).toEqual({ flow_id: 'f1', since: undefined, until: undefined });
    expect(state.runsFilterActive).toBe(false);
  });
});
