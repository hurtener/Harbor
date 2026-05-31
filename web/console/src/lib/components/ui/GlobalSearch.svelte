<script lang="ts">
  // Harbor Console — global ⌘K search launcher (Phase 108b chrome).
  //
  // The top-bar search affordance (mock: "Search anything… ⌘K"). Renders
  // the in-bar trigger pill AND, when open, an overlay command palette.
  // It calls the SHIPPED `search.query` Protocol method (Phase 72c) through
  // the typed `HarborClient` — no hand-rolled fetch (CONVENTIONS.md §6),
  // no new Protocol method (CLAUDE.md §13). Every row is real Runtime data;
  // nothing is synthesised. When no Runtime is attached the trigger is
  // disabled-with-tooltip (D-160), never a fake result.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import { onMount, tick } from 'svelte';
  import { goto } from '$app/navigation';
  import Search from '@lucide/svelte/icons/search';
  import CornerDownLeft from '@lucide/svelte/icons/corner-down-left';
  import { resolveConnection, isDisconnected, DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import { HarborClient } from '$lib/protocol/harbor.js';
  import type { SearchResultRow, SearchIndex } from '$lib/protocol/search.js';
  import { ProtocolError } from '$lib/protocol/errors.js';

  let open = $state(false);
  let query = $state('');
  let rows = $state<SearchResultRow[]>([]);
  let loading = $state(false);
  let errorMsg = $state('');
  let active = $state(0);
  let inputEl = $state<HTMLInputElement | null>(null);
  let seq = 0; // guards against out-of-order responses
  let debounce: ReturnType<typeof setTimeout> | undefined;

  const disconnected = $derived(isDisconnected());

  // Human label per index (the result-group heading).
  const INDEX_LABEL: Record<SearchIndex, string> = {
    sessions: 'Sessions',
    tasks: 'Tasks',
    events: 'Events',
    artifacts: 'Artifacts'
  };

  // Where a picked row navigates. Sessions have a detail route; the other
  // three indexes have no per-entity detail route in V1, so they route to
  // their list page (honest — not a fabricated deep link).
  function hrefFor(row: SearchResultRow): string {
    switch (row.index) {
      case 'sessions':
        return `/sessions/${encodeURIComponent(row.id)}`;
      case 'tasks':
        return '/tasks';
      case 'events':
        return '/events';
      case 'artifacts':
        return '/artifacts';
      default:
        return '/overview';
    }
  }

  function rowLabel(row: SearchResultRow): string {
    if (row.preview && row.preview.length > 0) return row.preview;
    if (row.ref?.filename) return row.ref.filename;
    return row.id;
  }

  async function openPalette() {
    if (disconnected) return;
    open = true;
    await tick();
    inputEl?.focus();
  }

  function closePalette() {
    open = false;
    query = '';
    rows = [];
    errorMsg = '';
    active = 0;
  }

  async function runSearch(q: string) {
    const conn = resolveConnection();
    if (conn === null) return;
    const mine = ++seq;
    loading = true;
    errorMsg = '';
    try {
      const client = new HarborClient({ connection: conn });
      const resp = await client.search.query({ query: q, page_size: 8 });
      if (mine !== seq) return; // a newer query superseded this one
      rows = resp.rows ?? [];
      active = 0;
    } catch (e) {
      if (mine !== seq) return;
      rows = [];
      errorMsg =
        e instanceof ProtocolError ? `${e.code}: ${e.message}` : 'Search failed';
    } finally {
      if (mine === seq) loading = false;
    }
  }

  function onInput() {
    clearTimeout(debounce);
    const q = query;
    debounce = setTimeout(() => void runSearch(q), 180);
  }

  function choose(row: SearchResultRow) {
    const href = hrefFor(row);
    closePalette();
    void goto(href);
  }

  function onKeydown(e: KeyboardEvent) {
    // ⌘K / Ctrl+K toggles the palette from anywhere.
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
      e.preventDefault();
      if (open) closePalette();
      else void openPalette();
      return;
    }
    if (!open) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      closePalette();
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (rows.length > 0) active = (active + 1) % rows.length;
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (rows.length > 0) active = (active - 1 + rows.length) % rows.length;
    } else if (e.key === 'Enter') {
      e.preventDefault();
      if (rows[active]) choose(rows[active]);
    }
  }

  onMount(() => {
    window.addEventListener('keydown', onKeydown);
    return () => window.removeEventListener('keydown', onKeydown);
  });
