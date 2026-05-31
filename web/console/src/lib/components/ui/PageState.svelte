<script lang="ts" module>
  // Harbor Console — shared PageState async boundary (D-121,
  // CONVENTIONS.md §4).
  //
  // `<PageState>` owns the mutually-exclusive async states every
  // Console page (and every detail rail) routes through. The audit found
  // five incompatible hand-rolled async contracts — several conflating
  // "no Runtime attached" with "request failed". This is the one contract.
  //
  // The states, as an if / else-if … chain:
  //   1. disconnected — no Runtime (connection.ts returned null). A
  //      centered CTA; NEVER conflated with error.
  //   2. loading      — a request in flight. A shape-matched skeleton
  //      (the `skeleton` snippet), never a bare "Loading…".
  //   3. error        — a thrown ProtocolError. `code: message` + a
  //      mandatory Retry button; suppresses any stale primary view.
  //   4. info         — the Runtime answered, but the requested surface
  //      is honestly "not available on this Runtime" (e.g. a planner /
  //      RunLoop runtime that returns `unknown_method` for
  //      `topology.snapshot`). A friendly info banner — never a red
  //      ERROR with a Retry that will always fail. Phase 83w-F5 /
  //      D-164. Use this branch for the `unknown_method` /
  //      `not_applicable` shape on a surface that simply isn't part
  //      of the Runtime's shape.
  //   5. empty        — succeeded, zero rows. A page-specific message
  //      (the `empty` snippet) + the page's primary affordance.
  // Otherwise the `children` (the loaded primary view) renders.

  /** The discriminated async-state union a page feeds `<PageState>`. */
  export type PageStatus = 'disconnected' | 'loading' | 'error' | 'info' | 'empty' | 'ready';
</script>

<script lang="ts">
  import type { Snippet } from 'svelte';
  import type { ProtocolError } from '$lib/protocol/errors.js';

  let {
    status,
    error,
    info,
    onretry,
    skeleton,
    empty,
    children,
    nested = false
  }: {
    /** The current async status. */
    status: PageStatus;
    /** The thrown error — required when `status === 'error'`. */
    error?: ProtocolError | { code: string; message: string } | null;
    /**
     * The friendly explanation rendered when `status === 'info'`
     * (Phase 83w-F5 / D-164). Use for "the Runtime answered but the
     * surface is not applicable to its shape" — the canonical case
     * being `unknown_method` on `topology.snapshot` when the Runtime
     * is planner/RunLoop-shaped, not engine-graph-shaped. `headline`
     * is the page-specific name of the unavailable surface; `detail`
     * is the one-line explanation pointing to docs.
     */
    info?: { headline: string; detail: string } | null;
    /** Re-invokes the page loader; wired to the Error-state Retry button. */
    onretry?: () => void;
    /** The shape-matched loading skeleton (CONVENTIONS.md §4 state 2). */
    skeleton?: Snippet;
    /** The page-specific empty-state content (CONVENTIONS.md §4 state 4). */
    empty?: Snippet;
    /** The loaded primary view — rendered only when `status === 'ready'`. */
    children?: Snippet;
    /**
     * Nested usage — a panel or detail rail rather than the whole page
     * (CONVENTIONS.md §4). Drops the full-page `min-height: 40vh` centering so
     * an empty/error placeholder is compact inside its card instead of
     * reserving ~40% of the viewport (Phase 108c — the oversized-empty fix).
     */
    nested?: boolean;
  } = $props();
</script>

{#if status === 'disconnected'}
  <div class="page-state disconnected" class:nested={nested} data-testid="page-state-disconnected">
    <p class="headline">Not connected to a Harbor Runtime</p>
    <p class="detail">Attach one in <a href="/settings">Settings</a>.</p>
  </div>
{:else if status === 'loading'}
  <div class="page-state loading" class:nested={nested} data-testid="page-state-loading">
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
  <div class="page-state error" class:nested={nested} data-testid="page-state-error" role="alert">
    <p class="headline">Request failed</p>
    <p class="detail code">{error?.code ?? 'runtime_error'}: {error?.message ?? 'unknown error'}</p>
    <button type="button" class="retry" data-testid="page-state-retry" onclick={() => onretry?.()}>
      Retry
    </button>
  </div>
{:else if status === 'info'}
  <div class="page-state info" class:nested={nested} data-testid="page-state-info">
    <p class="headline">{info?.headline ?? 'Not available on this Runtime'}</p>
    <p class="detail">
      {info?.detail ?? 'This surface is not part of this Runtime’s shape.'}
    </p>
  </div>
{:else if status === 'empty'}
  <div class="page-state empty" class:nested={nested} data-testid="page-state-empty">
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

  /* Phase 83r / N10 — Disconnected / Empty / Error placeholders center
     vertically in the main column rather than hugging the top. The
     `min-height: 40vh` is enough to make a near-empty page feel
     centered. The Loading state keeps the stretch alignment + smaller
     padding for skeleton rows. */
  .page-state.disconnected,
  .page-state.empty,
  .page-state.error,
  .page-state.info {
    min-height: 40vh;
  }

  /* Nested (panel / rail) — compact, no full-page centering. */
  .page-state.nested {
    min-height: auto;
    padding: var(--space-4);
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
