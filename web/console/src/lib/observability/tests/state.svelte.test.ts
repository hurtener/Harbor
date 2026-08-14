/**
 * Observability — `state.svelte.ts` controller tests (HA-65, Phase 247
 * minimal Console consumer).
 *
 * Pins the page controller's behaviour without a live Runtime:
 *   - the four-state PageStatus contract — Disconnected when no Runtime,
 *     loading → ready on a successful query, error on a thrown
 *     ProtocolError, info on the `unknown_method` shape (a Runtime that
 *     does not mount `observability.query` is not an error);
 *   - the request-shape contract — every request carries an aligned
 *     UTC-grid window, closed measures, NO tenant/user/session filter
 *     (the body never widens; the only filter axis is `models`);
 *   - the honesty contract — the freshness block is preserved, the
 *     ready/empty split is derived live, a measure selection can never
 *     empty, and a measure sort always keys on a member of the
 *     selection;
 *   - deterministic cursor pagination (next / prev via the cursor
 *     stack, restart from page 1 on any control change).
 *
 * A fake `ProtocolClient` is injected (CONVENTIONS.md §6); the
 * connection is seeded into `localStorage` (the agentconfig-spec
 * pattern).
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { STORAGE_KEYS } from '$lib/connection.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { ProtocolClient } from '$lib/protocol/harbor.js';
import type {
  ObservabilityQueryRequest,
  ObservabilityQueryResponse
} from '../../protocol/observability.js';
import { ObservabilityPageState } from '../state.svelte.js';
import {
  alignCeilUtc,
  alignFloorUtc,
  DEFAULT_OBSERVABILITY_MEASURES,
  HOUR_MS,
  isAlignedIso,
  toUtcIso
} from '../derive.js';

/** A deterministic query response fixture (current + covered). */
const RESPONSE: ObservabilityQueryResponse = {
  rows: [
    {
      bucket_start: '2026-08-14T05:00:00Z',
      dimensions: { user: 'u1', model: 'm1' },
      measures: {
        llm_completions: { n: 12, scale: 1 },
        llm_cost_micros: { n: 123_456_789, scale: 1_000_000 }
      }
    }
  ],
  next_cursor: 'cursor-2',
  quality: {
    state: 'current',
    watermark: 42,
    watermark_at: '2026-08-14T05:30:00Z',
    retention_start: '2026-08-13T00:00:00Z',
    retention_end: '2026-08-14T05:30:00Z',
    coverage: 'covered'
  },
  protocol_version: '0.1.0'
};

function seedConnection(scopes = 'tenant.user'): void {
  localStorage.setItem(STORAGE_KEYS.baseURL, 'http://127.0.0.1:18080');
  localStorage.setItem(STORAGE_KEYS.token, 'dummy-token');
  localStorage.setItem(STORAGE_KEYS.tenant, 't1');
  localStorage.setItem(STORAGE_KEYS.user, 'u1');
  localStorage.setItem(STORAGE_KEYS.session, 's1');
  localStorage.setItem(STORAGE_KEYS.scopes, scopes);
}

function fakeClient(
  respond: (req: ObservabilityQueryRequest) => ObservabilityQueryResponse = () => RESPONSE
): ProtocolClient {
  const query = vi.fn(async (req: ObservabilityQueryRequest) => respond(req));
  return { observability: { query } } as unknown as ProtocolClient;
}

/** Typed accessor for the injected observability mocks (avoids `any`). */
type ObsMocks = { query: ReturnType<typeof vi.fn> };
function obs(client: ProtocolClient): ObsMocks {
  return client.observability as unknown as ObsMocks;
}

/** The last request the injected mock received. */
function lastRequest(client: ProtocolClient): ObservabilityQueryRequest {
  const calls = obs(client).query.mock.calls;
  return calls[calls.length - 1][0] as ObservabilityQueryRequest;
}

/** Wait for the controller's async loadPage to settle (eventually-style,
 * never a fixed sleep). */
