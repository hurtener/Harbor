// Harbor Console — Agents page derive() projection unit tests
// (Phase 108l / D-184).
//
// Locks the pure projections against the real wire shapes the live probe
// captured. The load-bearing cases: the StatusChip mappings are exhaustive
// over the wire enums; the ready/empty split is derived LIVE from the loaded
// row count (the Events-page D-180 lesson); the activity projection filters by
// the payload's TitleCase `AgentID` (the untagged Go marshal) AND a snake_case
// alias, summarises each `agent.*` type honestly, and stays empty when quiet;
// the control result copy reports the ACTUAL re-read status (pause/restart
// stay `active` — the registry has no "paused" state — never a fabricated
// "paused", CLAUDE.md §13).

import { describe, it, expect } from 'vitest';
import {
  toPageError,
  healthKind,
  statusKind,
  displayStatus,
  extractAgentID,
  summarizeAgentEvent,
  projectActivity,
  controlResultMessage,
  isDestructiveVerb,
  AGENT_EVENT_TYPES
} from './derive.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { Event } from '$lib/protocol/events.js';

function ev(type: string, payload: unknown, at = '2026-06-03T12:00:00Z', seq = 1): Event {
  return { type, sequence: seq, occurred_at: at, tenant: 'dev', user: 'dev', session: 'dev', payload };
}

describe('healthKind', () => {
  it('maps each health badge onto the StatusChip scale', () => {
    expect(healthKind('Healthy')).toBe('success');
    expect(healthKind('Degraded')).toBe('warning');
    expect(healthKind('Force-Stopped')).toBe('danger');
    expect(healthKind('Paused')).toBe('accent');
    expect(healthKind('Drained')).toBe('accent');
    expect(healthKind('Unknown')).toBe('neutral');
  });
});

describe('statusKind', () => {
  it('maps each lifecycle status onto the StatusChip scale', () => {
    expect(statusKind('active')).toBe('success');
    expect(statusKind('paused')).toBe('accent');
    expect(statusKind('drained')).toBe('accent');
    expect(statusKind('force_stopped')).toBe('danger');
    expect(statusKind('deregistered')).toBe('neutral');
  });
});

describe('toPageError', () => {
  it('keeps a ProtocolError wire code (e.g. the not_found a deregistered id yields)', () => {
    const e = toPageError(new ProtocolError('not_found', 'agents.get: agent not found', 404));
    expect(e).toEqual({ code: 'not_found', message: 'agents.get: agent not found' });
  });

  it('collapses a plain Error onto runtime_error (no silent swallow)', () => {
    expect(toPageError(new Error('boom'))).toEqual({ code: 'runtime_error', message: 'boom' });
  });

  it('collapses a non-Error throw onto runtime_error', () => {
    expect(toPageError('weird')).toEqual({ code: 'runtime_error', message: 'unknown error' });
  });
});

describe('displayStatus', () => {
  it('passes loading / error / disconnected straight through', () => {
    expect(displayStatus('loading', 0)).toBe('loading');
    expect(displayStatus('error', 7)).toBe('error');
    expect(displayStatus('disconnected', 0)).toBe('disconnected');
  });

  it('derives ready when the loaded page has rows, empty when a filter drops them', () => {
    expect(displayStatus('ready', 3)).toBe('ready');
    expect(displayStatus('empty', 3)).toBe('ready');
    expect(displayStatus('ready', 0)).toBe('empty');
    expect(displayStatus('empty', 0)).toBe('empty');
  });
});

describe('extractAgentID', () => {
  it('reads the untagged Go marshal field (AgentID)', () => {
    expect(extractAgentID({ AgentID: 'agent-1', Command: 'pause' })).toBe('agent-1');
  });

  it('reads a snake_case alias defensively (future tagged generator)', () => {
    expect(extractAgentID({ agent_id: 'agent-2' })).toBe('agent-2');
  });

  it('returns empty for a missing / non-string / non-object payload', () => {
    expect(extractAgentID({ Command: 'pause' })).toBe('');
    expect(extractAgentID(null)).toBe('');
    expect(extractAgentID('nope')).toBe('');
    expect(extractAgentID({ AgentID: 42 })).toBe('');
  });
});

