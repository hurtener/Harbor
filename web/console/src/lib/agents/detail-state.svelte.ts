// Harbor Console — Agent detail page reactive state controller
// (Phase 108l / D-184). Svelte 5 runes mode (D-092).
//
// `AgentDetailPageState` owns the detail route's reactive state: the primary
// `agents.get` projection + each tab's own async load + the LIVE `agent.*`
// activity subscription + the five fleet-control verbs. The `.svelte` view
// reads it and calls its actions, never touching the Protocol client directly
// (CONVENTIONS.md §6). It is the king-file refactor — the controller + the
// pure `derive.ts` projections replace the ~481-line monolith.
//
// # Fleet control is now LIVE (Phase 108l / D-184, supersedes D-132/F4)
//
// The five verbs call the REAL admin-gated `agents.{pause,drain,restart,
// force_stop,deregister}` Protocol methods. They are control-scope gated
// (`canControl` ← the `admin` claim) — disabled-with-tooltip without it,
// NEVER a fabricated success (CLAUDE.md §13). The response carries the ACTUAL
// re-read status: the V1 registry has no "paused" Health, so pause/restart
// emit their `agent.*` event but leave status `active`; only drain→drained,
// force_stop→force_stopped, deregister→gone transition it. After a control
// call the detail re-loads so the rendered status reflects the runtime; the
// activity feed picks up the emitted event live.
//
// The `subscription` / async-loaded fields are `$state` so the open / load
// assignment triggers the reactive re-read (the Events-page D-180 lesson).

import { resolveConnection, hasScope, type RuntimeConnection } from '$lib/connection.js';
import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
import { EventsSubscription } from '$lib/events/subscription.svelte.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type { ActivityEntry } from '$lib/components/agents/AgentActivityFeed.svelte';
import {
  toPageError,
  projectActivity,
  controlResultMessage,
  isDestructiveVerb,
  AGENT_EVENT_TYPES,
  type PageError,
  type ControlVerb
} from '$lib/agents/derive.js';
import type {
  AgentControlResponse,
  AgentGetResponse,
  AgentGovernance,
  AgentMemoryBinding,
  AgentPermissions,
  AgentSkillBinding,
  AgentToolBinding
} from '$lib/protocol/agents.js';

/** The inline result of a fleet-control call. */
export interface ControlResult {
  ok: boolean;
  message: string;
}

/** The closed set of detail tabs (page-agents.md §4). */
export type DetailTabId = 'identity' | 'autonomy' | 'tools' | 'memory' | 'cost' | 'skills';

export class AgentDetailPageState {
  /* ---- connection + client (CONVENTIONS.md §6) ------------------- */
  connection = $state<RuntimeConnection | null>(null);
  /** True when the connection carries the elevated control claim (D-066). */
  canControl = $state(false);
  disconnected = $derived(this.connection === null);

  /* ---- primary (agents.get) async state -------------------------- */
  status = $state<PageStatus>('loading');
  detailError = $state<PageError | null>(null);
  detail = $state<AgentGetResponse | null>(null);

  /* ---- per-tab async state (each its OWN nested PageState) -------- */
  toolsStatus = $state<PageStatus>('loading');
  toolsError = $state<PageError | null>(null);
  toolBindings = $state<AgentToolBinding[]>([]);

  memoryStatus = $state<PageStatus>('loading');
  memoryError = $state<PageError | null>(null);
  memoryBinding = $state<AgentMemoryBinding | null>(null);

  costStatus = $state<PageStatus>('loading');
  costError = $state<PageError | null>(null);
  governance = $state<AgentGovernance | null>(null);

  skillsStatus = $state<PageStatus>('loading');
  skillsError = $state<PageError | null>(null);
  skills = $state<AgentSkillBinding[]>([]);

  permsStatus = $state<PageStatus>('loading');
  permsError = $state<PageError | null>(null);
  permissions = $state<AgentPermissions | null>(null);

  /* ---- live activity (events.subscribe — D-184) ------------------ */
  subscription = $state<EventsSubscription | null>(null);

