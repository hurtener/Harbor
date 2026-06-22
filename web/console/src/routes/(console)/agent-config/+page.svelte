<script lang="ts">
  // Harbor Console — agent-config control panel (`/agent-config`), the
  // consolidated agent-config consumer.
  //
  // ONE panel for a selected agent that renders the five control-plane
  // areas as self-contained carded components over the shared `ui/`
  // inventory: revision history + diff + rollback (92a), skills (92c), MCP
  // pause/resume + per-tool disable (92d), the layered prompt (92e), and
  // add-MCP-connection (92f). It is a pure Protocol client — every read is
  // an `agent_config.*` snapshot + the live `mcp.connection.*` stream, every
  // write goes through the typed `AgentConfigNamespace`; it holds NO config
  // of its own (D-061). Admin-gated writes (disabled-with-tooltip for
  // non-admin — the 92b precedent); reads use the four-state `<PageState>`.
  //
  // Built against `docs/design/console/CONVENTIONS.md` (D-121): routes under
  // `(console)/` (served at `/agent-config`), renders inside the app shell,
  // routes async state through `<PageState>`, talks to the Runtime only
  // through `HarborClient` + `connection.ts`, design tokens only, Svelte 5
  // runes (D-092). The agent selector defaults to the connected runtime's
  // agent (`harbor-dev-agent` in dev) and honours a `?agent=` deep-link.
  import { onMount, untrack } from 'svelte';
  import { page } from '$app/stores';
  import { PageState } from '$lib/components/ui';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import AgentConfigSubNavRail from '$lib/components/agentconfig/AgentConfigSubNavRail.svelte';
  import RevisionHistoryCard from '$lib/components/agentconfig/RevisionHistoryCard.svelte';
  import SkillsCard from '$lib/components/agentconfig/SkillsCard.svelte';
  import McpPolicyCard from '$lib/components/agentconfig/McpPolicyCard.svelte';
  import PromptLayersCard from '$lib/components/agentconfig/PromptLayersCard.svelte';
  import LLMParamsCard from '$lib/components/agentconfig/LLMParamsCard.svelte';
  import AddConnectionCard from '$lib/components/agentconfig/AddConnectionCard.svelte';
  import SaveAllBar from '$lib/components/agentconfig/SaveAllBar.svelte';
  import {
    AgentConfigPanelState,
    AGENT_CONFIG_AREAS,
    type AgentConfigAreaId
  } from '$lib/agentconfig/state.svelte.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';

  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  const panel = new AgentConfigPanelState();

  /** The active control-plane area — drives the rail highlight + the single
   * section rendered in the right pane (the Settings single-section model). */
  let activeArea = $state<AgentConfigAreaId>('revisions');

  /** The active area's descriptor (id / label) for the section heading. */
  const activeAreaObj = $derived(
    AGENT_CONFIG_AREAS.find((a) => a.id === activeArea) ?? AGENT_CONFIG_AREAS[0]
  );

  // The agent-selector input (separate from the committed `panel.agentId`
  // so typing does not re-fire loads on every keystroke — the operator
  // commits with the Load button / Enter).
  let agentInput = $state('');

  function commitAgent(): void {
    panel.setAgent(agentInput);
    void panel.load(injectedClient);
  }

  // Close the live subscription on unmount.
  onMount(() => () => panel.close());

  // (Re)load whenever the `?agent=` deep-link changes. This tracks ONLY the
  // query param, so it fires on the initial mount AND on a later client-side
  // navigation to a different agent (the "Configure" deep-link from the
  // Agents page, while already on this route). A plain guard skips redundant
  // reloads when an unrelated URL change leaves `?agent=` untouched; the
  // imperative (re)load runs untracked so it never re-triggers the effect.
  let lastQueryAgent: string | null = null;
  $effect(() => {
    const raw = $page.url.searchParams.get('agent');
    untrack(() => {
      const wanted = raw && raw.trim() !== '' ? raw.trim() : '';
      if (wanted === lastQueryAgent) return;
      lastQueryAgent = wanted;
      if (wanted !== '') {
        panel.setAgent(wanted);
      }
      agentInput = panel.agentId;
      void panel.load(injectedClient);
    });
  });
</script>

<svelte:head>
  <title>Agent Config · Harbor Console</title>
</svelte:head>

