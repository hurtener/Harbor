<script lang="ts">
  // Harbor Console — Playground `pip` resizable split (DisplayMode layout).
  //
  // A two-region horizontal split (left = chat, right = app) with a draggable
  // divider. The ratio is the LEFT column's width fraction; on drag it is
  // clamped to sane bounds via the pure `clampRatio` (the same function the
  // unit tests pin) and emitted to the parent. The parent owns the ratio (it is
  // Console-local view state that may persist — D-061), so reopening the right
  // rail never resets it.
  //
  // Custom-built (not a Skeleton primitive): the Console uses NO Skeleton
  // components — its entire primitive inventory is hand-built token-styled
  // Svelte, and Skeleton v3 ships Tailwind-utility-class components that the
  // `.stylelintrc.cjs` token-only rule rejects (CLAUDE.md §4.5 rule 4, D-092).
  //
  // Design tokens only.
  import type { Snippet } from 'svelte';
  import { clampRatio } from './layout.js';

  let {
    ratio = 0.5,
    left,
    right,
    onratio
  }: {
    /** The left (chat) column width fraction, clamped to [MIN, MAX]. */
    ratio?: number;
    /** The left region (chat). */
    left: Snippet;
    /** The right region (app panel). */
    right: Snippet;
    /** Emitted with the clamped ratio while the divider is dragged. */
    onratio: (ratio: number) => void;
  } = $props();

  let container = $state<HTMLDivElement | null>(null);
  let dragging = $state(false);

  const safeRatio = $derived(clampRatio(ratio));

  function startDrag(ev: PointerEvent): void {
    if (!container) return;
    dragging = true;
    (ev.currentTarget as HTMLElement).setPointerCapture(ev.pointerId);
    ev.preventDefault();
  }

  function moveDrag(ev: PointerEvent): void {
    if (!dragging || !container) return;
    const rect = container.getBoundingClientRect();
    if (rect.width <= 0) return;
    const next = (ev.clientX - rect.left) / rect.width;
    onratio(clampRatio(next));
  }

  function endDrag(ev: PointerEvent): void {
    if (!dragging) return;
    dragging = false;
    const el = ev.currentTarget as HTMLElement;
    if (el.hasPointerCapture(ev.pointerId)) el.releasePointerCapture(ev.pointerId);
  }

  // Keyboard a11y — arrow keys nudge the divider by a fixed step.
  function keyResize(ev: KeyboardEvent): void {
    const STEP = 0.02;
    if (ev.key === 'ArrowLeft') {
      onratio(clampRatio(safeRatio - STEP));
      ev.preventDefault();
    } else if (ev.key === 'ArrowRight') {
      onratio(clampRatio(safeRatio + STEP));
      ev.preventDefault();
    }
  }
</script>

<div
  class="split"
  data-testid="pip-split"
  bind:this={container}
  style="grid-template-columns: {safeRatio}fr min-content {1 - safeRatio}fr;"
  data-dragging={dragging}
>
  <section class="pane pane-left" data-testid="pip-pane-chat">
    {@render left()}
  </section>

  <!-- The ARIA "window splitter" pattern: a focusable, draggable separator with
       aria-valuenow. The separator role is noninteractive by default, so the
       a11y lint flags the tabindex + handlers — but a resize handle is exactly
       the sanctioned focusable-separator case (keyboard + pointer resize). -->
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
  <div
    class="divider"
    data-testid="pip-divider"
    role="separator"
    aria-orientation="vertical"
    aria-label="Resize chat and app panels"
    aria-valuemin={20}
    aria-valuemax={80}
    aria-valuenow={Math.round(safeRatio * 100)}
    tabindex="0"
    onpointerdown={startDrag}
    onpointermove={moveDrag}
    onpointerup={endDrag}
    onpointercancel={endDrag}
    onkeydown={keyResize}
  >
    <span class="divider-grip" aria-hidden="true"></span>
  </div>

  <section class="pane pane-right" data-testid="pip-pane-app">
    {@render right()}
  </section>
</div>

<style>
  .split {
    display: grid;
    width: 100%;
    height: 100%;
    min-height: 0;
    align-items: stretch;
  }

  .pane {
    min-width: 0;
    min-height: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }

  .divider {
    width: var(--space-2);
    cursor: col-resize;
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--color-surface);
    border-left: var(--border-hairline);
    border-right: var(--border-hairline);
    touch-action: none;
  }

  .divider:hover,
  .split[data-dragging='true'] .divider {
    background: var(--color-surface-raised);
  }

  .divider:focus-visible {
    outline: var(--border-hairline-width) solid var(--color-accent);
    outline-offset: calc(-1 * var(--border-hairline-width));
  }

  .divider-grip {
    width: var(--border-hairline-width);
    height: var(--space-8);
    background: var(--color-border);
    border-radius: var(--radius-pill);
  }

  .divider:hover .divider-grip,
  .split[data-dragging='true'] .divider-grip {
    background: var(--color-accent);
  }
</style>
