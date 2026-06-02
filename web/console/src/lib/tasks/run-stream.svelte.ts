// Harbor Console — Tasks page per-task run stream controller
// (Phase 108i / D-181). Svelte 5 runes mode (D-092).
//
// `TaskRunStream` owns ONE live `events.subscribe` subscription scoped to
// a selected task's run, and exposes the derived projections the per-task
// bottom dock + the right-rail Summary / Cost cards render. It mirrors the
// Sessions `BottomDockTabs` pattern (a session-scoped subscription) but
// narrows to a single RUN via {@link eventBelongsToRun} — so the dock
// tabs AND the rail cost/event figures read ONE stream, never two.
//
// The subscription opens a per-session client (identity.session =
// parent_session_id) so the runtime scopes the SSE server-side to the
// task's session (stream.go reads the `?session=` param); the run-narrow
// happens client-side because a run id is not a server-side SSE filter
// axis. The `subscription` / `aggregator` fields are `$state` so the
// async open assignment triggers the reactive re-read (the Events-page
// D-180 lesson: a plain field leaves the UI bound to the initial null).

import { HarborClient } from '$lib/protocol/harbor.js';
import type { RuntimeConnection } from '$lib/connection.js';
import { EventsSubscription } from '$lib/events/subscription.svelte.js';
import { EVENT_TYPES } from '$lib/events/taxonomy.js';
import { projectTrajectory, type TrajectoryStep } from '$lib/sessions/trajectory.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { Event } from '$lib/protocol/events.js';
import type { PauseSnapshot } from '$lib/protocol/pause.js';
import {
  filterRunEvents,
  filterControlEvents,
  filterInterventionEvents,
  filterGroupEvents,
  projectRunCost,
  type RunCost
} from './run-events.js';

/**
 * The lifecycle / control / cost / intervention / group event union the
 * dock tabs render — enumerated (not empty) so the named-SSE-frame
 * listeners register (subscription.svelte.ts) and the URL stays bounded.
 */
const RUN_DOCK_TYPES = EVENT_TYPES.filter(
  (t) =>
    t.startsWith('planner.') ||
    t.startsWith('tool.') ||
    t.startsWith('task.') ||
    t.startsWith('control.') ||
    t.startsWith('pause.') ||
    t === 'llm.cost.recorded'
);

/** A task's run-scope coordinates: its id (= run id) + parent session. */
export interface RunTarget {
  /** The task id — also the run id (a control verb targets `identity.run`). */
  id: string;
  /** The parent session the SSE scopes to (the X-Harbor-Session axis). */
  sessionID: string;
}

export class TaskRunStream {
  /** The live subscription — `$state` so the async open triggers re-read. */
  subscription = $state<EventsSubscription | null>(null);
  /** Still-pending pauses for this run (`pause.list` backfill). */
  pending = $state<PauseSnapshot[]>([]);
  /** The token of an in-flight Resume/Reject; null when idle. */
  actionBusy = $state<string | null>(null);
  /** Per-token Resume/Reject result copy. */
  actionResult = $state<Map<string, string>>(new Map());

  readonly #client: HarborClient;
  readonly #target: RunTarget;

  constructor(connection: RuntimeConnection, target: RunTarget) {
    this.#target = target;
    // A session-scoped client so the SSE filters server-side to the run's
    // session and `pause.list` defaults to that session's pauses.
    this.#client = new HarborClient({
      connection: { ...connection, identity: { ...connection.identity, session: target.sessionID } }
    });
  }

  /** Opens the run subscription + backfills pending pauses. */
  open(): void {
    const sub = new EventsSubscription(this.#client.events);
    sub.open({ eventTypes: RUN_DOCK_TYPES as string[], session: this.#target.sessionID });
    this.subscription = sub;
    this.pending = [];
    this.actionResult = new Map();
    void this.#loadPending();
  }

  /** Closes the subscription (mode-switch / re-scope teardown). */
  close(): void {
    this.subscription?.close();
    this.subscription = null;
  }

  /** All events on the stream (the session page, newest-first). */
  get events(): Event[] {
    return this.subscription?.events ?? [];
  }

  /** The events belonging to THIS task's run (the dock + cost source). */
  get runEvents(): Event[] {
    return filterRunEvents(this.events, this.#target.id);
  }

  /** The reconstructed planner trajectory for this run (oldest-first). */
  get trajectory(): TrajectoryStep[] {
    return projectTrajectory(this.runEvents);
  }

  /** The control-instruction events for this run. */
  get controlEvents(): Event[] {
    return filterControlEvents(this.runEvents);
  }

  /** The HITL / pause intervention events for this run. */
  get interventionEvents(): Event[] {
    return filterInterventionEvents(this.runEvents);
  }

  /** The TaskGroup lifecycle events for this run. */
  get groupEvents(): Event[] {
    return filterGroupEvents(this.runEvents);
  }

  /** The token-type cost rollup for this run (the rail Cost + Summary). */
  get cost(): RunCost {
    return projectRunCost(this.runEvents);
  }

  /** The count of run events loaded (the Summary "Events" figure). */
  get eventCount(): number {
    return this.runEvents.length;
  }

  /** The SSE stream state for the dock's live indicator. */
  get streamState(): string {
    return this.subscription?.state ?? 'idle';
  }

  /** Loads the still-pending pauses for this run (`pause.list`). */
  async #loadPending(): Promise<void> {
    try {
      const resp = await this.#client.pause.list({
        filter: { session_ids: [this.#target.sessionID], status: ['paused'] }
      });
      // Narrow to this run — the session may host multiple runs.
      this.pending = (resp.snapshots ?? []).filter(
        (s) => (s.identity.run ?? '') === this.#target.id
      );
    } catch {
      // Best-effort backfill — the live `pause.*` events still render.
      this.pending = [];
    }
  }

  /** Resolves a pending pause via the shipped `approve` / `reject` verbs. */
  async resolve(snap: PauseSnapshot, verb: 'approve' | 'reject'): Promise<void> {
    const run = snap.identity.run ?? '';
    if (run === '' || this.actionBusy !== null) return;
    this.actionBusy = snap.token;
    try {
      if (verb === 'approve') await this.#client.control.approve(run);
      else await this.#client.control.reject(run);
      const m = new Map(this.actionResult);
      m.set(snap.token, verb === 'approve' ? 'approved' : 'rejected');
      this.actionResult = m;
      void this.#loadPending();
    } catch (err) {
      const m = new Map(this.actionResult);
      m.set(snap.token, err instanceof ProtocolError ? `${err.code}: ${err.message}` : String(err));
      this.actionResult = m;
    } finally {
      this.actionBusy = null;
    }
  }
}