<section class="agentcfg-page" data-testid="agent-config-page">
  <header class="page-head">
    <div class="title-block">
      <h1 class="page-title">Agent configuration</h1>
      <p class="page-sub">
        Versioned, live control of one agent&rsquo;s prompt, skills, MCP policy,
        and connections. Changes apply on the agent&rsquo;s next run.
      </p>
    </div>
  </header>

  <div class="panel-layout">
    <div class="rail-column">
      <AgentConfigSubNavRail active={activeArea} onselect={(id) => (activeArea = id)}>
      {#snippet header()}
        <div class="agent-select">
          <label class="field">
            <span class="field-label">Agent</span>
            <input
              type="text"
              class="agent-input"
              data-testid="agent-config-agent-input"
              placeholder="harbor-dev-agent"
              bind:value={agentInput}
              disabled={panel.disconnected}
              title={panel.disconnected ? DISCONNECTED_TOOLTIP : undefined}
              onkeydown={(e) => e.key === 'Enter' && commitAgent()}
            />
          </label>
          <button
            type="button"
            class="primary"
            data-testid="agent-config-load"
            disabled={panel.disconnected || agentInput.trim() === ''}
            title={panel.disconnected ? DISCONNECTED_TOOLTIP : 'Load this agent’s configuration'}
            onclick={commitAgent}
          >
            Load
          </button>
        </div>
      {/snippet}
      </AgentConfigSubNavRail>
    </div>

    <div class="section-pane">
      <!-- The atomic multi-section "Save all" bar renders OUTSIDE the
           <PageState> async boundary (it is panel-level staging, visible
           across every area) and self-gates on staged edits. -->
      <SaveAllBar {panel} />

      <PageState status={panel.status} error={panel.error} info={panel.info} onretry={() => void panel.reload()}>
        {#snippet skeleton()}
          <div class="cards-skeleton" aria-hidden="true">
            <span class="skeleton-card"></span>
          </div>
        {/snippet}

        <section
          class="panel card"
          id="section-{activeAreaObj.id}"
          data-testid="agent-config-section-{activeAreaObj.id}"
        >
          <h2 class="section-title">{activeAreaObj.label}</h2>
          {#if activeArea === 'revisions'}
            <RevisionHistoryCard {panel} />
          {:else if activeArea === 'prompt'}
            <PromptLayersCard {panel} />
          {:else if activeArea === 'llm'}
            <LLMParamsCard {panel} />
          {:else if activeArea === 'skills'}
            <SkillsCard {panel} />
          {:else if activeArea === 'mcp'}
            <McpPolicyCard {panel} />
          {:else if activeArea === 'add-connection'}
            <AddConnectionCard {panel} />
          {/if}
        </section>
      </PageState>
    </div>
  </div>
</section>

<style>
  /* Viewport-locked: the page header is fixed; below it a two-pane layout
     (left sub-nav rail + right section pane) mirrors the Settings page. Only
     the right pane scrolls internally when a section is long — the document
     never full-page-scrolls. */
  .agentcfg-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: var(--space-4);
    padding: var(--space-4);
    overflow: hidden;
  }
  .page-head {
    flex-shrink: 0;
    display: flex;
    align-items: flex-start;
    gap: var(--space-4);
    padding-bottom: var(--space-3);
    border-bottom: var(--border-hairline);
  }
  .title-block {
    flex: 1 1 auto;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .page-title {
    margin: var(--space-0);
    font-size: var(--text-xl);
    font-weight: 600;
    color: var(--color-text);
  }
  .page-sub {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    max-width: var(--size-prose-max);
  }
  .panel-layout {
    flex: 1;
    min-height: 0;
    display: flex;
    gap: var(--space-4);
    align-items: flex-start;
  }
  .rail-column {
    width: var(--size-nav);
    flex-shrink: 0;
  }
  .section-pane {
    flex: 1;
    min-width: 0;
    min-height: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    overflow-y: auto;
    scrollbar-gutter: stable;
  }
  /* The carded surface — same vocabulary as the Settings / Overview pages. */
  .card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-4);
    min-width: 0;
  }
  .section-title {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }
  /* The agent selector lives at the top of the rail (a compact, stacked
     control) so the page header stays a clean title + description. */
  .agent-select {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .field-label {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  .agent-input {
    width: 100%;
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-bg);
    color: var(--color-text);
    font-size: var(--text-sm);
    font-family: var(--font-mono);
  }
  .agent-input:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .primary {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
    background: var(--color-accent);
    border-color: var(--color-accent);
    color: var(--color-bg);
  }
  .primary:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .cards-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .skeleton-card {
    height: var(--size-sparkline-height);
    background: var(--color-surface-raised);
    border-radius: var(--radius-md);
  }
</style>