async function settle(state: ObservabilityPageState, expected: PageStatus): Promise<void> {
  await vi.waitFor(() => {
    expect(state.status).not.toBe('loading');
  });
  expect(state.status).toBe(expected);
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('ObservabilityPageState — four-state contract', () => {
  it('enters the disconnected status when no Runtime is attached', () => {
    const state = new ObservabilityPageState();
    state.load();
    expect(state.status).toBe('disconnected');
    expect(state.disconnected).toBe(true);
  });

  it('loads page 1 to ready and preserves the mandatory freshness block', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    expect(state.rows).toHaveLength(1);
    expect(state.quality).not.toBeNull();
    expect(state.quality?.state).toBe('current');
    expect(state.quality?.watermark).toBe(42);
    expect(state.quality?.coverage).toBe('covered');
    expect(state.displayStatus).toBe('ready');
  });

  it('derives empty live from a zero-row response (with the freshness block intact)', async () => {
    seedConnection();
    const client = fakeClient(() => ({
      rows: [],
      quality: { state: 'current', watermark: 9, coverage: 'covered' },
      protocol_version: '0.1.0'
    }));
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    expect(state.rows).toHaveLength(0);
    expect(state.displayStatus).toBe('empty');
    expect(state.quality?.state).toBe('current');
  });

  it('routes a thrown ProtocolError to the error state and drops last-good rows', async () => {
    seedConnection();
    const client = fakeClient(() => {
      throw new ProtocolError('invalid_request', 'unsupported measure', 400);
    });
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'error');
    expect(state.pageError).toEqual({ code: 'invalid_request', message: 'unsupported measure' });
    expect(state.resp).toBeNull();
    expect(state.displayStatus).toBe('error');
  });

  it('routes the unknown_method shape to the info state, not error', async () => {
    seedConnection();
    const client = fakeClient(() => {
      throw new ProtocolError('unknown_method', 'no such method', 501);
    });
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'info');
    expect(state.info?.headline).toContain('Observability');
    expect(state.pageError).toBeNull();
  });
});

describe('ObservabilityPageState — request-shape contract', () => {
  it('sends an aligned window, closed measures, a limit, and NO identity filters', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    const req = lastRequest(client);
    // The window is aligned to the hour grid by construction (preset).
    expect(isAlignedIso(req.from, 'hour')).toBe(true);
    expect(isAlignedIso(req.to, 'hour')).toBe(true);
    expect(Date.parse(req.to) - Date.parse(req.from)).toBe(24 * HOUR_MS);
    expect(req.bucket).toBe('hour');
    expect(req.limit).toBeGreaterThan(0);
    expect(req.measures).toEqual([...DEFAULT_OBSERVABILITY_MEASURES]);
    // The body NEVER supplies tenant/user/session identity for widening —
    // at most the closed `models` axis rides in filters.
    expect(req.filters).toBeUndefined();
  });

  it('applies the closed models filter axis when the operator types one', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    state.setModelFilterText('  gpt-5 , claude, gpt-5 ');
    state.applyModelFilter();
    await settle(state, 'ready');
    const req = lastRequest(client);
    expect(req.filters?.models).toEqual(['gpt-5', 'claude']);
    expect(req.filters?.tenant_ids).toBeUndefined();
    expect(req.filters?.user_ids).toBeUndefined();
    expect(req.filters?.session_ids).toBeUndefined();
  });

  it('includes the group-by subset and a measure sort with a member sort_measure', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    state.toggleGroupBy('session');
    state.toggleGroupBy('model');
    state.setSort('measure_desc');
    await settle(state, 'ready');
    const req = lastRequest(client);
    expect(req.group_by).toEqual(['session', 'model']);
    expect(req.sort).toBe('measure_desc');
    expect(req.sort_measure).not.toBeNull();
    expect(req.measures).toContain(req.sort_measure);
  });

  it('never emits an empty measures set — the last measure cannot be removed', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    // Remove every measure except one, then try to remove the last.
    for (const key of [...state.selectedMeasures]) {
      state.toggleMeasure(key);
    }
    // Every toggle above except the final one succeeded; the last toggle
    // was a no-op, so exactly one measure remains.
    expect(state.selectedMeasures).toHaveLength(1);
    state.toggleMeasure(state.selectedMeasures[0]);
    expect(state.selectedMeasures).toHaveLength(1);
    expect(lastRequest(client).measures).toHaveLength(1);
  });

  it('resets the sort measure when its measure leaves the selection', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    state.setSort('measure_desc');
    state.setSortMeasure('llm_cost_micros');
    await settle(state, 'ready');
    expect(lastRequest(client).sort_measure).toBe('llm_cost_micros');
    state.toggleMeasure('llm_cost_micros');
    await settle(state, 'ready');
    expect(state.sortMeasure).toBeNull();
    // The measure sort falls back to a member of the new selection.
    expect(state.effectiveSortMeasure).not.toBeNull();
    expect(state.selectedMeasures).toContain(state.effectiveSortMeasure);
    expect(lastRequest(client).sort_measure).toBe(state.effectiveSortMeasure);
  });
});