describe('summarizeAgentEvent', () => {
  it('summarises lifecycle events from their payloads', () => {
    expect(summarizeAgentEvent(ev('agent.registered', { AgentID: 'a', Incarnation: 1 }))).toBe(
      'Registered (incarnation 1)'
    );
    expect(
      summarizeAgentEvent(ev('agent.restarted', { AgentID: 'a', Incarnation: 2, VersionHashChanged: true }))
    ).toBe('Restarted (incarnation 2) — version changed');
    expect(summarizeAgentEvent(ev('agent.health', { AgentID: 'a', Health: 'degraded' }))).toBe(
      'Health reported: degraded'
    );
    expect(summarizeAgentEvent(ev('agent.deregistered', { AgentID: 'a' }))).toBe(
      'Deregistered — record removed'
    );
  });

  it('appends the audit-redacted reason to control summaries when present', () => {
    expect(summarizeAgentEvent(ev('agent.drained', { AgentID: 'a', Command: 'drain', Reason: 'maintenance' }))).toBe(
      'Drain issued: maintenance'
    );
    expect(summarizeAgentEvent(ev('agent.paused', { AgentID: 'a', Command: 'pause' }))).toBe('Pause issued');
    expect(summarizeAgentEvent(ev('agent.restart_requested', { AgentID: 'a' }))).toBe('Restart requested');
    expect(summarizeAgentEvent(ev('agent.force_stopped', { AgentID: 'a', Reason: 'runaway' }))).toBe(
      'Force-stop issued: runaway'
    );
  });
});

describe('projectActivity', () => {
  const events: Event[] = [
    ev('agent.paused', { AgentID: 'a', Command: 'pause' }, '2026-06-03T12:02:00Z', 3),
    ev('agent.health', { AgentID: 'b', Health: 'healthy' }, '2026-06-03T12:01:00Z', 2),
    ev('agent.registered', { AgentID: 'a', Incarnation: 1 }, '2026-06-03T12:00:00Z', 1)
  ];

  it('filters to the target agent, newest-first, preserving order', () => {
    const rows = projectActivity(events, 'a');
    expect(rows.map((r) => r.type)).toEqual(['agent.paused', 'agent.registered']);
    expect(rows[0].at).toBe('2026-06-03T12:02:00Z');
    expect(rows[0].summary).toBe('Pause issued');
  });

  it('is honestly empty when quiet or when the id is empty (no fabricated backlog)', () => {
    expect(projectActivity([], 'a')).toEqual([]);
    expect(projectActivity(events, '')).toEqual([]);
    expect(projectActivity(events, 'zzz')).toEqual([]);
  });
});

describe('controlResultMessage (honest re-read status — CLAUDE.md §13)', () => {
  it('reports pause/restart leave the observable status active (no "paused" state)', () => {
    expect(controlResultMessage('pause', 'active')).toContain('Observable status: active');
    expect(controlResultMessage('pause', 'active')).toContain('no "paused" state');
    expect(controlResultMessage('restart', 'active')).toContain('Observable status: active');
  });

  it('reports drain/force_stop transition the status', () => {
    expect(controlResultMessage('drain', 'drained')).toBe('Drain issued — status is now drained.');
    expect(controlResultMessage('force_stop', 'force_stopped')).toBe(
      'Force-stop issued — status is now force_stopped.'
    );
  });

  it('reports deregister removed the record', () => {
    expect(controlResultMessage('deregister', 'deregistered')).toContain('removed from the registry');
  });
});

describe('verb metadata', () => {
  it('gates force_stop + deregister behind a confirm', () => {
    expect(isDestructiveVerb('force_stop')).toBe(true);
    expect(isDestructiveVerb('deregister')).toBe(true);
    expect(isDestructiveVerb('pause')).toBe(false);
  });

  it('enumerates the eight agent.* subscription types', () => {
    expect(AGENT_EVENT_TYPES).toHaveLength(8);
    expect(AGENT_EVENT_TYPES).toContain('agent.paused');
    expect(AGENT_EVENT_TYPES).toContain('agent.deregistered');
  });
});
