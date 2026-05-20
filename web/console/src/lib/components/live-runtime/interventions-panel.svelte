<script lang="ts" module>
  // Harbor Console — Live Runtime Interventions panel (Phase 73b /
  // D-126). The right-rail sub-panel listing the operator steering
  // actions taken this session — every `redirect` / `inject_context` /
  // `pause` / `resume` / `cancel` the composer dispatched plus any
  // `pause.*` / `control.*` event the SSE stream delivered.
  //
  // The panel is a Console-local view of dispatched control verbs; it
  // is NOT a Console DB shadow of a runtime entity (D-061) — it tracks
  // only what the page itself observed this session.
  //
  // Svelte 5 runes mode (D-092); design tokens only.

  /** One operator-intervention row. */
  export interface Intervention {
    /** The verb / event-type label. */
    label: string;
    /** A one-line detail. */
    detail: string;
    /** Whether the intervention succeeded. */
    ok: boolean;
  }
</script>

<script lang="ts">
  let { interventions }: { interventions: Intervention[] } = $props();
</script>

<div class="interventions" data-testid="interventions-panel">
  {#if interventions.length === 0}
    <p class="iv-empty">No operator interventions this session.</p>
  {:else}
    <ul class="iv-list">
      {#each interventions as iv, i (`${iv.label}-${i}`)}
        <li class="iv-row" class:err={!iv.ok} data-testid="intervention-row">
          <span class="iv-label">{iv.label}</span>
          <span class="iv-detail">{iv.detail}</span>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .interventions {
    font-size: var(--text-sm);
  }

  .iv-empty {
    margin: var(--space-0);
    color: var(--color-text-muted);
  }

  .iv-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .iv-row {
    display: flex;
    flex-direction: column;
  }

  .iv-label {
    color: var(--color-text);
    font-weight: 600;
  }

  .iv-row.err .iv-label {
    color: var(--color-danger);
  }

  .iv-detail {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
</style>
