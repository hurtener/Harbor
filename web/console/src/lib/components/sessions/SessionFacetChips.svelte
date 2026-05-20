<script lang="ts">
  // Harbor Console — Sessions-page faceted-filter chips (Phase 73c /
  // D-122). Sessions-specific component — lives in `components/sessions/`
  // per CONVENTIONS.md §3. It renders into the shared `FilterBar`'s
  // `facets` slot: Status chips + Identity (agent/user) picker + Tenants
  // facet (admin-only) + Date range + the More-filters toggle.
  //
  // Each control compiles to a field on the `SessionFilter` wire shape —
  // the chip set IS the wire filter (the "Faceted filter (sessions)"
  // glossary term). The component is controlled: it emits `onchange`
  // with the assembled filter; the page owns the state and re-invokes
  // `sessions.list`. Svelte 5 runes (D-092); design tokens only.
  import type { SessionFilter, SessionStatus } from '$lib/sessions/types.js';

  let {
    filter,
    adminScoped,
    onchange
  }: {
    /** The live filter the chips reflect. */
    filter: SessionFilter;
    /** Whether the operator holds the admin scope claim (D-079). */
    adminScoped: boolean;
    /** Emitted with the assembled filter when any chip changes. */
    onchange: (next: SessionFilter) => void;
  } = $props();

  const STATUSES: SessionStatus[] = ['running', 'paused', 'completed', 'failed'];
  const RANGES = [
    { key: '24h', label: 'Last 24h', hours: 24 },
    { key: '7d', label: 'Last 7d', hours: 24 * 7 },
    { key: 'all', label: 'All time', hours: 0 }
  ] as const;

  let moreOpen = $state(false);

  /** Toggles a status in/out of the filter and emits the new filter. */
  function toggleStatus(status: SessionStatus): void {
    const current = new Set(filter.statuses ?? []);
    if (current.has(status)) {
      current.delete(status);
    } else {
      current.add(status);
    }
    emit({ statuses: current.size > 0 ? [...current] : undefined });
  }

  /** Applies a date-range preset to the started_window facet. */
  function applyRange(hours: number): void {
    if (hours === 0) {
      emit({ started_window: undefined });
      return;
    }
    const from = new Date(Date.now() - hours * 3_600_000).toISOString();
    emit({ started_window: { from } });
  }

  function emit(patch: Partial<SessionFilter>): void {
    onchange({ ...filter, ...patch });
  }
</script>

<div class="facets" data-testid="session-facets">
  <div class="facet-group" data-testid="facet-status">
    <span class="facet-label">Status</span>
    {#each STATUSES as status (status)}
      <button
        type="button"
        class="chip"
        class:active={(filter.statuses ?? []).includes(status)}
        data-testid={`status-chip-${status}`}
        onclick={() => toggleStatus(status)}
      >
        {status}
      </button>
    {/each}
  </div>

  {#if adminScoped}
    <div class="facet-group" data-testid="facet-tenants">
      <span class="facet-label">Tenants</span>
      <input
        type="text"
        class="facet-input"
        placeholder="tenant-id (admin)"
        data-testid="tenant-input"
        value={(filter.tenant_ids ?? []).join(',')}
        onchange={(e) => {
          const raw = (e.currentTarget as HTMLInputElement).value.trim();
          emit({
            tenant_ids: raw.length > 0 ? raw.split(',').map((s) => s.trim()) : undefined
          });
        }}
      />
    </div>
  {/if}

  <div class="facet-group" data-testid="facet-identity">
    <span class="facet-label">Identity</span>
    <input
      type="text"
      class="facet-input"
      placeholder="user-id"
      data-testid="user-input"
      value={(filter.user_ids ?? []).join(',')}
      onchange={(e) => {
        const raw = (e.currentTarget as HTMLInputElement).value.trim();
        emit({ user_ids: raw.length > 0 ? raw.split(',').map((s) => s.trim()) : undefined });
      }}
    />
  </div>

  <div class="facet-group" data-testid="facet-daterange">
    <span class="facet-label">Date range</span>
    {#each RANGES as range (range.key)}
      <button
        type="button"
        class="chip"
        data-testid={`range-chip-${range.key}`}
        onclick={() => applyRange(range.hours)}
      >
        {range.label}
      </button>
    {/each}
  </div>

  <button
    type="button"
    class="chip more"
    data-testid="more-filters"
    onclick={() => (moreOpen = !moreOpen)}
  >
    More filters {moreOpen ? '▾' : '▸'}
  </button>

  {#if moreOpen}
    <div class="facet-group more-panel" data-testid="more-panel">
      <label class="check">
        <input
          type="checkbox"
          data-testid="filter-intervention"
          checked={filter.has_intervention === true}
          onchange={(e) =>
            emit({
              has_intervention: (e.currentTarget as HTMLInputElement).checked ? true : undefined
            })}
        />
        Has pending intervention
      </label>
      <label class="check">
        <input
          type="checkbox"
          data-testid="filter-failed"
          checked={filter.has_failed_task === true}
          onchange={(e) =>
            emit({
              has_failed_task: (e.currentTarget as HTMLInputElement).checked ? true : undefined
            })}
        />
        Has failed task
      </label>
      <label class="check">
        Cost above (cents)
        <input
          type="number"
          class="facet-input narrow"
          data-testid="filter-cost"
          min="0"
          value={filter.cost_above_cents ?? ''}
          onchange={(e) => {
            const v = Number((e.currentTarget as HTMLInputElement).value);
            emit({ cost_above_cents: v > 0 ? v : undefined });
          }}
        />
      </label>
    </div>
  {/if}
</div>

<style>
  .facets {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3);
  }

  .facet-group {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .facet-label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .chip {
    background: var(--color-surface);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .chip.active {
    background: var(--color-accent);
    color: var(--color-bg);
    border-color: var(--color-accent);
  }

  .chip.more {
    font-weight: 600;
  }

  .facet-input {
    background: var(--color-surface);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
  }

  .facet-input.narrow {
    width: var(--size-search-min);
    max-width: 8ch;
  }

  .more-panel {
    flex-basis: 100%;
    gap: var(--space-4);
  }

  .check {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text);
  }
</style>