</script>

<button
  type="button"
  class="search-trigger"
  data-testid="global-search-trigger"
  onclick={openPalette}
  disabled={disconnected}
  title={disconnected ? DISCONNECTED_TOOLTIP : 'Search (⌘K)'}
  aria-label="Search"
>
  <Search size={16} aria-hidden="true" />
  <span class="placeholder">Search anything…</span>
  <kbd>⌘K</kbd>
</button>

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="overlay" data-testid="global-search-overlay" onclick={closePalette}>
    <div
      class="palette"
      role="dialog"
      aria-modal="true"
      aria-label="Global search"
      tabindex="-1"
      onclick={(e) => e.stopPropagation()}
    >
      <div class="palette-input">
        <Search size={18} aria-hidden="true" />
        <input
          bind:this={inputEl}
          bind:value={query}
          oninput={onInput}
          type="text"
          placeholder="Search sessions, tasks, events, artifacts…"
          data-testid="global-search-input"
          autocomplete="off"
          spellcheck="false"
        />
        {#if loading}<span class="hint">searching…</span>{/if}
      </div>

      <div class="palette-results" data-testid="global-search-results">
        {#if errorMsg}
          <p class="state error" data-testid="global-search-error">{errorMsg}</p>
        {:else if query.length === 0}
          <p class="state muted">Type to search across the Runtime.</p>
        {:else if !loading && rows.length === 0}
          <p class="state muted" data-testid="global-search-empty">
            No matches for “{query}”.
          </p>
        {:else}
          <ul>
            {#each rows as row, i (row.index + ':' + row.id)}
              <li>
                <button
                  type="button"
                  class="result"
                  class:active={i === active}
                  data-testid="global-search-result"
                  onclick={() => choose(row)}
                  onmouseenter={() => (active = i)}
                >
                  <span class="result-index">{INDEX_LABEL[row.index]}</span>
                  <span class="result-label">{rowLabel(row)}</span>
                  {#if i === active}
                    <span class="result-enter"><CornerDownLeft size={14} /></span>
                  {/if}
                </button>
              </li>
            {/each}
          </ul>
        {/if}
      </div>
    </div>
  </div>
{/if}

<style>
  .search-trigger {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-width: var(--size-search-min);
    padding: var(--space-1) var(--space-2);
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .search-trigger:hover:not(:disabled) {
    border-color: var(--color-accent);
    color: var(--color-text);
  }

  .search-trigger:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .search-trigger .placeholder {
    flex: 1;
    text-align: left;
  }

  .search-trigger kbd {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    padding: var(--space-0) var(--space-1);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    color: var(--color-text-muted);
  }

  .overlay {
    position: fixed;
    inset: 0;
    background: var(--color-overlay);
    display: flex;
    align-items: flex-start;
    justify-content: center;
    padding-top: var(--space-10);
  }

  .palette {
    width: var(--size-palette-width);
    max-width: 90vw;
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    overflow: hidden;
  }

  .palette-input {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-3) var(--space-4);
    border-bottom: var(--border-hairline);
    color: var(--color-text-muted);
  }

  .palette-input input {
    flex: 1;
    background: transparent;
    border: none;
    outline: none;
    color: var(--color-text);
    font-size: var(--text-base);
    font-family: var(--font-sans);
  }

  .palette-input .hint {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .palette-results {
    max-height: var(--size-palette-max-height);
    overflow-y: auto;
  }

  .palette-results .state {
    margin: var(--space-0);
    padding: var(--space-4);
    font-size: var(--text-sm);
  }

  .palette-results .state.muted {
    color: var(--color-text-muted);
  }

  .palette-results .state.error {
    color: var(--color-danger);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .palette-results ul {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-1);
  }

  .result {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    width: 100%;
    padding: var(--space-2) var(--space-3);
    background: transparent;
    border: none;
    border-radius: var(--radius-sm);
    color: var(--color-text);
    font-size: var(--text-sm);
    text-align: left;
    cursor: pointer;
  }

  .result.active {
    background: var(--color-accent-soft);
  }

  .result-index {
    flex-shrink: 0;
    min-width: var(--size-search-group-width);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wider);
    color: var(--color-text-muted);
  }

  .result-label {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .result-enter {
    flex-shrink: 0;
    color: var(--color-accent);
  }
</style>
