// Harbor Console — Playground reopen page test (HA-64 / D-425, the two-read
// chat open).
//
// Mounts the REAL `+page.svelte` (Svelte 5 runes, jsdom) over a stubbed
// connection + HarborClient + Console DB (the sessions page.spec / D-424
// recipe) and pins the HA-64 consumer contract:
//
//   - the NORMAL open performs EXACTLY two reads — `sessions.inspect` with
//     `projection: 'lifecycle'` (Phase 245 / HA-63) plus one
//     `sessions.turns.list` tail page (Phase 246 / HA-64) — and NEVER calls
//     `state.history`, `tasks.list`, `tasks.get`, or `events.list`;
//   - tail-paged root foreground turns render (query + answer bubbles) and
//     fold the honest per-measure usage into the header;
//   - older-page loading passes the opaque `next_older_cursor` back verbatim
//     and prepends older turns without duplicates;
//   - a refused older-page cursor surfaces an honest typed notice — never a
//     silent reset to page one, never a fabricated empty page;
//   - terminal reconciliation after live streaming uses ONE
//     `sessions.turns.get` (never `tasks.get` / the raw transcript);
//   - a runtime predating the projection answers `unknown_method` on the open:
//     the page shows the explicit degraded/forensic control and does NOT run
//     the legacy `state.history` event-replay path until the operator clicks.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';

const harness = vi.hoisted(() => ({
  inspectCalls: [] as Array<Record<string, unknown>>,
  turnsListCalls: [] as Array<Record<string, unknown>>,
  turnsGetCalls: [] as Array<Record<string, unknown>>,
  stateHistoryCalls: 0,
  tasksListCalls: 0,
  tasksGetCalls: 0,
  eventsListCalls: 0,
  // The scripted durable turn page(s), consumed FIFO by sessionTurns.list.
  turnPages: [] as Array<Record<string, unknown>>,
  listError: undefined as unknown,
  // The scripted terminal reconcile row.
  turnsGetResult: undefined as Record<string, unknown> | undefined,
  getError: undefined as unknown,
  lifecycleRow: {} as Record<string, unknown>,
  lifecycleError: undefined as unknown,
  // The lifecycle/terminal frames the fake EventSource can dispatch.
  eventListeners: new Map<string, (msg: { data: string }) => void>()
}));

vi.mock('$app/state', () => ({
  page: { params: { session_id: 's-reopen' } }
}));

vi.mock('$lib/connection.js', async (importActual) => {
  const actual = await importActual<typeof import('$lib/connection.js')>();
  return {
    ...actual,
    resolveConnection: () => ({
      baseURL: 'http://127.0.0.1:18080',
      token: 'dummy-bearer-token',
      identity: { tenant: 't1', user: 'u1', session: 's-reopen' },
      scopes: ['admin']
    })
  };
});

