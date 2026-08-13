// Harbor Console — Sessions list page test (Phase 245 / D-424, HA-63
// consumer).
//
// Mounts the REAL `+page.svelte` (Svelte 5 runes, jsdom) over a stubbed
// connection + HarborClient + Console DB (the AppStatusBar.test.ts
// recipe: real component, stubbed seams) and asserts the page's D-424
// behaviors:
//
//   - the catalog request explicitly carries `projection: 'full'` — the
//     page's Events / Cost columns and the `cost_desc` sort depend on the
//     read-time counters (the chat-catalog consumer requests `lifecycle`
//     instead);
//   - counter absence renders honestly: an `unavailable` / `not_requested`
//     row's zero cost never reads as a measured "$0.00" (a dash, with a
//     naming title);
//   - a `partial` row keeps the "≥" honest lower-bound affordance;
//   - a `current` measured zero stays "$0.00" (that one IS a measured
//     zero);
//   - a pre-D-424 row (no `counter_status` marker) falls back to the
//     legacy `counters_partial` flag.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

// The stubbed HarborClient records every sessions.list request body and
// serves the scripted rows (hoisted so the vi.mock factory can close over
// it — AppStatusBar.test.ts precedent).
const harness = vi.hoisted(() => ({
  listCalls: [] as Array<Record<string, unknown>>,
  rows: [] as Array<Record<string, unknown>>
}));

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));

vi.mock('$lib/connection.js', async (importActual) => {
  const actual = await importActual<typeof import('$lib/connection.js')>();
  return {
    ...actual,
    resolveConnection: () => ({
      baseURL: 'http://127.0.0.1:18080',
      token: 'dummy-bearer-token',
      identity: { tenant: 't1', user: 'u1', session: 's1' },
      scopes: ['admin']
    })
  };
});

// Stub the runtime client: sessions.list records the request and serves
// the scripted rows; the best-effort per-row enrichment legs (events
// aggregate + session-scoped tasks.list) return empty buckets/rows so the
// table renders without further network.
vi.mock('$lib/protocol/harbor.js', () => ({
  HarborClient: class {
    sessions = {
      list: async (req: Record<string, unknown>) => {
        harness.listCalls.push(req);
        return { rows: harness.rows, next_cursor: '', truncated: false };
      },
      inspect: async () => ({ row: {}, recent_interventions: [], recent_artifacts: [] }),
      setTitle: async () => ({ session_id: '', title: '', title_source: '' })
    };
    events = { aggregate: async () => ({ buckets: [] }) };
    tasks = { list: async () => ({ rows: [] }) };
    control = { cancel: async () => {}, pause: async () => {} };
    capabilities = async () => new Set<string>();
  }
}));

// The saved-view chips are Console-DB-backed; make the DB open fail so the
// page falls back to the empty-chips catch path it already carries.
vi.mock('$lib/db/console_db.js', () => ({
  openListPageDB: async () => {
    throw new Error('no db in test');
  }
}));
vi.mock('$lib/db/schema.js', () => ({
  operatorIdOf: async () => 'op-test'
}));
vi.mock('$lib/db/saved_filters_sessions.js', () => ({
  SessionsSavedFilters: class {}
}));

import SessionsPage from './+page.svelte';

let mounted: ReturnType<typeof mount> | undefined;
let target: HTMLElement | undefined;

/** A minimal well-formed catalog row the page renders. */
function row(id: string, overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    session_id: id,
    status: 'running',
    user_id: 'u1',
    tenant_id: 't1',
    started_at: '2026-08-01T10:00:00.000Z',
    last_activity_at: '2026-08-01T10:30:00.000Z',
    duration: 0,
    tasks_count: 0,
    events_count: 0,
    total_cost_cents: 0,
    total_tokens: 0,
    has_pending_intervention: false,
    has_failed_task: false,
    identity: { tenant: 't1', user: 'u1', session: id },
    ...overrides
  };
}

async function render(): Promise<HTMLElement> {
  target = document.createElement('div');
  document.body.appendChild(target);
  mounted = mount(SessionsPage, { target, props: {} });
  // Drain the mount $effect's async loadCatalog + best-effort enrichment
  // chains (the mcp-app-replay spec's settling loop).
  for (let i = 0; i < 12; i++) {
    flushSync();
    await Promise.resolve();
  }
  flushSync();
  return target;
}

/** The events/cost cells of the row whose session-id span carries `id`. */
function rowCells(id: string): { events: HTMLElement | null; cost: HTMLElement | null } {
  const span = target?.querySelector(`[data-session-id="${id}"]`);
  const tr = span?.closest('tr');
  return {
    events: tr?.querySelector('[data-testid="row-events"]') ?? null,
    cost: tr?.querySelector('[data-testid="row-cost"]') ?? null
  };
}

afterEach(() => {
  if (mounted) {
    unmount(mounted);
    mounted = undefined;
  }
  target?.remove();
  target = undefined;
  harness.listCalls.length = 0;
  harness.rows.length = 0;
});

describe('Sessions list page — D-424 projection + honest counter rendering', () => {
  it('requests the FULL projection (its counter columns and cost_desc sort depend on it)', async () => {
    harness.rows = [row('s1')];
    await render();
    expect(harness.listCalls.length).toBeGreaterThan(0);
    expect(harness.listCalls[0].projection).toBe('full');
  });

  it('renders an unavailable row\u2019s counter absence honestly — cost dash, never "$0.00"', async () => {
    harness.rows = [row('s-unavail', { counter_status: 'unavailable', total_cost_cents: 0 })];
    await render();
    const { cost } = rowCells('s-unavail');
    expect(cost?.textContent?.trim()).toBe('—');
    expect(cost?.getAttribute('title') ?? '').toContain('unavailable');
  });

  it('renders a not_requested row\u2019s counter absence honestly (defensive — the page never asks for lifecycle)', async () => {
    harness.rows = [row('s-nr', { counter_status: 'not_requested', total_cost_cents: 0 })];
    await render();
    expect(rowCells('s-nr').cost?.textContent?.trim()).toBe('—');
  });

  it('keeps the "≥" honest lower-bound affordance for a partial row', async () => {
    harness.rows = [
      row('s-partial', { counter_status: 'partial', total_cost_cents: 1234, events_count: 5 })
    ];
    await render();
    const { events, cost } = rowCells('s-partial');
    expect(cost?.textContent?.trim()).toBe('≥$12.34');
    expect(events?.textContent?.trim()).toBe('≥5');
  });

  it('renders a current measured zero honestly as "$0.00" (that zero IS measured)', async () => {
    harness.rows = [
      row('s-current', { counter_status: 'current', total_cost_cents: 0, events_count: 3 })
    ];
    await render();
    const { events, cost } = rowCells('s-current');
    expect(cost?.textContent?.trim()).toBe('$0.00');
    expect(events?.textContent?.trim()).toBe('3');
  });

  it('preserves the legacy counters_partial fallback for pre-D-424 rows (no counter_status marker)', async () => {
    harness.rows = [
      row('s-legacy', { counters_partial: true, total_cost_cents: 500, events_count: 7 })
    ];
    await render();
    const { events, cost } = rowCells('s-legacy');
    expect(cost?.textContent?.trim()).toBe('≥$5.00');
    expect(events?.textContent?.trim()).toBe('≥7');
  });
});
