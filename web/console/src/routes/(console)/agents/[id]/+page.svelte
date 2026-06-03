<script lang="ts">
  // Harbor Console — Agent detail page (`/agents/<id>`) — Phase 108l
  // rebuild (D-184; supersedes the Phase 73e / D-124 pre-chrome layout +
  // the D-132/F4 disabled-control stub).
  //
  // The detail-mode view for one agent, rethemed to the carded,
  // viewport-locked THREE-COLUMN main canvas the mock + page-agents.md §4
  // pin (NOT a right rail):
  //   - Row 1 — header card: name + health + status pill + version_hash +
  //     the five LIVE fleet-control buttons + Open-in-Playground + copy-id.
  //   - Row 2, left column — the six-tab detail strip (Identity / Autonomy /
  //     Tools / Memory / Cost / Skills), each tab its OWN nested PageState.
  //   - Row 2, center column — the topology mini-graph.
  //   - Row 2, right column — the LIVE activity feed + connected tools +
  //     memory summary + permissions.
  // The ~481-line king file is refactored into an `AgentDetailPageState`
  // controller + pure `derive.ts` projections. Drops the per-page header
  // (breadcrumb / ⌘K / footer are app-shell chrome, 108b).
  //
  // # Fleet control is LIVE (D-184, supersedes D-132/F4)
  //
  // The five verbs call the REAL admin-gated `agents.*` Protocol methods,
  // control-scope gated (disabled-with-tooltip without `admin`, never a fake
  // success — §13). The honest result reports the ACTUAL re-read status: the
  // registry has no "paused" state, so pause/restart leave status `active`
  // and emit their `agent.*` event; only drain/force_stop/deregister
  // transition it. The activity feed observes the emitted event live.
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient + connection.ts
  // only — no hand-rolled fetch (CONVENTIONS.md §6).
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { PageState } from '$lib/components/ui';
  import DetailHeader from '$lib/components/agents/DetailHeader.svelte';
  import IdentityTab from '$lib/components/agents/IdentityTab.svelte';
  import AutonomyTab from '$lib/components/agents/AutonomyTab.svelte';
  import ToolsTab from '$lib/components/agents/ToolsTab.svelte';
  import MemoryTab from '$lib/components/agents/MemoryTab.svelte';
  import CostTab from '$lib/components/agents/CostTab.svelte';
  import SkillsTab from '$lib/components/agents/SkillsTab.svelte';
  import TopologyMiniGraph from '$lib/components/agents/TopologyMiniGraph.svelte';
  import AgentActivityFeed from '$lib/components/agents/AgentActivityFeed.svelte';
  import { AgentDetailPageState, type DetailTabId } from '$lib/agents/detail-state.svelte.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';

  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  /** The six detail tabs (page-agents.md §4). */
  const TABS: { id: DetailTabId; label: string }[] = [
    { id: 'identity', label: 'Identity' },
    { id: 'autonomy', label: 'Autonomy' },
    { id: 'tools', label: 'Tools' },
    { id: 'memory', label: 'Memory' },
    { id: 'cost', label: 'Cost' },
    { id: 'skills', label: 'Skills' }
  ];

  const agentID = $derived(page.params.id ?? '');

  const state = new AgentDetailPageState();
  // A local derived so the `{#if detail}` block narrows cleanly to a
  // non-null `AgentGetResponse` for the child props (svelte-check).
  const detail = $derived(state.detail);

  onMount(() => {
    state.load(agentID, injectedClient);
    return () => state.close();
  });
</script>

<svelte:head>
  <title>Agent · Harbor Console</title>
</svelte:head>

