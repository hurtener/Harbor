<script lang="ts">
  // Harbor Console — Background Jobs page (`/background-jobs`) — Phase 108j
  // rebuild (D-182; supersedes the Phase 73h / D-128 pre-chrome layout).
  //
  // The queue view for planner-spawned background tasks — the focused
  // `tasks.list` projection with `kinds: ['background']`. Phase 108j
  // rethemes it to the carded, viewport-locked Events-108h composition
  // (TABLE-primary on the left + a right-rail detail on the right; the
  // table stays visible, the rail shows the selected job or an idle hint —
  // NOT a Tasks-style mode-switch), deepens the right rail to full mock
  // fidelity, and refactors the ~801-line king file into a
  // `BackgroundJobsPageState` controller + pure `derive.ts` /
  // `orphan-detector.ts` projections + focused rail components.
  //
  // Every datum + action is real-wired (PAGE-POLISH §3 — live-verified):
  //   - queue rows + per-status aggregates ← `tasks.list` (kinds:[background])
  //   - per-job Details / Parent task ← `tasks.get`
  //   - per-job Progress ← `task.progress` + derived ETA + the run's
  //     `task.*` lifecycle timeline (JobProgressTab)
  //   - per-job Events / Control History ← the RUN-scoped `events.subscribe`
  //     projection owned by `TaskRunStream` (reused from Tasks, NOT forked)
  //   - per-job Artifacts ← `artifacts.list` scoped to the job's run id
  //   - Related Sessions ← `tasks.list?group_id` siblings
  //   - bulk Cancel / Pause / Resume / Prioritize ← the shipped Phase 54
  //     control verbs via `client.control.*`, control-scope gated (D-066)
  //   - the `AwaitTask` orphan badge ← the pure Console-side detector (D-128)
  //
  // Honest states for the genuine V1 gaps (PAGE-POLISH §1): ETA is the
  // planner's own progress projected (or "Unknown"); the type badge is
  // derived from tags/description (or "Job"); spawned-by shows the
  // parent_task_id (no fabricated agent); artifacts / related-sessions are
  // honest-empty when none; the run-scoped SSE is live-only so the Events /
  // Control tabs are honest-empty for a quiet run; free-text search is a
  // Console-local substring match (no shipped `search.tasks`).
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient +
  // connection.ts only — no hand-rolled fetch (CONVENTIONS.md §6).
  import { onMount } from 'svelte';
  import { FilterBar, BulkActionBar, Pagination, PageState } from '$lib/components/ui/index.js';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import QueueTable from '$lib/components/background-jobs/QueueTable.svelte';
  import BulkToolbar from '$lib/components/background-jobs/BulkToolbar.svelte';
  import JobDetailRail from '$lib/components/background-jobs/JobDetailRail.svelte';
  import SavedFilterChips from '$lib/components/background-jobs/SavedFilterChips.svelte';
  import { BackgroundJobsPageState, STATUS_FACETS } from '$lib/background-jobs/state.svelte.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';
  import type { TaskStatus } from '$lib/protocol/tasks.js';

  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  const state = new BackgroundJobsPageState();
  const disconnected = $derived(state.disconnected);

  onMount(() => {
    state.load(injectedClient);
    return () => state.close();
  });
</script>

<svelte:head>
  <title>Background Jobs · Harbor Console</title>
</svelte:head>

