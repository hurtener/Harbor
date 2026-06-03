// Harbor Console — Agents page pure projections (Phase 108l / D-184).
//
// The king-file refactor pulls every pure, side-effect-free projection the
// Agents list + detail pages render into this unit-testable module — the same
// split the Tools-108k / Background-Jobs-108j rebuilds used. Nothing here
// touches the Protocol client, `localStorage`, the DOM, or any reactive
// state; the `AgentsListPageState` / `AgentDetailPageState` controllers and
// the views import these helpers, and `derive.test.ts` pins them against the
// real wire shapes the live probe captured.
//
// No Svelte runes here (a plain `.ts` module); design-token concerns live in
// the `.svelte` views, not in these projections.

import type { StatusKind } from '$lib/components/ui/StatusChip.svelte';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { AgentHealth, AgentStatus } from '$lib/protocol/agents.js';
import type { Event } from '$lib/protocol/events.js';
import type { ActivityEntry } from '$lib/components/agents/AgentActivityFeed.svelte';

/** A page-friendly `{code, message}` projection of a thrown error. */
export type PageError = { code: string; message: string };

/**
 * Normalises any thrown value into a `{code, message}` the four-state
 * `<PageState>` boundary renders. A `ProtocolError` keeps its wire code
 * (`not_found` / `identity_scope_required` / …); anything else collapses to
 * `runtime_error` — never a silent swallow (CLAUDE.md §13).
 */
export function toPageError(err: unknown): PageError {
  if (err instanceof ProtocolError) {
    return { code: err.code, message: err.message };
  }
  return {
    code: 'runtime_error',
    message: err instanceof Error ? err.message : 'unknown error'
  };
}

/** Maps an agent's operational-health badge onto the shared StatusChip scale. */
export function healthKind(h: AgentHealth): StatusKind {
  switch (h) {
    case 'Healthy':
      return 'success';
    case 'Degraded':
      return 'warning';
    case 'Force-Stopped':
      return 'danger';
    case 'Paused':
    case 'Drained':
      return 'accent';
    default:
      return 'neutral';
  }
}

/** Maps an agent's lifecycle status onto the shared StatusChip scale. */
export function statusKind(s: AgentStatus): StatusKind {
  switch (s) {
    case 'active':
      return 'success';
    case 'paused':
    case 'drained':
      return 'accent';
    case 'force_stopped':
      return 'danger';
    case 'deregistered':
      return 'neutral';
    default:
      return 'neutral';
  }
}

/**
 * The status the list `<PageState>` boundary renders. `loading` / `error` /
 * `disconnected` pass through; the ready/empty split is derived LIVE from the
 * loaded-row count so a catalog whose rows all dropped under a tight facet
 * still reads "empty" (the Events-page D-180 lesson — derive the display
 * state, never a `status` set once at load that won't flip when data streams
 * in).
 */
export function displayStatus(status: PageStatus, rowCount: number): PageStatus {
  if (status === 'loading' || status === 'error' || status === 'disconnected') {
    return status;
  }
  return rowCount > 0 ? 'ready' : 'empty';
}

/* ================================================================ */
/* Activity feed — live `agent.*` event projection                  */
/* ================================================================ */

/**
 * The eight `agent.*` lifecycle + fleet-control event types the detail
 * activity feed subscribes to (mirrors `internal/runtime/registry/events.go`
 * + `events/taxonomy.ts`). Enumerated (not `[]`) so the named-SSE-frame
 * listeners register and the subscription URL stays bounded
 * (subscription.svelte.ts).
 */
export const AGENT_EVENT_TYPES: readonly string[] = [
  'agent.registered',
  'agent.restarted',
  'agent.health',
  'agent.drained',
  'agent.deregistered',
  'agent.paused',
  'agent.restart_requested',
  'agent.force_stopped'
] as const;

/** Reads the first present field by name off an unknown payload object. */
function payloadField(payload: unknown, ...names: string[]): unknown {
  if (typeof payload !== 'object' || payload === null) return undefined;
  const rec = payload as Record<string, unknown>;
  for (const n of names) {
    if (n in rec) return rec[n];
  }
  return undefined;
}

/**
 * Extracts the registration `agent_id` an `agent.*` event carries in its
 * payload (RFC §6.16 "Events"). The Go payloads marshal exported fields
 * untagged (`AgentID`); the snake_case alias is read defensively in case a
 * future generator emits tagged JSON.
 */
