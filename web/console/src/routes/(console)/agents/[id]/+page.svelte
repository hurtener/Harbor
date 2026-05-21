<script lang="ts">
  // Harbor Console — Agent detail page (`/agents/<id>`), Phase 73e /
  // D-124, built against the D-121 design-system foundation.
  //
  // The detail-mode view for one agent: the `DetailHeader` (status badge
  // + version_hash + the five fleet-control buttons) + a six-tab strip
  // (Identity / Autonomy / Tools / Memory / Cost / Skills) + a
  // `DetailRail` of `RailCard`s (topology mini-graph, connected tools,
  // recent activity). Read-only inspector (page-agents.md §10).
  //
  // # Console consistency (CONVENTIONS.md §9)
  //
  // - Routes under `(console)/agents/[id]` — a NEW detail route, so the
  //   segment is `[id]` (§1); no `/console/` URL prefix.
  // - Renders inside the app shell (§2).
  // - Composes the `ui/` inventory: `PageHeader`, `DetailRail`/`RailCard`,
  //   `PageState` (§3/§4). Agents-specific pieces stay in
  //   `components/agents/` (§3).
  // - Routes async state through the four-state `<PageState>` (§4); each
  //   detail tab + rail card flows through its OWN nested `<PageState>`.
  // - Talks to the Runtime only through `HarborClient` + `connection.ts`
  //   (§6) — no hand-rolled `fetch`. Design tokens only (§7).
  //
  // # Control verbs (page-agents.md §9, D-066, D-132)
  //
  // The five fleet-control verbs (Pause / Drain / Restart / Force-Stop /
  // Deregister) are exposed by the shipped `registry.*` IN-PROCESS Go
  // API — there is NO Protocol method a Console client can call. The
  // Wave 13 §17.5 checkpoint (D-132 / F4) pinned that the previous
  // `controlFeedback`-string wiring was a fake-success path. Until a
  // `registry.*` Protocol surface exists, the buttons are rendered
  // disabled-with-tooltip by `ControlButtons.svelte` — regardless of
  // scope claim. The page wires no control handler.
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import {
    AgentsProtocol,
    type AgentGetResponse,
    type AgentGovernance,
    type AgentMemoryBinding,
    type AgentPermissions,
    type AgentSkillBinding,
    type AgentToolBinding
  } from '$lib/protocol/agents.js';
  import {
    resolveConnection,
    type RuntimeConnection
  } from '$lib/connection.js';
  import {
    DetailRail,
    RailCard,
    PageState,
    type PageStatus
  } from '$lib/components/ui';
  import DetailHeader from '$lib/components/agents/DetailHeader.svelte';
  import IdentityTab from '$lib/components/agents/IdentityTab.svelte';
  import AutonomyTab from '$lib/components/agents/AutonomyTab.svelte';
  import ToolsTab from '$lib/components/agents/ToolsTab.svelte';
  import MemoryTab from '$lib/components/agents/MemoryTab.svelte';
  import CostTab from '$lib/components/agents/CostTab.svelte';
  import SkillsTab from '$lib/components/agents/SkillsTab.svelte';
  import TopologyMiniGraph from '$lib/components/agents/TopologyMiniGraph.svelte';
  import AgentActivityFeed, {
    type ActivityEntry
  } from '$lib/components/agents/AgentActivityFeed.svelte';

  interface AgentsPageGlobals {
    __HARBOR_PROTOCOL_CLIENT__?: ProtocolClient;
  }
  const injected = globalThis as unknown as AgentsPageGlobals;

  /** The closed set of detail tabs (page-agents.md §4). */
  type TabId = 'identity' | 'autonomy' | 'tools' | 'memory' | 'cost' | 'skills';
  const TABS: { id: TabId; label: string }[] = [
    { id: 'identity', label: 'Identity' },
    { id: 'autonomy', label: 'Autonomy' },
    { id: 'tools', label: 'Tools' },
    { id: 'memory', label: 'Memory' },
    { id: 'cost', label: 'Cost' },
    { id: 'skills', label: 'Skills' }
  ];

  const agentID = $derived(page.params.id ?? '');

  let connection = $state<RuntimeConnection | null>(null);
  let agentsApi = $state<AgentsProtocol | null>(null);

  let activeTab = $state<TabId>('identity');

  // ---- primary (agents.get) async state ---------------------------
  let status = $state<PageStatus>('loading');
  let detailError = $state<ProtocolError | { code: string; message: string } | null>(
    null
  );
  let detail = $state<AgentGetResponse | null>(null);

  // ---- per-tab async state (each its OWN nested PageState) --------
  let toolsStatus = $state<PageStatus>('loading');
  let toolsError = $state<ProtocolError | { code: string; message: string } | null>(
    null
  );
  let toolBindings = $state<AgentToolBinding[]>([]);

  let memoryStatus = $state<PageStatus>('loading');
  let memoryError = $state<ProtocolError | { code: string; message: string } | null>(
    null
  );
  let memoryBinding = $state<AgentMemoryBinding | null>(null);

  let costStatus = $state<PageStatus>('loading');
  let costError = $state<ProtocolError | { code: string; message: string } | null>(
    null
  );
  let governance = $state<AgentGovernance | null>(null);

  let skillsStatus = $state<PageStatus>('loading');
  let skillsError = $state<ProtocolError | { code: string; message: string } | null>(
    null
  );
  let skills = $state<AgentSkillBinding[]>([]);

  let permsStatus = $state<PageStatus>('loading');
  let permsError = $state<ProtocolError | { code: string; message: string } | null>(
    null
  );
  let permissions = $state<AgentPermissions | null>(null);

  // ---- recent activity (events stream — a later wave wires it) ----
  const activityEntries = $state<ActivityEntry[]>([]);

  /** Maps a thrown error into `<PageState>`'s Error shape. */
  function asPageError(
    err: unknown
  ): ProtocolError | { code: string; message: string } {
    if (err instanceof ProtocolError) return err;
    return {
      code: 'runtime_error',
      message: err instanceof Error ? err.message : String(err)
    };
  }

  /** Loads the primary `agents.get` projection — the Retry target. */
  async function loadDetail(): Promise<void> {
    if (agentsApi === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    detailError = null;
    try {
      detail = await agentsApi.get(agentID);
      status = 'ready';
    } catch (err) {
      detail = null;
      detailError = asPageError(err);
      status = 'error';
    }
  }

  /** Loads the Tools tab — its own nested PageState (§4). */
  async function loadTools(): Promise<void> {
    if (agentsApi === null) {
      toolsStatus = 'disconnected';
      return;
    }
    toolsStatus = 'loading';
    toolsError = null;
    try {
      const resp = await agentsApi.tools(agentID);
      toolBindings = resp.bindings;
      toolsStatus = resp.bindings.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      toolBindings = [];
      toolsError = asPageError(err);
      toolsStatus = 'error';
    }
  }

  /** Loads the Memory tab — its own nested PageState (§4). */
  async function loadMemory(): Promise<void> {
    if (agentsApi === null) {
      memoryStatus = 'disconnected';
      return;
    }
    memoryStatus = 'loading';
    memoryError = null;
    try {
      const resp = await agentsApi.memory(agentID);
      memoryBinding = resp.binding;
      memoryStatus = 'ready';
    } catch (err) {
      memoryBinding = null;
      memoryError = asPageError(err);
      memoryStatus = 'error';
    }
  }

  /** Loads the Cost tab — its own nested PageState (§4). */
  async function loadCost(): Promise<void> {
    if (agentsApi === null) {
      costStatus = 'disconnected';
      return;
    }
    costStatus = 'loading';
    costError = null;
    try {
      const resp = await agentsApi.governance(agentID);
      governance = resp.governance;
      costStatus = 'ready';
    } catch (err) {
      governance = null;
      costError = asPageError(err);
      costStatus = 'error';
    }
  }

  /** Loads the Skills tab — its own nested PageState (§4). */
  async function loadSkills(): Promise<void> {
    if (agentsApi === null) {
      skillsStatus = 'disconnected';
      return;
    }
    skillsStatus = 'loading';
    skillsError = null;
    try {
      const resp = await agentsApi.skills(agentID);
      skills = resp.skills;
      skillsStatus = resp.skills.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      skills = [];
      skillsError = asPageError(err);
      skillsStatus = 'error';
    }
  }

  /** Loads the agent's permission model for the rail's Permissions card. */
  async function loadPermissions(): Promise<void> {
    if (agentsApi === null) {
      permsStatus = 'disconnected';
      return;
    }
    permsStatus = 'loading';
    permsError = null;
    try {
      const resp = await agentsApi.permissions(agentID);
      permissions = resp.permissions;
      permsStatus = 'ready';
    } catch (err) {
      permissions = null;
      permsError = asPageError(err);
      permsStatus = 'error';
    }
  }

  function selectTab(id: TabId): void {
    activeTab = id;
  }

  onMount(() => {
    connection = resolveConnection();
    if (injected.__HARBOR_PROTOCOL_CLIENT__) {
      agentsApi = new AgentsProtocol(injected.__HARBOR_PROTOCOL_CLIENT__);
    } else if (connection !== null) {
      agentsApi = new AgentsProtocol(new HarborClient({ connection }));
    }

    void loadDetail();
    void loadTools();
    void loadMemory();
    void loadCost();
    void loadSkills();
    void loadPermissions();
  });
</script>

<svelte:head>
  <title>Agent · Harbor Console</title>
</svelte:head>

<div class="agent-detail" data-testid="agent-detail-page">
  <PageState {status} error={detailError} onretry={() => void loadDetail()}>
    {#snippet skeleton()}
      <div class="detail-skeleton" aria-hidden="true">
        <span class="skeleton-bar"></span>
        <span class="skeleton-bar"></span>
      </div>
    {/snippet}
    {#snippet empty()}
      <p class="empty-detail">Agent not found — perhaps it was deregistered.</p>
      <a class="back-link" href="/agents">← Back to Agents</a>
    {/snippet}

    {#if detail}
      <DetailHeader agent={detail.agent} />

      <div class="detail-body">
        <main class="tab-column">
          <nav class="tab-strip" data-testid="agent-tab-strip">
            {#each TABS as tab (tab.id)}
              <button
                type="button"
                class="tab"
                class:active={activeTab === tab.id}
                data-testid={`agent-tab-${tab.id}`}
                aria-pressed={activeTab === tab.id}
                onclick={() => selectTab(tab.id)}
              >
                {tab.label}
              </button>
            {/each}
          </nav>

          <section class="tab-body" data-testid="agent-tab-body">
            {#if activeTab === 'identity'}
              <IdentityTab {detail} />
            {:else if activeTab === 'autonomy'}
              <AutonomyTab config={detail.config} />
            {:else if activeTab === 'tools'}
              <PageState
                status={toolsStatus}
                error={toolsError}
                onretry={() => void loadTools()}
              >
                {#snippet empty()}
                  <p class="tab-empty">No tool bindings configured.</p>
                {/snippet}
                <ToolsTab bindings={toolBindings} />
              </PageState>
            {:else if activeTab === 'memory'}
              <PageState
                status={memoryStatus}
                error={memoryError}
                onretry={() => void loadMemory()}
              >
                {#if memoryBinding}
                  <MemoryTab binding={memoryBinding} />
                {/if}
              </PageState>
            {:else if activeTab === 'cost'}
              <PageState
                status={costStatus}
                error={costError}
                onretry={() => void loadCost()}
              >
                {#if governance}
                  <CostTab {governance} />
                {/if}
              </PageState>
            {:else}
              <PageState
                status={skillsStatus}
                error={skillsError}
                onretry={() => void loadSkills()}
              >
                {#snippet empty()}
                  <p class="tab-empty">No skills attached.</p>
                {/snippet}
                <SkillsTab {skills} />
              </PageState>
            {/if}
          </section>
        </main>

        <DetailRail>
          <RailCard title="Topology">
            <TopologyMiniGraph bindings={toolBindings} />
          </RailCard>
          <RailCard title="Recent activity">
            <AgentActivityFeed entries={activityEntries} />
          </RailCard>
          <RailCard title="Permissions">
            <PageState
              status={permsStatus}
              error={permsError}
              onretry={() => void loadPermissions()}
            >
              {#if permissions}
                <p class="perm-model" data-testid="agent-permissions-model">
                  {permissions.model}
                </p>
                <p class="perm-desc">{permissions.description}</p>
              {/if}
            </PageState>
          </RailCard>
        </DetailRail>
      </div>
    {/if}
  </PageState>
</div>

<style>
  .agent-detail {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .detail-body {
    display: flex;
    gap: var(--space-4);
    align-items: flex-start;
  }

  .tab-column {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .tab-strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
  }

  .tab {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    background: transparent;
    border: none;
    border-bottom: var(--size-px) solid transparent;
    padding: var(--space-2) var(--space-3);
    cursor: pointer;
  }

  .tab.active {
    color: var(--color-text);
    border-bottom-color: var(--color-accent);
  }

  .tab-body {
    padding: var(--space-3) var(--space-0);
  }

  .tab-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .perm-model {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .perm-desc {
    margin: var(--space-1) var(--space-0) var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .detail-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-bar {
    height: var(--size-progress-track);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .back-link {
    font-size: var(--text-sm);
    color: var(--color-accent);
  }
</style>