describe('ObservabilityPageState — window honesty', () => {
  it('re-aligns a hand-edited window on refresh — never sends raw edges', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    // Off-grid raw edges (local wall time): mid-hour start/end.
    state.fromRaw = '2026-08-14T05:12:00';
    state.toRaw = '2026-08-14T07:38:00';
    state.refresh();
    await settle(state, 'ready');
    const rawFrom = Date.parse(state.fromRaw);
    const rawTo = Date.parse(state.toRaw);
    expect(state.fromMs).toBe(alignFloorUtc(rawFrom, 'hour'));
    expect(state.toMs).toBe(alignCeilUtc(rawTo, 'hour'));
    const req = lastRequest(client);
    expect(req.from).toBe(toUtcIso(state.fromMs));
    expect(req.to).toBe(toUtcIso(state.toMs));
    expect(isAlignedIso(req.from, 'hour')).toBe(true);
    expect(isAlignedIso(req.to, 'hour')).toBe(true);
    // The raw inputs snapped to the aligned window (round-trip exact).
    expect(Date.parse(state.fromRaw)).toBe(state.fromMs);
    expect(Date.parse(state.toRaw)).toBe(state.toMs);
  });

  it('applyPreset switches the aligned window and restarts from page 1', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    state.applyPreset('72h');
    await settle(state, 'ready');
    expect(state.presetId).toBe('72h');
    const req = lastRequest(client);
    expect(isAlignedIso(req.from, 'hour')).toBe(true);
    expect(isAlignedIso(req.to, 'hour')).toBe(true);
    expect(Date.parse(req.to) - Date.parse(req.from)).toBe(72 * HOUR_MS);
    expect(state.cursorStack).toEqual(['']);
    expect(state.pageIndex).toBe(1);
  });

  it('setBucket re-aligns the current window onto the new grid', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    state.setBucket('day');
    await settle(state, 'ready');
    const req = lastRequest(client);
    expect(req.bucket).toBe('day');
    expect(isAlignedIso(req.from, 'day')).toBe(true);
    expect(isAlignedIso(req.to, 'day')).toBe(true);
  });
});

describe('ObservabilityPageState — deterministic cursor paging', () => {
  it('pages forward through the opaque cursor and back through the stack', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    expect(state.pageIndex).toBe(1);
    expect(state.hasNextPage).toBe(true);

    state.nextPage();
    await settle(state, 'ready');
    expect(state.pageIndex).toBe(2);
    expect(state.cursorStack).toEqual(['', 'cursor-2']);
    expect(lastRequest(client).cursor).toBe('cursor-2');

    // The mock always returns the same cursor; a real runtime would
    // return the next page's cursor. Paging again keeps pushing.
    state.nextPage();
    await settle(state, 'ready');
    expect(state.pageIndex).toBe(3);

    state.prevPage();
    await settle(state, 'ready');
    expect(state.pageIndex).toBe(2);
    expect(state.cursorStack).toEqual(['', 'cursor-2']);
    expect(lastRequest(client).cursor).toBe('cursor-2');

    state.prevPage();
    await settle(state, 'ready');
    expect(state.pageIndex).toBe(1);
    expect(state.cursorStack).toEqual(['']);
    // The first page omits the cursor entirely ("" = first page).
    expect(lastRequest(client).cursor ?? '').toBe('');

    // Cannot page before the first page.
    state.prevPage();
    await settle(state, 'ready');
    expect(state.pageIndex).toBe(1);
  });

  it('does not page forward when the response carries no next cursor', async () => {
    seedConnection();
    const client = fakeClient(() => ({
      rows: RESPONSE.rows,
      quality: RESPONSE.quality,
      protocol_version: '0.1.0'
    }));
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    expect(state.hasNextPage).toBe(false);
    const callsBefore = obs(client).query.mock.calls.length;
    state.nextPage();
    await settle(state, 'ready');
    expect(obs(client).query.mock.calls.length).toBe(callsBefore);
    expect(state.pageIndex).toBe(1);
  });

  it('a control change restarts from the first page (the cursor is query-bound)', async () => {
    seedConnection();
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    state.nextPage();
    await settle(state, 'ready');
    expect(state.pageIndex).toBe(2);
    state.toggleGroupBy('tenant');
    await settle(state, 'ready');
    expect(state.pageIndex).toBe(1);
    expect(state.cursorStack).toEqual(['']);
    // The first page omits the cursor entirely ("" = first page).
    expect(lastRequest(client).cursor ?? '').toBe('');
  });
});

describe('ObservabilityPageState — authority posture', () => {
  it('flags the elevated scope for an admin claim and never for an ordinary caller', async () => {
    seedConnection('admin');
    const adminState = new ObservabilityPageState();
    adminState.load(fakeClient());
    await settle(adminState, 'ready');
    expect(adminState.canWiden).toBe(true);

    seedConnection('tenant.user');
    const ordinaryState = new ObservabilityPageState();
    ordinaryState.load(fakeClient());
    await settle(ordinaryState, 'ready');
    expect(ordinaryState.canWiden).toBe(false);
  });

  it('never sends a tenant/user/session filter even for an elevated caller', async () => {
    seedConnection('admin,console:fleet');
    const client = fakeClient();
    const state = new ObservabilityPageState();
    state.load(client);
    await settle(state, 'ready');
    const req = lastRequest(client);
    expect(req.filters?.tenant_ids ?? []).toHaveLength(0);
    expect(req.filters?.user_ids ?? []).toHaveLength(0);
    expect(req.filters?.session_ids ?? []).toHaveLength(0);
  });
});