<section class="agent-detail" data-testid="agent-detail-page">
  <PageState status={state.status} error={state.detailError} onretry={() => void state.loadDetail()}>
    {#snippet skeleton()}
      <div class="detail-skeleton" aria-hidden="true">
        <span class="skeleton-bar"></span>
        <span class="skeleton-bar"></span>
      </div>
    {/snippet}
    {#snippet empty()}
      <div class="not-found">
        <p class="empty-detail">Agent not found — perhaps it was deregistered.</p>
        <a class="back-link" href="/agents">← Back to Agents</a>
      </div>
    {/snippet}

    {#if state.deregistered}
      <div class="not-found" data-testid="agent-deregistered">
        <p class="empty-detail">
          This agent was deregistered — its record was removed from the registry.
        </p>
        <a class="back-link" href="/agents">← Back to Agents</a>
      </div>
    {:else if detail}
      <section class="panel card header-card">
        <DetailHeader
          agent={detail.agent}
          canControl={state.canControl}
          controlBusy={state.controlBusy}
          controlResult={state.controlResult}
          onverb={(verb) => void state.control(verb)}
        />
      </section>

      <div class="canvas">
        <!-- Left column — the tabbed detail. -->
        <section class="panel card tab-column">
          <nav class="tab-strip" data-testid="agent-tab-strip">
            {#each TABS as tab (tab.id)}
              <button
                type="button"
                class="tab"
                class:active={state.activeTab === tab.id}
                data-testid={`agent-tab-${tab.id}`}
                aria-pressed={state.activeTab === tab.id}
                onclick={() => state.selectTab(tab.id)}
              >
                {tab.label}
              </button>
            {/each}
          </nav>

          <section class="tab-body" data-testid="agent-tab-body">
            {#if state.activeTab === 'identity'}
              <IdentityTab {detail} />
            {:else if state.activeTab === 'autonomy'}
              <AutonomyTab config={detail.config} />
            {:else if state.activeTab === 'tools'}
              <PageState status={state.toolsStatus} error={state.toolsError} onretry={() => void state.loadTools()}>
                {#snippet empty()}
                  <p class="tab-empty">No tool bindings configured.</p>
                {/snippet}
                <ToolsTab bindings={state.toolBindings} />
              </PageState>
            {:else if state.activeTab === 'memory'}
              <PageState status={state.memoryStatus} error={state.memoryError} onretry={() => void state.loadMemory()}>
                {#if state.memoryBinding}
                  <MemoryTab binding={state.memoryBinding} />
                {/if}
              </PageState>
            {:else if state.activeTab === 'cost'}
              <PageState status={state.costStatus} error={state.costError} onretry={() => void state.loadCost()}>
                {#if state.governance}
                  <CostTab governance={state.governance} />
                {/if}
              </PageState>
            {:else}
              <PageState status={state.skillsStatus} error={state.skillsError} onretry={() => void state.loadSkills()}>
                {#snippet empty()}
                  <p class="tab-empty">No skills attached.</p>
                {/snippet}
                <SkillsTab skills={state.skills} />
              </PageState>
            {/if}
          </section>
        </section>

        <!-- Center column — the topology mini-graph. -->
        <section class="panel card topology-column">
          <h2 class="panel-title">Topology</h2>
          <TopologyMiniGraph bindings={state.toolBindings} />
        </section>

        <!-- Right column — live activity + connected tools + memory + perms. -->
        <section class="panel card activity-column">
          <div class="rail-block">
            <h2 class="panel-title">Live activity</h2>
            <AgentActivityFeed entries={state.activityEntries} streamState={state.streamState} />
          </div>

          <div class="rail-block">
            <h2 class="panel-title">Connected tools</h2>
            {#if state.toolBindings.length === 0}
              <p class="rail-empty">No tools connected.</p>
            {:else}
              <ul class="connected-tools" data-testid="agent-connected-tools">
                {#each state.toolBindings as binding (binding.tool_id)}
                  <li>
                    <a class="tool-link" href={`/tools/${binding.tool_id}`}>
                      {binding.tool_name || binding.tool_id}
                    </a>
                    <span class="tool-transport">{binding.transport}</span>
                  </li>
                {/each}
              </ul>
            {/if}
          </div>

          <div class="rail-block">
            <h2 class="panel-title">Memory</h2>
            {#if state.memoryBinding && state.memoryBinding.strategy_id}
              <dl class="rail-kv">
                <dt>Strategy</dt>
                <dd>{state.memoryBinding.strategy_id}</dd>
                <dt>Scope</dt>
                <dd>{state.memoryBinding.scope || '—'}</dd>
              </dl>
            {:else}
              <p class="rail-empty">No memory strategy configured.</p>
            {/if}
          </div>

          <div class="rail-block">
            <h2 class="panel-title">Permissions</h2>
            <PageState status={state.permsStatus} error={state.permsError} onretry={() => void state.loadPermissions()}>
              {#if state.permissions}
                <p class="perm-model" data-testid="agent-permissions-model">
                  {state.permissions.model}
                </p>
                <p class="perm-desc">{state.permissions.description}</p>
              {/if}
            </PageState>
          </div>
        </section>
      </div>
    {/if}
  </PageState>
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls; each of the three columns scrolls INTERNALLY
     (PAGE-POLISH §6). */
  .agent-detail {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    padding: var(--space-3);
    overflow: hidden;
  }

  .agent-detail :global(.page-state) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .card {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-width: 0;
  }

  .panel-title {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .header-card {
    flex-shrink: 0;
  }

  /* The three-column canvas fills the remaining height; every column scrolls
     internally with its own padding + a stable scrollbar gutter so
     right-aligned values never clip (the Tools/BG rail lesson). */
  .canvas {
    display: grid;
    grid-template-columns: minmax(0, 1.1fr) minmax(0, 1fr) var(--size-rail);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  .tab-column,
  .topology-column,
  .activity-column {
    display: flex;
    flex-direction: column;
    min-height: 0;
    overflow-y: auto;
    scrollbar-gutter: stable;
    gap: var(--space-3);
  }

  .tab-strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
    flex-shrink: 0;
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
    padding: var(--space-1) var(--space-0);
  }

  .tab-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .rail-block {
    display: flex;
    flex-direction: column;
  }

  .rail-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .connected-tools {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .connected-tools li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .tool-link {
    font-size: var(--text-sm);
    color: var(--color-accent);
  }

  .tool-transport {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .rail-kv {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
  }

  .rail-kv dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .rail-kv dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
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

  .not-found {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
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
