<script lang="ts">
  // SubHeaderStrip — the row above the catalog table (page-tools.md
  // §12): saved-filter chips, faceted filter chips, a free-text search
  // box, and a client-side Export control. Svelte 5 runes mode (D-092).
  import type {
    ToolFilter,
    ToolTransport,
    ToolApprovalPolicy
  } from '$lib/protocol/tools.js';
  import type { ToolsSavedFilter } from '$lib/db/saved_filters_tools.js';

  let {
    filter,
    savedFilters = [],
    onfilter,
    onapplysaved,
    onsavefilter,
    onexport
  }: {
    filter: ToolFilter;
    savedFilters?: ToolsSavedFilter[];
    onfilter: (f: ToolFilter) => void;
    onapplysaved: (id: string) => void;
    onsavefilter: (name: string) => void;
    onexport: (format: 'csv' | 'json') => void;
  } = $props();

  const TRANSPORTS: ToolTransport[] = ['in-proc', 'HTTP', 'MCP', 'A2A', 'flow'];
  const APPROVALS: ToolApprovalPolicy[] = ['auto', 'gated', 'denied'];

  // searchText starts blank; the operator types into it and Apply
  // pushes it into the filter. It is intentionally NOT bound to
  // `filter.search` — the filter is the parent's source of truth and
  // is only mutated on an explicit Apply / Clear.
  let searchText = $state('');
  let saveName = $state('');

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

  function submitSearch(): void {
    onfilter({ ...filter, search: searchText.trim() });
  }

  function clearAll(): void {
    searchText = '';
    onfilter({});
  }

  function saveCurrent(): void {
    const name = saveName.trim();
    if (name.length > 0) {
      onsavefilter(name);
      saveName = '';
    }
  }
</script>

<div class="strip" data-testid="tools-subheader">
  <div class="row">
    <span class="label">Saved filters</span>
    {#if savedFilters.length === 0}
      <span class="muted">none</span>
    {:else}
      {#each savedFilters as sf (sf.id)}
        <button
          type="button"
          class="chip chip-saved"
          data-testid="tools-saved-filter-chip"
          onclick={() => onapplysaved(sf.id)}
        >
          {sf.name}
        </button>
      {/each}
    {/if}
    <input
      class="save-input"
      type="text"
      placeholder="Save current as…"
      bind:value={saveName}
      data-testid="tools-save-filter-name"
      onkeydown={(e) => e.key === 'Enter' && saveCurrent()}
    />
    <button
      type="button"
      class="btn"
      data-testid="tools-save-filter"
      onclick={saveCurrent}
    >
      Save
    </button>
  </div>

  <div class="row">
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

  <div class="row">
    <input
      class="search"
      type="search"
      placeholder="Search tools…"
      bind:value={searchText}
      data-testid="tools-search"
      onkeydown={(e) => e.key === 'Enter' && submitSearch()}
    />
    <button type="button" class="btn" data-testid="tools-search-apply" onclick={submitSearch}>
      Apply
    </button>
    <button type="button" class="btn" data-testid="tools-filter-clear" onclick={clearAll}>
      Clear
    </button>
    <span class="spacer"></span>
    <button type="button" class="btn" data-testid="tools-export-csv" onclick={() => onexport('csv')}>
      Export CSV
    </button>
    <button
      type="button"
      class="btn"
      data-testid="tools-export-json"
      onclick={() => onexport('json')}
    >
      Export JSON
    </button>
  </div>
</div>

<style>
  .strip {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    background: var(--color-surface);
    border: var(--border-width-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  .row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .label {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--border-width-thin);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .spacer {
    flex: 1;
  }

  .chip {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-sm);
    border: var(--border-width-thin) solid var(--color-border);
    background: var(--color-surface-raised);
    color: var(--color-text);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .chip-active {
    background: var(--color-accent-soft);
    color: var(--color-accent);
    border-color: var(--color-accent);
  }

  .chip-saved {
    color: var(--color-accent);
  }

  .btn {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-sm);
    border: var(--border-width-thin) solid var(--color-border);
    background: var(--color-surface-raised);
    color: var(--color-text);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .search,
  .save-input {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-sm);
    border: var(--border-width-thin) solid var(--color-border);
    background: var(--color-bg);
    color: var(--color-text);
    font-size: var(--text-sm);
  }

  .search {
    min-width: var(--layout-detail-min-height);
  }
</style>