  /* ---- the active detail tab (Console-local view state) ---------- */
  activeTab = $state<DetailTabId>('identity');

  /* ---- fleet control (real Protocol calls — §13) ----------------- */
  controlBusy = $state<ControlVerb | null>(null);
  controlResult = $state<ControlResult | null>(null);
  /** Set once a deregister removes the record — the page shows a back link. */
  deregistered = $state(false);

  /** Confirmation gate for destructive verbs — overridable in tests. */
  confirmDestructive: (verb: ControlVerb) => boolean = (verb) =>
    globalThis.confirm?.(`${verb.replace('_', '-')} this agent? This cannot be undone.`) ?? false;

  #client: ProtocolClient | null = null;
  #agentID = '';

  /* ================================================================ */
  /* Derived projections                                               */
  /* ================================================================ */

  /** The agent id this controller is scoped to. */
  get agentID(): string {
    return this.#agentID;
  }

  /** The activity rows for THIS agent, newest-first (live SSE, honest-empty). */
  get activityEntries(): ActivityEntry[] {
    return projectActivity(this.subscription?.events ?? [], this.#agentID);
  }

  /** The SSE stream state for the activity feed's live indicator. */
  get streamState(): string {
    return this.subscription?.state ?? 'idle';
  }

  /* ================================================================ */
  /* Boot + loading                                                    */
  /* ================================================================ */

  /**
   * Resolves the connection, opens the activity subscription, and loads the
   * primary projection + every tab. `injected` is an optional in-page client
   * the harness supplies.
   */
  load(agentID: string, injected?: ProtocolClient): void {
    this.#agentID = agentID;
    const connection = resolveConnection();
    this.connection = connection;
    this.canControl = hasScope(connection, 'admin');

    if (connection === null) {
      this.#client = null;
      this.status = 'disconnected';
      this.toolsStatus = 'disconnected';
      this.memoryStatus = 'disconnected';
      this.costStatus = 'disconnected';
      this.skillsStatus = 'disconnected';
      this.permsStatus = 'disconnected';
      return;
    }
    this.#client = injected ?? new HarborClient({ connection });

    // Open the live `agent.*` activity subscription (scoped to the operator's
    // default session — where the control events this page issues land). The
    // ProtocolClient interface always carries the `events` namespace; the
    // injected harness client supplies a deterministic one.
    const sub = new EventsSubscription(this.#client.events);
    sub.open({ eventTypes: AGENT_EVENT_TYPES as string[] });
    this.subscription = sub;

    void this.loadDetail();
    void this.loadTools();
    void this.loadMemory();
    void this.loadCost();
    void this.loadSkills();
    void this.loadPermissions();
  }

  /** Selects a detail tab (Console-local view state). */
  selectTab(id: DetailTabId): void {
    this.activeTab = id;
  }

  /** Closes the activity subscription on unmount / re-scope teardown. */
  close(): void {
    this.subscription?.close();
    this.subscription = null;
  }

  /** Loads the primary `agents.get` projection — the Retry target. */
  async loadDetail(): Promise<void> {
    if (this.#client === null) {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.detailError = null;
    try {
      this.detail = await this.#client.agents.get<AgentGetResponse>(this.#agentID);
      this.status = 'ready';
    } catch (err) {
      this.detail = null;
      this.detailError = toPageError(err);
      this.status = 'error';
    }
  }

  async loadTools(): Promise<void> {
    if (this.#client === null) {
      this.toolsStatus = 'disconnected';
      return;
    }
    this.toolsStatus = 'loading';
    this.toolsError = null;
    try {
      const resp = await this.#client.agents.tools<{ bindings: AgentToolBinding[] }>(this.#agentID);
      this.toolBindings = resp.bindings;
      this.toolsStatus = resp.bindings.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      this.toolBindings = [];
      this.toolsError = toPageError(err);
      this.toolsStatus = 'error';
    }
  }

  async loadMemory(): Promise<void> {
    if (this.#client === null) {
      this.memoryStatus = 'disconnected';
      return;
    }
    this.memoryStatus = 'loading';
    this.memoryError = null;
    try {
      const resp = await this.#client.agents.memory<{ binding: AgentMemoryBinding }>(this.#agentID);
      this.memoryBinding = resp.binding;
      this.memoryStatus = 'ready';
    } catch (err) {
      this.memoryBinding = null;
      this.memoryError = toPageError(err);
      this.memoryStatus = 'error';
    }
  }

  async loadCost(): Promise<void> {
    if (this.#client === null) {
      this.costStatus = 'disconnected';
      return;
    }
    this.costStatus = 'loading';
    this.costError = null;
    try {
      const resp = await this.#client.agents.governance<{ governance: AgentGovernance }>(this.#agentID);
      this.governance = resp.governance;
      this.costStatus = 'ready';
    } catch (err) {
      this.governance = null;
      this.costError = toPageError(err);
      this.costStatus = 'error';
    }
  }

  async loadSkills(): Promise<void> {
    if (this.#client === null) {
      this.skillsStatus = 'disconnected';
      return;
    }
    this.skillsStatus = 'loading';
    this.skillsError = null;
    try {
      const resp = await this.#client.agents.skills<{ skills: AgentSkillBinding[] }>(this.#agentID);
      this.skills = resp.skills;
      this.skillsStatus = resp.skills.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      this.skills = [];
      this.skillsError = toPageError(err);
      this.skillsStatus = 'error';
    }
  }

  async loadPermissions(): Promise<void> {
    if (this.#client === null) {
      this.permsStatus = 'disconnected';
      return;
    }
    this.permsStatus = 'loading';
    this.permsError = null;
    try {
      const resp = await this.#client.agents.permissions<{ permissions: AgentPermissions }>(this.#agentID);
      this.permissions = resp.permissions;
      this.permsStatus = 'ready';
    } catch (err) {
      this.permissions = null;
      this.permsError = toPageError(err);
      this.permsStatus = 'error';
    }
  }

  /* ================================================================ */
  /* Fleet control — the REAL agents.* admin verbs (D-184)             */
  /* ================================================================ */

  /**
   * Dispatch one fleet-control verb against this agent. Control-scope gated
   * (no-op without `canControl`); destructive verbs (force_stop / deregister)
   * gate behind {@link confirmDestructive}. The honest result reports the
   * ACTUAL re-read status the response carried — pause/restart leave status
   * `active` (the registry has no "paused" state), only drain/force_stop/
   * deregister transition it (CLAUDE.md §13). On success the detail re-loads;
   * the activity feed observes the emitted `agent.*` event live.
   */
  async control(verb: ControlVerb): Promise<void> {
    if (this.#client === null || !this.canControl || this.#agentID === '') return;
    if (this.controlBusy !== null) return;
    if (isDestructiveVerb(verb) && !this.confirmDestructive(verb)) return;

    this.controlBusy = verb;
    this.controlResult = null;
    try {
      const resp = await this.#invoke(verb);
      this.controlResult = { ok: true, message: controlResultMessage(verb, resp.status) };
      if (verb === 'deregister') {
        // The record is gone — a re-read would 404. Show the removed banner.
        this.deregistered = true;
      } else {
        await this.loadDetail();
      }
    } catch (err) {
      const e = toPageError(err);
      this.controlResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      this.controlBusy = null;
    }
  }

  /** Routes a verb to its typed namespace method. */
  #invoke(verb: ControlVerb): Promise<AgentControlResponse> {
    const a = this.#client!.agents;
    const id = this.#agentID;
    switch (verb) {
      case 'pause':
        return a.pause<AgentControlResponse>(id);
      case 'drain':
        return a.drain<AgentControlResponse>(id);
      case 'restart':
        return a.restart<AgentControlResponse>(id);
      case 'force_stop':
        return a.forceStop<AgentControlResponse>(id);
      case 'deregister':
        return a.deregister<AgentControlResponse>(id);
    }
  }
}
