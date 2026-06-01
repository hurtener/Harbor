<script lang="ts">
  // Events page — faceted filter chips (Phase 73g / D-125).
  //
  // The sub-header faceted filter chips: Event type ▾, Tenant ▾, User ▾,
  // Session ▾, Run ▾, Window ▾ (page-events.md §12). Each chip drives a
  // Console-local facet-state change; none fires a Protocol mutation
  // (the page re-opens the subscription). The `Tenant ▾` facet is gated
  // on the operator's admin scope (D-079) — a non-admin sees only their
  // own tenant, disabled-with-tooltip. Page-specific component:
  // `components/events/` per CONVENTIONS.md §3. Svelte 5 runes (D-092);
  // tokens only.
  import { eventTypesByCategory } from '$lib/events/taxonomy.js';
  import type { EventFacetState } from '$lib/events/filters.js';
  import type { TimeWindow } from '$lib/protocol/events.js';
  import { WINDOW_SPEC } from '$lib/protocol/events.js';

  let {
    facets,
    isAdmin,
    ownTenant,
    onapply
  }: {
    /** The current Console-local facet state. */
    facets: EventFacetState;
    /** True when the operator holds a cross-tenant scope claim (D-079). */
    isAdmin: boolean;
    /** The operator's own tenant — the only one a non-admin may pin. */
    ownTenant: string;
    /** Emitted with the next facet state when a chip changes. */
    onapply: (next: EventFacetState) => void;
  } = $props();

  /** The window options, in mockup order. */
  const WINDOWS: TimeWindow[] = ['5m', '1h', '24h', '7d'];

  /** Whether the Event-type dropdown is open. */
  let typeMenuOpen = $state(false);

  function toggleType(type: string): void {
    const has = facets.eventTypes.includes(type);
    onapply({
      ...facets,
      eventTypes: has
        ? facets.eventTypes.filter((t) => t !== type)
        : [...facets.eventTypes, type]
    });
  }

  function setWindow(w: TimeWindow): void {
    onapply({ ...facets, window: w });
  }

  function setIdentity(axis: 'tenant' | 'user' | 'session' | 'run', value: string): void {
    onapply({ ...facets, [axis]: value.trim() === '' ? null : value.trim() });
  }
</script>

<div class="filter-chips" data-testid="events-filter-chips">
  <!-- Event type ▾ -->
  <div class="chip-group">
    <button
      type="button"
      class="facet-chip"
      class:active={facets.eventTypes.length > 0}
      data-testid="facet-event-type"
      aria-expanded={typeMenuOpen}
      onclick={() => (typeMenuOpen = !typeMenuOpen)}
    >
      Event type{facets.eventTypes.length > 0 ? ` (${facets.eventTypes.length})` : ''} ▾
    </button>
    {#if typeMenuOpen}
      <div class="type-menu" data-testid="events-type-menu" role="group" aria-label="Event types">
        {#each eventTypesByCategory() as group (group.category)}
          <p class="type-group-label">{group.category}</p>
          {#each group.types as type (type)}
            <label class="type-option">
              <input
                type="checkbox"
                checked={facets.eventTypes.includes(type)}
                data-testid={`type-opt-${type}`}
                onchange={() => toggleType(type)}
              />
              <span>{type}</span>
            </label>
          {/each}
        {/each}
      </div>
    {/if}
  </div>

  <!-- Tenant ▾ (scope-gated, D-079) -->
  <label class="chip-input">
    <span class="chip-label">Tenant</span>
    <input
      type="text"
      class="facet-text"
      data-testid="facet-tenant"
      placeholder={isAdmin ? 'any tenant' : ownTenant}
      value={facets.tenant ?? (isAdmin ? '' : ownTenant)}
      disabled={!isAdmin}
      title={isAdmin
        ? 'Cross-tenant fan-in — emits audit.admin_scope_used'
        : 'Cross-tenant event viewing requires the admin scope claim'}
      oninput={(e) => setIdentity('tenant', (e.currentTarget as HTMLInputElement).value)}
    />
  </label>

  {#each [{ axis: 'user', label: 'User' }, { axis: 'session', label: 'Session' }, { axis: 'run', label: 'Run' }] as f (f.axis)}
    <label class="chip-input">
      <span class="chip-label">{f.label}</span>
      <input
        type="text"
        class="facet-text"
        data-testid={`facet-${f.axis}`}
        placeholder="any"
        value={facets[f.axis as 'user' | 'session' | 'run'] ?? ''}
        oninput={(e) =>
          setIdentity(
            f.axis as 'user' | 'session' | 'run',
            (e.currentTarget as HTMLInputElement).value
          )}
      />
    </label>
  {/each}

  <!-- Window ▾ -->
  <div class="chip-group window-group" data-testid="facet-window">
    {#each WINDOWS as w (w)}
      <button
        type="button"
        class="facet-chip"
        class:active={facets.window === w}
        data-testid={`window-${w}`}
        aria-pressed={facets.window === w}
        onclick={() => setWindow(w)}
      >
        {WINDOW_SPEC[w].label}
      </button>
    {/each}
  </div>
</div>

<style>
  .filter-chips {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .chip-group {
    position: relative;
    display: flex;
    gap: var(--space-1);
  }

  .facet-chip {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .facet-chip.active {
    border-color: var(--color-accent);
    color: var(--color-accent);
  }

  .facet-chip:hover {
    border-color: var(--color-accent);
  }

  .type-menu {
    position: absolute;
    top: 100%;
    left: var(--space-0);
    z-index: 1;
    margin-top: var(--space-1);
    max-height: var(--size-graph-max-height);
    overflow-y: auto;
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-2);
    min-width: var(--size-search-min);
  }

  .type-group-label {
    margin: var(--space-2) var(--space-0) var(--space-1);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    color: var(--color-text-muted);
  }

  .type-option {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-0);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .chip-input {
    display: flex;
    align-items: center;
    gap: var(--space-1);
  }

  .chip-label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .facet-text {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    width: var(--size-facet-input);
  }

  .facet-text:disabled {
    color: var(--color-text-muted);
    cursor: not-allowed;
  }

  .window-group {
    margin-left: auto;
  }
</style>
