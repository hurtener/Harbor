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
  import RevisionHistoryCard from '$lib/components/agentconfig/RevisionHistoryCard.svelte';
  import SkillsCard from '$lib/components/agentconfig/SkillsCard.svelte';
  import McpPolicyCard from '$lib/components/agentconfig/McpPolicyCard.svelte';
  import PromptLayersCard from '$lib/components/agentconfig/PromptLayersCard.svelte';
  import AddConnectionCard from '$lib/components/agentconfig/AddConnectionCard.svelte';
  import { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';

  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  const panel = new AgentConfigPanelState();

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
  <section class="panel card header-card">
    <div class="header-row">
      <div class="title-block">
        <h1 class="page-title">Agent configuration</h1>
        <p class="page-sub">
          Versioned, live control of one agent&rsquo;s prompt, skills, MCP policy,
          and connections. Changes apply on the agent&rsquo;s next run.
        </p>
      </div>
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
    </div>
  </section>

  <div class="panel-scroll">
    <PageState status={panel.status} error={panel.error} info={panel.info} onretry={() => void panel.reload()}>
      {#snippet skeleton()}
        <div class="cards-skeleton" aria-hidden="true">
          <span class="skeleton-card"></span>
          <span class="skeleton-card"></span>
          <span class="skeleton-card"></span>
        </div>
      {/snippet}

      <div class="cards-grid">
        <section class="panel card" data-testid="agent-config-section-revisions">
          <h2 class="section-title">Revision history</h2>
          <RevisionHistoryCard {panel} />
        </section>

        <section class="panel card" data-testid="agent-config-section-prompt">
          <h2 class="section-title">Layered prompt</h2>
          <PromptLayersCard {panel} />
        </section>

        <section class="panel card" data-testid="agent-config-section-skills">
          <h2 class="section-title">Skills</h2>
          <SkillsCard {panel} />
        </section>

        <section class="panel card" data-testid="agent-config-section-mcp">
          <h2 class="section-title">MCP policy</h2>
          <McpPolicyCard {panel} />
        </section>

        <section class="panel card wide" data-testid="agent-config-section-add-connection">
          <h2 class="section-title">Add MCP connection</h2>
          <AddConnectionCard {panel} />
        </section>
      </div>
    </PageState>
  </div>
</section>

<style>
  /* Viewport-locked: the header is fixed; the cards canvas scrolls
     internally (the Agents-108l / Events-108h pattern). */
  .agentcfg-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow: hidden;
  }
  .card {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-4);
    min-width: 0;
  }
  .header-card {
    flex-shrink: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .header-row {
    display: flex;
    align-items: flex-end;
    justify-content: space-between;
    gap: var(--space-4);
    flex-wrap: wrap;
  }
  .title-block {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
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
    max-width: var(--size-session-max-width);
  }
  .agent-select {
    display: flex;
    align-items: flex-end;
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
    width: var(--size-input-compact);
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
  .panel-scroll {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    scrollbar-gutter: stable;
  }
  .cards-grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-3);
    align-items: start;
  }
  .cards-grid .wide {
    grid-column: 1 / -1;
  }
  .section-title {
    margin: var(--space-0) var(--space-0) var(--space-3);
    font-size: var(--text-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }
  .cards-skeleton {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--space-3);
  }
  .skeleton-card {
    height: var(--size-sparkline-height);
    background: var(--color-surface-raised);
    border-radius: var(--radius-md);
  }
</style>
