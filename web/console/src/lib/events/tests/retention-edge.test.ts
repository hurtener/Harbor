/**
 * Events window-edge honesty tests (D-296).
 *
 * Pins the pure compare `windowEdgeNotice` (banner fires when the picked
 * window reaches before the observed events retention horizon, hidden
 * otherwise) and the `eventsHorizonFrom` projection over a
 * `runtime.health` response.
 */
import { describe, expect, it } from 'vitest';
import { windowEdgeNotice, eventsHorizonFrom } from '../state.svelte.js';
import type { RuntimeHealth } from '$lib/protocol/posture.js';

// A fixed "now" for deterministic window-start math.
const NOW = Date.parse('2026-07-10T00:00:00.000Z');

describe('windowEdgeNotice', () => {
  it('hides when the horizon is unknown', () => {
    expect(windowEdgeNotice('7d', null, NOW)).toBeNull();
    expect(windowEdgeNotice('7d', '', NOW)).toBeNull();
  });

  it('hides when the whole window sits within retention', () => {
    // Horizon 30 days ago; a 7d window starts 7 days ago — within.
    const horizon = new Date(NOW - 30 * 86_400_000).toISOString();
    expect(windowEdgeNotice('7d', horizon, NOW)).toBeNull();
  });

  it('fires when the window reaches before the horizon', () => {
    // Horizon 2 days ago; a 7d window starts 7 days ago — predates it.
    const horizon = new Date(NOW - 2 * 86_400_000).toISOString();
    const notice = windowEdgeNotice('7d', horizon, NOW);
    expect(notice).not.toBeNull();
    expect(notice).toContain('retains events only back to');
  });

  it('hides for a short window even when the horizon is recent', () => {
    // Horizon 2 days ago; a 5m window is well within it.
    const horizon = new Date(NOW - 2 * 86_400_000).toISOString();
    expect(windowEdgeNotice('5m', horizon, NOW)).toBeNull();
  });

  it('treats a malformed horizon as unknown', () => {
    expect(windowEdgeNotice('7d', 'not-a-timestamp', NOW)).toBeNull();
  });
});

describe('eventsHorizonFrom', () => {
  it('returns null when the runtime reports no retention block', () => {
    expect(eventsHorizonFrom(null)).toBeNull();
    expect(eventsHorizonFrom({ subsystems: [] })).toBeNull();
  });

  it('extracts the events entry, ignoring other surfaces', () => {
    const health: RuntimeHealth = {
      subsystems: [],
      retention: [
        { surface: 'tasks', oldest_retained_at: '2026-07-01T00:00:00Z' },
        { surface: 'events', oldest_retained_at: '2026-07-08T00:00:00Z' }
      ]
    };
    expect(eventsHorizonFrom(health)).toBe('2026-07-08T00:00:00Z');
  });

  it('returns null when the events entry omits the timestamp', () => {
    const health: RuntimeHealth = {
      subsystems: [],
      retention: [{ surface: 'events' }]
    };
    expect(eventsHorizonFrom(health)).toBeNull();
  });
});