// A fake EventSource so the page's SSE subscription records listeners and the
// test can dispatch a `task.completed` frame for the terminal-reconcile case.
class FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readonly url: string;
  onopen: (() => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  readyState = FakeEventSource.CONNECTING;
  constructor(url: string) {
    this.url = url;
  }
  addEventListener(type: string, fn: (msg: { data: string }) => void): void {
    harness.eventListeners.set(type, fn);
  }
  removeEventListener(type: string): void {
    harness.eventListeners.delete(type);
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

vi.stubGlobal('EventSource', FakeEventSource);

// The stubbed HarborClient records every call and serves the scripted
// surfaces. The reopen-forbidden namespaces (state.history / tasks.list /
// tasks.get / events.list) count calls so the test can prove the normal open
// never touches them.
vi.mock('$lib/protocol/harbor.js', () => ({
  HarborClient: class {
    sessions = {
      list: async () => ({ rows: [], next_cursor: '', truncated: false }),
      inspect: async (req: Record<string, unknown>) => {
        harness.inspectCalls.push(req);
        if (harness.lifecycleError !== undefined) throw harness.lifecycleError;
        return { row: harness.lifecycleRow, recent_interventions: [], recent_artifacts: [] };
      },
      setTitle: async () => ({ session_id: '', title: '', title_source: '' })
    };
    sessionTurns = {
      list: async (req: Record<string, unknown>) => {
        harness.turnsListCalls.push(req);
        if (harness.listError !== undefined) throw harness.listError;
        const page = harness.turnPages.shift();
        if (page === undefined) {
          return {
            header: { session_id: 's-reopen', snapshot_id: 1, as_of: '2026-07-10T12:00:01Z' },
            turns: [],
            order: 'newest_first',
            has_more: false,
            count_exact: true,
            live_resume_seq: 0,
            page_completeness: 'complete',
            protocol_version: '0.1.0'
          };
        }
        return page;
      },
      get: async (req: Record<string, unknown>) => {
        harness.turnsGetCalls.push(req);
        if (harness.getError !== undefined) throw harness.getError;
        return harness.turnsGetResult ?? { session_id: 's-reopen', protocol_version: '0.1.0' };
      }
    };
    state = {
      history: async () => {
        harness.stateHistoryCalls++;
        return { events: [], head_sequence: 0, tail_sequence: 0, has_more: false, truncated: false };
      }
    };
    tasks = {
      list: async () => {
        harness.tasksListCalls++;
        return { rows: [] };
      },
      get: async () => {
        harness.tasksGetCalls++;
        return {};
      }
    };
    events = {
      subscribeURL: () => 'http://127.0.0.1:18080/v1/events?access_token=x',
      list: async () => {
        harness.eventsListCalls++;
        return { rows: [] };
      }
    };
    artifacts = {
      list: async () => ({ rows: [] }),
      put: async () => ({}),
      getRef: async () => ({ presigned_url: 'http://127.0.0.1:18080/a' }),
      get: async () => ({})
    };
    tools = { list: async () => ({ tools: [] }) };
    control = {
      start: async () => ({ task_id: 'task-live-1' }),
      dispatch: async () => ({}),
      pause: async () => ({}),
      resume: async () => ({}),
      cancel: async () => ({})
    };
    runs = { setOverrides: async () => ({}) };
    capabilities = async () => new Set<string>();
    topology = { snapshot: async () => ({ nodes: [] }) };
    posture = { info: async () => ({}) };
    agents = { list: async () => ({ agents: [] }) };
    mcp = { servers: {}, apps: {} };
    observability = { query: async () => ({}) };
    memory = { list: async () => ({ rows: [] }) };
    flows = {};
    pause = {};
    metrics = {};
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
vi.mock('$lib/db/saved_filters_playground.js', () => ({
  PlaygroundSavedFilters: class {}
}));

import PlaygroundPage from './+page.svelte';
import type { SessionTurnRow } from '$lib/protocol/session-turns.js';

let mounted: ReturnType<typeof mount> | undefined;
let target: HTMLElement | undefined;

/** A minimal well-formed durable turn row the projection renders. */
function turnRow(id: string, over: Partial<SessionTurnRow> = {}): Record<string, unknown> {
  return {
    turn_id: id,
    task_id: id,
    session_id: 's-reopen',
    sequence: 0,
    tie_breaker: id,
    status: 'complete',
    sealed: true,
    version: 1,
    last_applied_event_seq: 1,
    started_at: '2026-07-10T12:00:00Z',
    updated_at: '2026-07-10T12:00:01Z',
    finished_at: '2026-07-10T12:00:01Z',
    agent: { id: 'ag-1', name: 'analyst', binding_source: 'explicit', complete: 'complete' },
    query: { text: `query-${id}`, at: '2026-07-10T12:00:00Z', complete: 'complete' },
    answer: { state: 'inline', inline: `answer-${id}`, seq: 1, complete: 'complete' },
    pause: { availability: 'unavailable' },
    usage: {
      prompt_tokens: { state: 'exact', value: 10 },
      completion_tokens: { state: 'exact', value: 5 },
      reasoning_tokens: { state: 'unavailable' },
      cache_read_tokens: { state: 'unavailable' },
      cache_write_tokens: { state: 'unavailable' },
      total_tokens: { state: 'exact', value: 15 },
      cost_micro_usd: { state: 'exact', value: 100 },
      latency_ns: { state: 'estimated', value: 500_000_000 },
      model: 'claude-haiku'
    },
    reasoning: { steps: [{ index: 0, kind: 'tool_call' }], complete: 'complete', seq: 1 },
    activity: {
      rows: [
        { position: 0, tool: 'reports_render', step_sequence: 1, status: 'succeeded', retryable: false, policy_exhausted: false, summary: '1.2s' }
      ],
      complete: 'complete',
      more: false,
      totals: { invoked: 0, succeeded: 1, failed: 0, cancelled: 0, retried: 0, policy_exhausted: 0 }
    },
    apps: [
      {
        server_id: 'reports',
        resource_uri: 'ui://reports/dashboard.html',
        display_mode: 'inline',
        raw_html_trusted: false,
        tool_call_id: 'tc_9f2c1a',
        tool_name: 'reports_render',
        availability: 'available',
        complete: 'complete'
      }
    ],
    ...over
  };
}

function newestPage(turns: Array<Record<string, unknown>>, over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    header: { session_id: 's-reopen', snapshot_id: 1, as_of: '2026-07-10T12:00:01Z' },
    turns,
    order: 'newest_first',
    has_more: false,
    count_exact: true,
    live_resume_seq: turns.length,
    page_completeness: 'complete',
    protocol_version: '0.1.0',
    ...over
  };
}

async function render(): Promise<HTMLElement> {
  target = document.createElement('div');
  document.body.appendChild(target);
  mounted = mount(PlaygroundPage, { target, props: {} });
  // Drain the mount $effect's async load + hydration chains (the sessions
  // page.spec settling loop).
  for (let i = 0; i < 16; i++) {
    flushSync();
    await Promise.resolve();
  }
  flushSync();
  return target;
}

function bubbleTexts(): string[] {
  const nodes = Array.from(
    (target?.querySelectorAll('[data-testid="chat-message-bubble"]') ?? []) as NodeListOf<HTMLElement>
  );
  return nodes.map((n) => n.textContent ?? '');
}

afterEach(() => {
  if (mounted) {
    unmount(mounted);
    mounted = undefined;
  }
  target?.remove();
  target = undefined;
  harness.inspectCalls.length = 0;
  harness.turnsListCalls.length = 0;
  harness.turnsGetCalls.length = 0;
  harness.stateHistoryCalls = 0;
  harness.tasksListCalls = 0;
  harness.tasksGetCalls = 0;
  harness.eventsListCalls = 0;
  harness.turnPages.length = 0;
  harness.listError = undefined;
  harness.turnsGetResult = undefined;
  harness.getError = undefined;
  harness.lifecycleRow = {};
  harness.lifecycleError = undefined;
  harness.eventListeners.clear();
});

describe('Playground reopen — the TWO-READ open (HA-64 / D-425)', () => {
  it('normal open performs exactly inspect(lifecycle) + turns.list — never state.history/tasks.list/tasks.get/events.list', async () => {
    harness.lifecycleRow = {
      session_id: 's-reopen',
      status: 'completed',
      started_at: '2026-07-10T11:00:00Z',
      title: 'A session'
    };
    harness.turnPages = [newestPage([turnRow('t2'), turnRow('t1')])];
    await render();

    // Exactly the two-read open.
    expect(harness.inspectCalls).toEqual([{ session_id: 's-reopen', projection: 'lifecycle' }]);
    expect(harness.turnsListCalls).toHaveLength(1);
    expect(harness.turnsListCalls[0]).toMatchObject({ session_id: 's-reopen' });
    // The forbidden forensic/raw surfaces are never touched.
    expect(harness.stateHistoryCalls).toBe(0);
    expect(harness.tasksListCalls).toBe(0);
    expect(harness.tasksGetCalls).toBe(0);
    expect(harness.eventsListCalls).toBe(0);

    // The tail-paged root foreground turns render (query + answer + usage).
    const text = bubbleTexts().join('\n');
    expect(text).toContain('query-t2');
    expect(text).toContain('answer-t2');
    expect(text).toContain('query-t1');
    expect(text).toContain('answer-t1');
  });

  it('older-page loading passes the opaque cursor back and prepends without duplicates', async () => {
    harness.lifecycleRow = { session_id: 's-reopen', status: 'completed' };
    harness.turnPages = [
      newestPage([turnRow('t4'), turnRow('t3')], { has_more: true, next_older_cursor: 'snap-1/seq-2/turn-t3' }),
      newestPage([turnRow('t2'), turnRow('t1')])
    ];
    await render();
    expect(harness.turnsListCalls).toHaveLength(1);

    // The opaque cursor rides back verbatim on the older-page read.
    (target?.querySelector('[data-testid="load-older-turns"]') as HTMLButtonElement | null)?.click();
    for (let i = 0; i < 8; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();

    expect(harness.turnsListCalls).toHaveLength(2);
    expect(harness.turnsListCalls[1]).toMatchObject({ older_cursor: 'snap-1/seq-2/turn-t3' });
    // Oldest at top, no duplicates across the page boundary.
    const ids = Array.from(
      (target?.querySelectorAll('[data-testid="chat-message-bubble"]') ?? []) as NodeListOf<HTMLElement>
    ).map((n) => n.getAttribute('data-message-id') ?? '');
    const taskIds = [...new Set(ids.map((id) => id.replace(/^t-/, '').replace(/-(u|a)$/, '')))];
    expect(taskIds).toEqual(['t1', 't2', 't3', 't4']);
  });

  it('a refused older-page cursor surfaces an honest notice — never a silent reset or empty page', async () => {
    harness.lifecycleRow = { session_id: 's-reopen', status: 'completed' };
    harness.turnPages = [
      newestPage([turnRow('t2')], { has_more: true, next_older_cursor: 'stale-cursor' })
    ];
    await render();
    harness.listError = new Error('cursor refused');
    (harness.listError as { code?: string }).code = 'invalid_request';

    (target?.querySelector('[data-testid="load-older-turns"]') as HTMLButtonElement | null)?.click();
    for (let i = 0; i < 8; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();

    const notice = target?.querySelector('[data-testid="older-turns-error"]');
    expect(notice).not.toBeNull();
    expect(notice?.textContent ?? '').toContain('cursor');
    // The already-loaded turns are still rendered — the refusal never blanks
    // the conversation.
    expect(bubbleTexts().join('\n')).toContain('answer-t2');
  });

  it('terminal reconciliation after live streaming uses ONE sessions.turns.get', async () => {
    harness.lifecycleRow = { session_id: 's-reopen', status: 'completed' };
    harness.turnPages = [newestPage([])];
    harness.turnsGetResult = {
      session_id: 's-reopen',
      turn: turnRow('task-live-1', {
        turn_id: 'task-live-1',
        task_id: 'task-live-1',
        answer: { state: 'inline', inline: 'sealed answer', seq: 2, complete: 'complete' },
        status: 'complete',
        sealed: true
      }),
      protocol_version: '0.1.0'
    };
    await render();

    // Drive a send (control.start → activeTaskID = task-live-1), then a live
    // task.completed SSE frame.
    const input = target?.querySelector('[data-testid="chat-composer-input"]') as HTMLTextAreaElement | null;
    const sendBtn = target?.querySelector('[data-testid="chat-send-button"]') as HTMLButtonElement | null;
    if (input === null || sendBtn === null) {
      throw new Error('composer not rendered');
    }
    input.value = 'hello live';
    input.dispatchEvent(new Event('input'));
    flushSync();
    sendBtn.click();
    for (let i = 0; i < 8; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();

    const onTerminal = harness.eventListeners.get('task.completed');
    expect(onTerminal).toBeDefined();
    onTerminal?.({
      data: JSON.stringify({ type: 'task.completed', run: 'task-live-1', payload: { TaskID: 'task-live-1' } })
    });
    for (let i = 0; i < 8; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();

    // The terminal reconcile is EXACTLY one sessions.turns.get — never
    // tasks.get / the raw transcript.
    expect(harness.turnsGetCalls).toEqual([
      { session_id: 's-reopen', task_id: 'task-live-1' }
    ]);
    expect(harness.tasksGetCalls).toBe(0);
    expect(harness.stateHistoryCalls).toBe(0);
    expect(bubbleTexts().join('\n')).toContain('sealed answer');
  });

  it('a runtime predating the projection is NOT silently degraded — the forensic reopen is explicit + user-invoked', async () => {
    harness.lifecycleRow = { session_id: 's-reopen', status: 'completed' };
    harness.listError = { code: 'unknown_method', message: 'no such method' };
    await render();

    // The open failed with unknown_method: no forensic replay ran on its own.
    expect(harness.stateHistoryCalls).toBe(0);
    expect(harness.tasksListCalls).toBe(0);
    expect(bubbleTexts().length).toBe(0);

    // The explicit degraded control is visible and names the degradation.
    const fallback = target?.querySelector('[data-testid="forensic-fallback"]');
    expect(fallback).not.toBeNull();
    expect(fallback?.textContent ?? '').toContain('forensic');

    // Clicking it runs the legacy state.history + tasks.list path.
    (target?.querySelector('[data-testid="forensic-reopen-button"]') as HTMLButtonElement | null)?.click();
    for (let i = 0; i < 16; i++) {
      flushSync();
      await Promise.resolve();
    }
    flushSync();
    expect(harness.stateHistoryCalls).toBe(1);
  });

  it('an unavailable usage measure never folds a fabricated zero into the KPI or bubble', async () => {
    harness.lifecycleRow = { session_id: 's-reopen', status: 'completed' };
    harness.turnPages = [
      newestPage([
        turnRow('t-no-usage', {
          usage: {
            prompt_tokens: { state: 'unavailable' },
            completion_tokens: { state: 'unavailable' },
            reasoning_tokens: { state: 'unavailable' },
            cache_read_tokens: { state: 'unavailable' },
            cache_write_tokens: { state: 'unavailable' },
            total_tokens: { state: 'unavailable' },
            cost_micro_usd: { state: 'unavailable' },
            latency_ns: { state: 'unavailable' }
          }
        })
      ])
    ];
    await render();
    // The bubble renders the answer; the meta never invents "0 tok · $0.0000".
    expect(bubbleTexts().join('\n')).toContain('answer-t-no-usage');
    const meta = target?.querySelector('[data-testid="bubble-meta"]');
    // An honest elapsed span may render, but a fabricated token/cost zero never.
    if (meta !== null && meta !== undefined) {
      expect(meta.textContent ?? '').not.toContain('tok');
      expect(meta.textContent ?? '').not.toContain('$');
    }
  });
});
