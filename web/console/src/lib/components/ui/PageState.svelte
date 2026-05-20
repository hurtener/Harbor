<script lang="ts" module>
  // Harbor Console — shared PageState async boundary (D-121,
  // CONVENTIONS.md §4).
  //
  // `<PageState>` owns the FOUR mutually-exclusive async states every
  // Console page (and every detail rail) routes through. The audit found
  // five incompatible hand-rolled async contracts — several conflating
  // "no Runtime attached" with "request failed". This is the one contract.
  //
  // The four states, as an if / else-if / else-if / else chain:
  //   1. disconnected — no Runtime (connection.ts returned null). A
  //      centered CTA; NEVER conflated with error.
  //   2. loading      — a request in flight. A shape-matched skeleton
  //      (the `skeleton` snippet), never a bare "Loading…".
  //   3. error        — a thrown ProtocolError. `code: message` + a
  //      mandatory Retry button; suppresses any stale primary view.
  //   4. empty        — succeeded, zero rows. A page-specific message
  //      (the `empty` snippet) + the page's primary affordance.
  // Otherwise the `children` (the loaded primary view) renders.

  /** The discriminated async-state union a page feeds `<PageState>`. */
  export type PageStatus = 'disconnected' | 'loading' | 'error' | 'empty' | 'ready';
</script>

<script lang="ts">
  import type { Snippet } from 'svelte';
  import type { ProtocolError } from '$lib/protocol/errors.js';

  let {
    status,
    error,
    onretry,
    skeleton,
    empty,
    children
  }: {
    /** The current async status. */
    status: PageStatus;
    /** The thrown error — required when `status === 'error'`. */
    error?: ProtocolError | { code: string; message: string } | null;
    /** Re-invokes the page loader; wired to the Error-state Retry button. */
    onretry?: () => void;
    /** The shape-matched loading skeleton (CONVENTIONS.md §4 state 2). */
    skeleton?: Snippet;
    /** The page-specific empty-state content (CONVENTIONS.md §4 state 4). */
    empty?: Snippet;
    /** The loaded primary view — rendered only when `status === 'ready'`. */
    children?: Snippet;
  } = $props();
</script>

{#if status === 'disconnected'}
  <div class="page-state disconnected" data-testid="page-state-disconnected">
    <p class="headline">Not connected to a Harbor Runtime</p>
    <p class="detail">Attach one in <a href="/settings">Settings</a>.</p>
  </div>
{:else if status === 'loading'}
  <div class="page-state loading" data-testid="page-state-loading">
    {#if skeleton}
      {@render skeleton()}
    {:else}
      <div class="skeleton-rows" aria-hidden="true">
        <span class="skeleton-row"></span>
        <span class="skeleton-row"></span>
        <span class="skeleton-row"></span>
      </div>
    {/if}
    <span class="sr-only">Loading…</span>
  </div>
{:else if status === 'error'}
  <div class="page-state error" data-testid="page-state-error" role="alert">
    <p class="headline">Request failed</p>
    <p class="detail code">{error?.code ?? 'runtime_error'}: {error?.message ?? 'unknown error'}</p>
    <button type="button" class="retry" data-testid="page-state-retry" onclick={() => onretry?.()}>
      Retry
    </button>
  </div>
{:else if status === 'empty'}
  <div class="page-state empty" data-testid="page-state-empty">
    {#if empty}
      {@render empty()}
    {:else}
      <p class="headline">Nothing here yet</p>
    {/if}
  </div>
{:else}
  {@render children?.()}
{/if}

<style>
  .page-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: var(--space-2);
    padding: var(--space-12) var(--space-4);
    text-align: center;
  }

  .page-state.loading {
    align-items: stretch;
    padding: var(--space-4) var(--space-0);
  }

  .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .detail.code {
    font-family: var(--font-mono);
  }

  .detail a {
    color: var(--color-accent);
  }

  .retry {
    margin-top: var(--space-2);
    background: var(--color-accent);
    color: var(--color-bg);
    border: none;
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-4);
    font-size: var(--text-sm);
    font-weight: 600;
    cursor: pointer;
  }

  .skeleton-rows {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--layout-table-row-height);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    animation: pulse var(--motion-slow) ease-in-out infinite alternate;
  }

  @keyframes pulse {
    from {
      opacity: 0.4;
    }
    to {
      opacity: 0.8;
    }
  }

  .sr-only {
    position: absolute;
    width: var(--size-sr-square);
    height: var(--size-sr-square);
    margin: calc(-1 * var(--size-sr-square));
    padding: var(--space-0);
    overflow: hidden;
    clip: rect(0, 0, 0, 0);
    white-space: nowrap;
    border: 0;
  }
</style>
