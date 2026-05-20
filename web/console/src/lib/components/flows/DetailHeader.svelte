<script lang="ts">
  // Harbor Console — selected-flow detail header (Phase 73i / D-117).
  //
  // The mid-page header for a selected flow: name + version chip +
  // health pill + `Run this flow ▶` + `Save snapshot` (Console-local)
  // + `Compare versions` (Console-local diff). There is NO authoring
  // affordance — `Add node` / `New flow` do not render (D-063).
  import type { FlowDescription } from '$lib/flows/types';

  interface Props {
    description: FlowDescription;
    canRun: boolean;
    onrun: () => void;
    onsavesnapshot: () => void;
    oncompare: () => void;
  }

  const {
    description,
    canRun,
    onrun,
    onsavesnapshot,
    oncompare,
  }: Props = $props();

  const health = $derived.by(() => {
    const r = description.flow.success_rate;
    if (description.flow.runs_24h === 0) {
      return { label: 'No runs', tone: 'muted' };
    }
    if (r >= 0.95) {
      return { label: 'Healthy', tone: 'success' };
    }
    if (r >= 0.7) {
      return { label: 'Degraded', tone: 'warning' };
    }
    return { label: 'Errored', tone: 'danger' };
  });
</script>

<header class="detail-header" data-testid="flow-detail-header">
  <div class="title">
    <h2 data-testid="detail-flow-name">{description.flow.name}</h2>
    {#if description.flow.version}
      <span class="version-chip">{description.flow.version}</span>
    {/if}
    <span class={`pill tone-${health.tone}`} data-testid="detail-health">
      {health.label}
    </span>
  </div>
  <div class="actions">
    <button
      class="primary"
      data-testid="detail-run"
      disabled={!canRun}
      title={canRun
        ? 'Run this flow'
        : 'Running a flow requires the flows.run scope claim'}
      onclick={onrun}
    >
      Run this flow ▶
    </button>
    <button class="ghost" data-testid="detail-save-snapshot" onclick={onsavesnapshot}>
      Save snapshot
    </button>
    <button class="ghost" data-testid="detail-compare" onclick={oncompare}>
      Compare versions
    </button>
  </div>
</header>

<style>
  .detail-header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .title {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  h2 {
    font-size: var(--text-lg);
    margin: var(--space-0);
  }

  .version-chip {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .pill {
    font-size: var(--text-xs);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .tone-success {
    background: var(--color-success);
    color: var(--color-bg);
  }

  .tone-warning {
    background: var(--color-warning);
    color: var(--color-bg);
  }

  .tone-danger {
    background: var(--color-danger);
    color: var(--color-bg);
  }

  .tone-muted {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
  }

  .actions {
    display: flex;
    gap: var(--space-2);
  }

  .primary {
    background: var(--color-accent);
    color: var(--color-bg);
    border: none;
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .primary:disabled {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    cursor: not-allowed;
  }

  .ghost {
    background: none;
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }
</style>
