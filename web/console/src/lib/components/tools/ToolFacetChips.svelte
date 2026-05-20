<script lang="ts">
  // ToolFacetChips — the Tools-page faceted-filter chips (page-tools.md
  // §12): Transport + Approval-policy facet toggles. Tools-specific
  // content rendered into the shared `ui/FilterBar`'s `facets` slot —
  // the FilterBar owns the bar chrome, this owns only the Tools facet
  // set (D-121, CONVENTIONS.md §3). Svelte 5 runes mode (D-092).
  import type {
    ToolFilter,
    ToolTransport,
    ToolApprovalPolicy
  } from '$lib/protocol/tools.js';

  let {
    filter,
    onfilter
  }: {
    /** The current applied filter (the page's source of truth). */
    filter: ToolFilter;
    /** Emitted with the next filter when a facet toggles. */
    onfilter: (f: ToolFilter) => void;
  } = $props();

  const TRANSPORTS: ToolTransport[] = ['in-proc', 'HTTP', 'MCP', 'A2A', 'flow'];
  const APPROVALS: ToolApprovalPolicy[] = ['auto', 'gated', 'denied'];

  function toggleTransport(t: ToolTransport): void {
    const set = new Set(filter.transports ?? []);
    if (set.has(t)) {
      set.delete(t);
    } else {
      set.add(t);
    }
    onfilter({ ...filter, transports: [...set] });
  }

  function toggleApproval(p: ToolApprovalPolicy): void {
    const set = new Set(filter.approval_policies ?? []);
    if (set.has(p)) {
      set.delete(p);
    } else {
      set.add(p);
    }
    onfilter({ ...filter, approval_policies: [...set] });
  }
</script>

<div class="facets">
  <span class="label">Transport</span>
  {#each TRANSPORTS as t (t)}
    <button
      type="button"
      class="chip"
      class:chip-active={(filter.transports ?? []).includes(t)}
      data-testid="tools-facet-transport"
      data-facet-value={t}
      onclick={() => toggleTransport(t)}
    >
      {t}
    </button>
  {/each}
  <span class="label">Approval</span>
  {#each APPROVALS as p (p)}
    <button
      type="button"
      class="chip"
      class:chip-active={(filter.approval_policies ?? []).includes(p)}
      data-testid="tools-facet-approval"
      data-facet-value={p}
      onclick={() => toggleApproval(p)}
    >
      {p}
    </button>
  {/each}
</div>

<style>
  .facets {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-1);
  }

  .label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    margin-right: var(--space-1);
  }

  .chip {
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    border: var(--border-hairline);
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .chip-active {
    background: var(--color-accent-soft);
    color: var(--color-accent);
    border-color: var(--color-accent);
  }
</style>
