<script lang="ts">
  // Harbor Console — Sessions detail-view Recent Interventions card
  // (Phase 73c / D-122). Renders the capped `recent_interventions`
  // slice from `sessions.inspect` — per-intervention type + reason +
  // outcome + age (mockup §12). Sessions-specific component. Svelte 5
  // runes (D-092); design tokens only.
  import type { InterventionSummary } from '$lib/sessions/types.js';
  import { formatRelative } from '$lib/sessions/format.js';

  let { interventions }: { interventions: InterventionSummary[] } = $props();
</script>

{#if interventions.length === 0}
  <p class="empty" data-testid="interventions-empty">
    No interventions recorded for this session.
  </p>
{:else}
  <ul class="list" data-testid="recent-interventions">
    {#each interventions as iv, i (i)}
      <li class="item">
        <div class="row-1">
          <span class="type">{iv.type}</span>
          <span class="age">{formatRelative(iv.occurred_at)}</span>
        </div>
        <p class="reason">{iv.reason}</p>
        <span class="outcome">{iv.outcome}</span>
      </li>
    {/each}
  </ul>
{/if}

<style>
  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    max-height: var(--layout-rail-list-max);
    overflow-y: auto;
  }

  .item {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
    padding-bottom: var(--space-2);
  }

  .row-1 {
    display: flex;
    justify-content: space-between;
    font-size: var(--text-xs);
  }

  .type {
    font-weight: 600;
    color: var(--color-text);
  }

  .age {
    color: var(--color-text-muted);
  }

  .reason {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .outcome {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
