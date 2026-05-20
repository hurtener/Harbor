<script lang="ts">
  // Sub-header strip — Phase 73j / D-118.
  //
  // Saved-filter chips on the left + faceted filter controls + a
  // content-search box; Refresh + Export on the right. The saved-filter
  // chips are a typed wrapper over the Phase 72h `saved_filters` Console
  // DB table (D-061 — Console-local; never a runtime entity). Export is
  // a Console-local snapshot of the current filtered page — NO Protocol
  // mutation (D-061). Svelte 5 runes mode (D-092).
  import type { MemorySavedFilter } from '$lib/db/saved_filters_memory';

  let {
    savedFilters,
    contentSearch,
    scopeFacet,
    driverFacet,
    onContentSearch,
    onScopeFacet,
    onDriverFacet,
    onApplySaved,
    onRefresh,
    onExport
  }: {
    savedFilters: MemorySavedFilter[];
    contentSearch: string;
    scopeFacet: string;
    driverFacet: string;
    onContentSearch: (v: string) => void;
    onScopeFacet: (v: string) => void;
    onDriverFacet: (v: string) => void;
    onApplySaved: (id: string) => void;
    onRefresh: () => void;
    onExport: (format: 'ndjson' | 'csv') => void;
  } = $props();
</script>

<div class="strip">
  <div class="left">
    {#if savedFilters.length > 0}
      <div class="saved" aria-label="Saved filters">
        {#each savedFilters as sf (sf.id)}
          <button type="button" class="chip" onclick={() => onApplySaved(sf.id)}>
            {sf.name}
          </button>
        {/each}
      </div>
    {/if}
    <label class="facet">
      <span>Scope</span>
      <select
        value={scopeFacet}
        onchange={(e) => onScopeFacet(e.currentTarget.value)}
      >
        <option value="">All</option>
        <option value="session">session</option>
        <option value="user">user</option>
        <option value="tenant">tenant</option>
      </select>
    </label>
    <label class="facet">
      <span>Driver</span>
      <select
        value={driverFacet}
        onchange={(e) => onDriverFacet(e.currentTarget.value)}
      >
        <option value="">All</option>
        <option value="inmem">inmem</option>
        <option value="sqlite">sqlite</option>
        <option value="postgres">postgres</option>
      </select>
    </label>
    <label class="search">
      <span class="visually-hidden">Content search</span>
      <input
        type="search"
        placeholder="Search content…"
        value={contentSearch}
        oninput={(e) => onContentSearch(e.currentTarget.value)}
      />
    </label>
  </div>
  <div class="right">
    <button type="button" class="action" onclick={onRefresh}>Refresh</button>
    <button type="button" class="action" onclick={() => onExport('ndjson')}>
      Export NDJSON
    </button>
    <button type="button" class="action" onclick={() => onExport('csv')}>
      Export CSV
    </button>
  </div>
</div>

<style>
  .strip {
    display: flex;
    justify-content: space-between;
    gap: var(--space-4);
    flex-wrap: wrap;
  }

  .left,
  .right {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .saved {
    display: flex;
    gap: var(--space-1);
  }

  .chip {
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-lg);
    border: var(--border-width-hairline) solid var(--color-accent);
    background: var(--color-surface);
    color: var(--color-text);
    cursor: pointer;
  }

  .facet {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  select,
  input {
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
    background: var(--color-surface);
    color: var(--color-text);
    border: var(--border-width-hairline) solid var(--color-border);
    border-radius: var(--radius-sm);
  }

  .action {
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-3);
    background: var(--color-surface);
    color: var(--color-text);
    border: var(--border-width-hairline) solid var(--color-border);
    border-radius: var(--radius-sm);
    cursor: pointer;
  }

  .action:hover {
    background: var(--color-surface-raised);
  }

  .visually-hidden {
    position: absolute;
    width: var(--size-px);
    height: var(--size-px);
    overflow: hidden;
    clip-path: inset(50%);
  }
</style>
