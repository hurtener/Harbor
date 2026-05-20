<script lang="ts">
  // Harbor Console — Sessions detail-view bottom-dock tab strip (Phase
  // 73c / D-122). The five-tab strip per mockup §12: Trajectory |
  // Events | Cost History | Control History | Interventions.
  //
  // The tab strip is the per-session detail pane shared in shape with
  // Live Runtime (Brief 11 §"Per-task detail pane"). Phase 73c ships
  // the tab strip + each tab's placeholder content; the live data each
  // tab renders flows from the Console's own event-stream subscription
  // on the detail route (the page consumes `state.*` / `pause.*` /
  // `control.*` / `llm.cost.recorded` events filtered to the session —
  // page spec §5). The tab strip is the navigable surface; the
  // event-stream wiring lands with the Live Runtime page (Phase 73b)
  // whose shared detail-pane components this strip mirrors.
  //
  // Sessions-specific component. Svelte 5 runes (D-092); tokens only.

  type Tab = {
    key: string;
    label: string;
    blurb: string;
  };

  const TABS: Tab[] = [
    {
      key: 'trajectory',
      label: 'Trajectory',
      blurb:
        'The chronological planner-step timeline (decision → tool → result) for the session’s primary run.'
    },
    {
      key: 'events',
      label: 'Events',
      blurb: 'The full event log filtered to this session’s (tenant, user, session) scope.'
    },
    {
      key: 'cost',
      label: 'Cost History',
      blurb: 'The per-step LLM cost rollup, aggregated from llm.cost.recorded events.'
    },
    {
      key: 'control',
      label: 'Control History',
      blurb: 'The control.received / control.applied / control.rejected audit for the session.'
    },
    {
      key: 'interventions',
      label: 'Interventions',
      blurb: 'The complete pause / approval history for the session.'
    }
  ];

  let active = $state(TABS[0].key);
  const activeTab = $derived(TABS.find((t) => t.key === active) ?? TABS[0]);
</script>

<section class="dock" data-testid="bottom-dock">
  <div class="tab-strip" role="tablist" aria-label="Session detail tabs">
    {#each TABS as tab (tab.key)}
      <button
        type="button"
        role="tab"
        class="tab"
        class:active={active === tab.key}
        aria-selected={active === tab.key}
        data-testid={`dock-tab-${tab.key}`}
        onclick={() => (active = tab.key)}
      >
        {tab.label}
      </button>
    {/each}
  </div>
  <div class="tab-panel" role="tabpanel" data-testid="dock-panel">
    <p class="panel-title">{activeTab.label}</p>
    <p class="panel-blurb">{activeTab.blurb}</p>
  </div>
</section>

<style>
  .dock {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .tab-strip {
    display: flex;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
    padding: var(--space-2) var(--space-2) var(--space-0);
  }

  .tab {
    background: none;
    color: var(--color-text-muted);
    border: none;
    border-bottom: var(--border-hairline);
    border-bottom-color: transparent;
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .tab.active {
    color: var(--color-text);
    border-bottom-color: var(--color-accent);
    font-weight: 600;
  }

  .tab-panel {
    padding: var(--space-3);
  }

  .panel-title {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .panel-blurb {
    margin: var(--space-1) var(--space-0) var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