export function extractAgentID(payload: unknown): string {
  const v = payloadField(payload, 'AgentID', 'agent_id');
  return typeof v === 'string' ? v : '';
}

/**
 * A short, operator-facing one-line summary of one `agent.*` event, derived
 * from its payload. Honest — it reports only what the payload carries; it
 * never invents a status the registry did not emit (CLAUDE.md §13).
 */
export function summarizeAgentEvent(ev: Event): string {
  const p = ev.payload;
  switch (ev.type) {
    case 'agent.registered': {
      const inc = payloadField(p, 'Incarnation', 'incarnation');
      return typeof inc === 'number' ? `Registered (incarnation ${inc})` : 'Registered';
    }
    case 'agent.restarted': {
      const inc = payloadField(p, 'Incarnation', 'incarnation');
      const changed = payloadField(p, 'VersionHashChanged', 'version_hash_changed');
      const tail = changed === true ? ' — version changed' : '';
      return typeof inc === 'number'
        ? `Restarted (incarnation ${inc})${tail}`
        : `Restarted${tail}`;
    }
    case 'agent.health': {
      const h = payloadField(p, 'Health', 'health');
      return typeof h === 'string' ? `Health reported: ${h}` : 'Health reported';
    }
    case 'agent.drained':
      return reasonSuffix('Drain issued', p);
    case 'agent.paused':
      return reasonSuffix('Pause issued', p);
    case 'agent.restart_requested':
      return reasonSuffix('Restart requested', p);
    case 'agent.force_stopped':
      return reasonSuffix('Force-stop issued', p);
    case 'agent.deregistered':
      return 'Deregistered — record removed';
    default:
      return ev.type;
  }
}

/** Appends an audit-redacted reason to a control summary when present. */
function reasonSuffix(head: string, payload: unknown): string {
  const r = payloadField(payload, 'Reason', 'reason');
  return typeof r === 'string' && r.length > 0 ? `${head}: ${r}` : head;
}

/**
 * Projects the live SSE event page down to the activity rows for ONE agent —
 * the events whose payload `agent_id` matches, newest-first (the subscription
 * keeps its page newest-first). The runtime has no durable event read-back,
 * so this is a LIVE-only view: it is honestly empty until events stream in
 * (CLAUDE.md §13 — never a fabricated backlog).
 */
export function projectActivity(events: Event[], agentID: string): ActivityEntry[] {
  if (agentID === '') return [];
  const out: ActivityEntry[] = [];
  for (const ev of events) {
    if (extractAgentID(ev.payload) !== agentID) continue;
    out.push({
      sequence: ev.sequence,
      type: ev.type,
      at: ev.occurred_at,
      summary: summarizeAgentEvent(ev)
    });
  }
  return out;
}

/* ================================================================ */
/* Fleet control — honest result copy                               */
/* ================================================================ */

/** The five canonical fleet-control verbs (D-066 / D-184). */
export type ControlVerb = 'pause' | 'drain' | 'restart' | 'force_stop' | 'deregister';

/**
 * The honest result line a control call produces. It reports the ACTUAL
 * post-control status the response re-read server-side — never the command's
 * intent. The V1 registry has no "paused" Health, so pause/restart emit their
 * `agent.*` event but leave the observable status `active`; only
 * drain→drained, force_stop→force_stopped, deregister→deregistered transition
 * it (CLAUDE.md §13 — no fabricated state). `status` is the
 * `AgentControlResponse.status` the runtime returned.
 */
export function controlResultMessage(verb: ControlVerb, status: AgentStatus): string {
  switch (verb) {
    case 'pause':
      return `Pause issued — agent.paused emitted. Observable status: ${status} (the V1 registry has no "paused" state).`;
    case 'restart':
      return `Restart requested — agent.restart_requested emitted. Observable status: ${status}.`;
    case 'drain':
      return `Drain issued — status is now ${status}.`;
    case 'force_stop':
      return `Force-stop issued — status is now ${status}.`;
    case 'deregister':
      return 'Deregistered — the agent record was removed from the registry.';
    default:
      return `Control issued — status: ${status}.`;
  }
}

/** The destructive verbs that gate behind a confirm() prompt. */
export function isDestructiveVerb(verb: ControlVerb): boolean {
  return verb === 'force_stop' || verb === 'deregister';
}
