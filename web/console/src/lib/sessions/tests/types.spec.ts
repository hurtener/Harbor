// Harbor Console — Sessions wire-type mirror tests (Phase 245 / D-424,
// HA-63 consumer).
//
// Pins the Console's hand-authored mirror of the closed `full|lifecycle`
// session projection and the explicit
// `current|partial|not_requested|unavailable` counter-availability marker
// against the Go contract (`internal/protocol/types/sessions.go`): the
// closed sets, the omitted-projection default compatibility, and the
// availability predicates the Sessions page renders through (a zero
// counter never reads as a measured zero).

import { describe, expect, it } from 'vitest';

import {
  COUNTER_STATUSES,
  counterAvailability,
  countersAreAbsent,
  countersArePartial,
  SESSION_PROJECTIONS,
  type CounterStatus,
  type SessionRow,
  type SessionProjection,
  type SessionsListRequest
} from '../types.js';

describe('SessionProjection — the closed full|lifecycle set (D-424)', () => {
  it('mirrors the two canonical Go projections exactly', () => {
    expect(SESSION_PROJECTIONS).toEqual(['full', 'lifecycle']);
  });

  it('the page request type accepts exactly the two closed values', () => {
    const full: SessionProjection = 'full';
    const lifecycle: SessionProjection = 'lifecycle';
    expect([full, lifecycle]).toEqual(SESSION_PROJECTIONS);
  });

  it('an omitted projection is default-compatible: the field is optional', () => {
    // The pre-D-424 request shape must keep type-checking without the
    // field — an omitted `projection` resolves to `full` at the method
    // edge (mirrors `IsValidSessionProjection`'s empty→full resolution).
    const omitted: SessionsListRequest = { filter: {}, limit: 50 };
    expect(omitted.projection).toBeUndefined();
    expect('projection' in omitted).toBe(false);
  });

  it('an explicit lifecycle projection is carried on the request', () => {
    const lifecycle: SessionsListRequest = { filter: {}, projection: 'lifecycle' };
    expect(lifecycle.projection).toBe('lifecycle');
  });
});

describe('CounterStatus — the explicit availability marker (D-424)', () => {
  it('mirrors the four canonical Go availability states exactly', () => {
    expect(COUNTER_STATUSES).toEqual(['current', 'partial', 'not_requested', 'unavailable']);
  });

  it('the four states are mutually distinct', () => {
    expect(new Set<CounterStatus>(COUNTER_STATUSES).size).toBe(4);
  });
});

describe('counterAvailability — the effective four-state marker', () => {
  it('an explicit counter_status wins over the legacy partial flag', () => {
    const row = { counter_status: 'not_requested' as CounterStatus, counters_partial: true };
    expect(counterAvailability(row)).toBe('not_requested');
  });

  it('a row with no marker and no partial flag defaults to current (pre-D-424 behavior)', () => {
    expect(counterAvailability({})).toBe('current');
  });

  it('a row with only the legacy counters_partial flag falls back to partial', () => {
    expect(counterAvailability({ counters_partial: true })).toBe('partial');
  });

  it('a row with an explicit unavailable marker is honored', () => {
    expect(counterAvailability({ counter_status: 'unavailable' })).toBe('unavailable');
  });
});

describe('availability predicates — the honest-zero contract', () => {
  it('countersAreAbsent covers not_requested + unavailable (zeros mean "not computed")', () => {
    expect(countersAreAbsent('not_requested')).toBe(true);
    expect(countersAreAbsent('unavailable')).toBe(true);
    expect(countersAreAbsent('current')).toBe(false);
    expect(countersAreAbsent('partial')).toBe(false);
  });

  it('countersArePartial marks only the honest lower bound', () => {
    expect(countersArePartial('partial')).toBe(true);
    expect(countersArePartial('current')).toBe(false);
    expect(countersArePartial('not_requested')).toBe(false);
    expect(countersArePartial('unavailable')).toBe(false);
  });

  it('a not_requested SessionRow (lifecycle projection) never reads as a measured zero', () => {
    const lifecycleRow: SessionRow = {
      session_id: 's1',
      status: 'running',
      user_id: 'u1',
      tenant_id: 't1',
      started_at: '2026-08-01T10:00:00Z',
      last_activity_at: '2026-08-01T10:30:00Z',
      duration: 0,
      tasks_count: 0,
      events_count: 0,
      total_cost_cents: 0,
      total_tokens: 0,
      has_pending_intervention: false,
      has_failed_task: false,
      identity: { tenant: 't1', user: 'u1', session: 's1' },
      counter_status: 'not_requested'
    };
    const availability = counterAvailability(lifecycleRow);
    expect(availability).toBe('not_requested');
    expect(countersAreAbsent(availability)).toBe(true);
    expect(countersArePartial(availability)).toBe(false);
  });

  it('an unavailable row is absent too (no Enricher wired on this runtime)', () => {
    const unwiredRow: SessionRow = {
      session_id: 's2',
      status: 'completed',
      user_id: 'u1',
      tenant_id: 't1',
      started_at: '2026-08-01T10:00:00Z',
      last_activity_at: '2026-08-01T10:30:00Z',
      duration: 0,
      tasks_count: 0,
      events_count: 0,
      total_cost_cents: 0,
      total_tokens: 0,
      has_pending_intervention: false,
      has_failed_task: false,
      identity: { tenant: 't1', user: 'u1', session: 's2' },
      counter_status: 'unavailable'
    };
    expect(countersAreAbsent(counterAvailability(unwiredRow))).toBe(true);
  });
});