<section class="bg-page" data-testid="background-jobs-page">
  <section class="panel card filter-card">
    <FilterBar>
      {#snippet saved()}
        <SavedFilterChips
          activeDerivedId={state.activeDerived?.id ?? null}
          savedViews={state.savedViews}
          activeSavedId={state.activeSavedId}
          onderived={(chip) => state.applyDerivedChip(chip)}
          onsaved={(id) => state.applySavedView(id)}
          ondeletesaved={(id) => void state.deleteSavedView(id)}
        />
        <input
          class="bar-input save-input"
          type="text"
          placeholder="Save current as…"
          bind:value={state.saveName}
          data-testid="bg-save-filter-name"
          disabled={state.savedFilters === null || disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onkeydown={(e) => e.key === 'Enter' && void state.saveCurrentFilter()}
        />
        <button
          type="button"
          class="bar-action"
          data-testid="bg-save-filter"
          disabled={state.savedFilters === null || state.saveName.trim().length === 0 || disconnected}
          title={disconnected
            ? DISCONNECTED_TOOLTIP
            : state.savedFilters === null
              ? 'Console-local saved-view store unavailable'
              : undefined}
          onclick={() => void state.saveCurrentFilter()}
        >
          Save view
        </button>
      {/snippet}

      {#snippet facets()}
        <div class="facet-chips" data-testid="bg-facets">
          {#each STATUS_FACETS as s (s)}
            <button
              type="button"
              class="chip"
              class:on={(state.facetFilter.statuses ?? []).includes(s)}
              data-testid={`bg-facet-status-${s}`}
              disabled={disconnected}
              title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
              onclick={() => state.toggleStatusFacet(s as TaskStatus)}
            >
              {s}
            </button>
          {/each}
          <button
            type="button"
            class="chip"
            class:on={state.approvalFacet === true}
            data-testid="bg-facet-approval"
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onclick={() => state.toggleApprovalFacet()}
          >
            Has pending approval
          </button>
        </div>
      {/snippet}

      {#snippet search()}
        <input
          class="bar-input search-input"
          type="search"
          placeholder="Search this queue…"
          value={state.searchText}
          data-testid="bg-search"
          disabled={disconnected}
          title={disconnected
            ? DISCONNECTED_TOOLTIP
            : 'Searching the loaded page only — runtime-side search.tasks is unshipped (brief 11 §CC-4)'}
          oninput={(e) => state.setSearch((e.currentTarget as HTMLInputElement).value)}
          onkeydown={(e) => e.key === 'Enter' && state.submitSearch()}
        />
        <button
          type="button"
          class="bar-action"
          data-testid="bg-search-apply"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.submitSearch()}
        >
          Apply
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="bg-filter-clear"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.clearFilters()}
        >
          Clear
        </button>
      {/snippet}

      {#snippet actions()}
        <button
          type="button"
          class="bar-action"
          data-testid="bg-refresh"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.refresh()}
        >
          Refresh
        </button>
      {/snippet}
    </FilterBar>

    <BulkActionBar count={state.selected.size} onclear={() => state.setSelection(new Set())}>
      {#snippet actions()}
        <BulkToolbar
          canControl={state.canControl}
          pending={state.bulkPending}
          onverb={(verb) => void state.bulkVerb(verb)}
          onprioritize={(p) => void state.bulkPrioritize(p)}
        />
      {/snippet}
    </BulkActionBar>
    {#if state.bulkResult !== null}
      <p
        class="inline-result"
        class:ok={state.bulkResult.ok}
        class:err={!state.bulkResult.ok}
        data-testid="bg-bulk-result"
      >
        {state.bulkResult.message}
      </p>
    {/if}
  </section>

  <div class="layout">
    <section class="panel card table-card">
      <PageState
        status={state.displayStatus}
        error={state.pageError}
        onretry={() => state.refresh()}
      >
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each [0, 1, 2, 3, 4] as i (i)}
              <span class="skeleton-row"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="bg-empty">
            <p class="empty-headline">No background jobs running</p>
            <p class="empty-detail">
              No background jobs match the current view. Background jobs are
              in-process at V1 (D-006) — a runtime restart clears the queue.
            </p>
            <a class="bar-action" href="/tasks">View all tasks</a>
          </div>
        {/snippet}

        <div class="table-scroll">
          <QueueTable
            rows={state.rows}
            selected={state.selected}
            activeId={state.selectedId}
            orphans={state.orphans}
            onselect={(id) => void state.selectJob(id)}
            onselectionchange={(s) => state.setSelection(s)}
          />
        </div>
      </PageState>

      {#if state.displayStatus === 'ready' || state.displayStatus === 'empty'}
        <Pagination
          page={state.pageIndex}
          pageSize={state.pageSize}
          total={state.totalRows}
          pageSizeOptions={[50, 100, 250]}
          onpage={(p) => state.onPageRequest(p)}
          onpagesize={(s) => state.changePageSize(s)}
        />
      {/if}
    </section>

    <!-- Right column — the per-job detail when a row is selected, else the
         idle hint. Both fill the column and scroll INTERNALLY (the rail
         never drives a page-level scroll) — PAGE-POLISH §6. -->
    {#if state.selectedId !== null}
      <JobDetailRail
        detail={state.detail}
        loading={state.detailLoading}
        row={state.selectedRow}
        stream={state.stream}
        siblings={state.siblings}
        siblingsLoading={state.siblingsLoading}
        artifacts={state.artifacts}
        artifactsLoading={state.artifactsLoading}
        onclose={() => state.clearSelection()}
      />
    {:else}
      <section class="panel card idle-card" data-testid="bg-rail-idle">
        <h2 class="panel-title">Selected job</h2>
        <p class="idle-hint">
          Click a job to inspect its detail — Details, Progress, Events,
          Control History, plus artifacts and related sessions.
        </p>
      </section>
    {/if}
  </div>
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls; only the queue table + the right rail scroll
     internally (PAGE-POLISH §6 — the Events / Sessions pattern). */
  .bg-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow: hidden;
  }

  /* The carded surface — same vocabulary as Overview / Sessions / Events. */
  .card {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-width: 0;
  }

  .panel-title {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  /* ---- filter card (fixed height) ---- */
  .filter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  /* Kill the shared FilterBar's own vertical padding so the strip packs
     tight (the card already supplies the padding — Events-108h pattern). */
  .filter-card :global(.filter-bar) {
    padding: var(--space-1) var(--space-0);
    gap: var(--space-2);
  }

  .facet-chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }

  .chip {
    background: var(--color-bg);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
    text-transform: capitalize;
  }

  .chip.on {
    color: var(--color-accent);
    border-color: var(--color-accent);
    font-weight: 600;
  }

  .bar-input {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
  }

  .search-input {
    flex: 1;
    min-width: var(--size-search-min);
  }

  .save-input {
    width: var(--size-search-min);
  }

  .bar-action {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
    text-decoration: none;
  }

  .bar-action:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .inline-result {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .inline-result.ok {
    color: var(--color-success);
  }

  .inline-result.err {
    color: var(--color-danger);
  }

  /* ---- layout (fills remaining height; both columns scroll internally) ---- */
  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  .table-card {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    gap: var(--space-3);
  }

  /* PageState wraps the table; let its content fill + scroll. */
  .table-card :global(.page-state) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }

  .table-scroll {
    flex: 1;
    min-height: 0;
    overflow: auto;
  }

  .idle-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-height: 0;
    overflow-y: auto;
  }

  .idle-hint {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .table-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--space-8);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .empty-block {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-8) var(--space-4);
    text-align: center;
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    max-width: var(--size-modal-width);
  }
</style>
