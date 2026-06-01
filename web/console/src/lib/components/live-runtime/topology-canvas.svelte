<script lang="ts">
  // Harbor Console — Live Runtime topology canvas (Phase 108d Stage 2).
  //
  // A Live-Runtime WRAPPER around the SHARED engine graph canvas
  // (`components/graph/EngineGraphCanvas.svelte`, the Flows-page renderer).
  // The wrapper adapts the live `TopologyProjection` into the shared
  // canvas's `GraphInput` model, forwards node-click selection, and layers
  // the Live-Runtime-specific chrome ON TOP of the shared canvas so the
  // Flows page stays untouched (CLAUDE.md §4.5; the shared canvas is not
  // modified except for the additive, backward-compatible `meta.state`
  // styling on `GraphNode`):
  //   - an in-canvas status legend (corner) with per-state counts;
  //   - canvas controls (zoom out / reset / zoom in + Pause-stream toggle,
  //     top-right);
  //   - filter chips (above the canvas) that re-scope which nodes render
  //     by status + by tool name — purely local UI state.
  //
  // # Node state is Console-side enrichment (no fabricated wire fields)
  //
  // The Protocol `TopologyProjection` carries node identity + kind + edge
  // queue depth, NOT a per-node run state. The legend counts + the red
  // failed/reject terminal nodes are driven by the `nodeStates` map the
  // page derives from the live event stream (keyed by node name). On a
  // planner/RunLoop runtime (which returns `unknown_method` for
  // `topology.snapshot` and emits no node states) the map is empty: the
  // legend reads zeros and no node is styled failed — nothing invented
  // (CLAUDE.md §13). See `$lib/live-runtime/topology-adapter.ts`.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import EngineGraphCanvas from '$lib/components/graph/EngineGraphCanvas.svelte';
  import {
    projectionToGraph,
    legendCounts,
    filterProjection,
    type NodeStateMap,
    type TopologyStatusFilter
  } from '$lib/live-runtime/topology-adapter.js';
  import type { TopologyProjection } from '$lib/protocol/topology.js';
  import ZoomOut from '@lucide/svelte/icons/zoom-out';
  import ZoomIn from '@lucide/svelte/icons/zoom-in';
  import Maximize from '@lucide/svelte/icons/maximize';
  import Pause from '@lucide/svelte/icons/pause';
  import Play from '@lucide/svelte/icons/play';

  let {
    projection,
    selectedNode = null,
    onnodeclick,
    nodeStates = {},
    streamPaused = false,
    onstreamtoggle
  }: {
    /** The engine topology projection from `topology.snapshot`. */
    projection: TopologyProjection;
    /** The currently-selected node name, if any. */
    selectedNode?: string | null;
    /** Emitted with a node name when a canvas node is clicked. */
    onnodeclick?: (node: string) => void;
    /**
     * Console-derived per-node run states (keyed by node name). Drives the
     * status legend counts + the failed-node styling. Empty on runtimes
     * that emit no node states — see the module doc.
     */
    nodeStates?: NodeStateMap;
    /** The Console-side live-update pause state (controlled by the page). */
    streamPaused?: boolean;
    /** Toggle the Console-side live-update pause (local UI state). */
    onstreamtoggle?: (next: boolean) => void;
  } = $props();

  /* ---- filter state (local UI only — no Protocol calls) ----------- */
  let statusFilter = $state<TopologyStatusFilter>('all');
  let toolFilter = $state('');

  /* ---- zoom state (CSS transform around the shared canvas) -------- */
  // A minimal, robust zoom: a scale factor the wrapper applies via a CSS
  // transform. The shared canvas owns its own internal layout + scroll;
  // this is an additive viewport scale that Reset returns to 1.
  let zoom = $state(1);
  const ZOOM_STEP = 0.2;
  const ZOOM_MIN = 0.4;
  const ZOOM_MAX = 2;

  function zoomIn(): void {
    zoom = Math.min(ZOOM_MAX, Math.round((zoom + ZOOM_STEP) * 100) / 100);
  }
  function zoomOut(): void {
    zoom = Math.max(ZOOM_MIN, Math.round((zoom - ZOOM_STEP) * 100) / 100);
  }
  function resetZoom(): void {
    zoom = 1;
  }

  /* ---- status legend counts (over the FULL projection) ------------ */
  // The legend reports the run's at-a-glance health independent of the
  // active chip, so it counts the full projection, not the filtered view.
  let counts = $derived(legendCounts(projection, nodeStates));

  const LEGEND: { key: Exclude<TopologyStatusFilter, 'all'>; label: string }[] = [
    { key: 'running', label: 'Running' },
    { key: 'pending', label: 'Queued' },
    { key: 'completed', label: 'Completed' },
    { key: 'paused', label: 'Paused' },
    { key: 'failed', label: 'Failed' }
  ];

  /* ---- filtered projection → shared graph model ------------------- */
  let filtered = $derived(filterProjection(projection, nodeStates, statusFilter, toolFilter));
  let graph = $derived(projectionToGraph(filtered, nodeStates));

  function setStatus(next: TopologyStatusFilter): void {
    statusFilter = next;
  }

  const CHIPS: { key: TopologyStatusFilter; label: string }[] = [
    { key: 'all', label: 'All' },
    { key: 'pending', label: 'Pending' },
    { key: 'running', label: 'Running' },
    { key: 'completed', label: 'Completed' },
    { key: 'paused', label: 'Paused' },
    { key: 'failed', label: 'Failed' }
  ];
</script>

<div class="topology-canvas" data-testid="topology-canvas">
  <!-- Filter chips above the canvas (status + tool-name). Local UI state. -->
  <div class="filter-bar" data-testid="topology-filter-bar">
    <div class="chips" role="group" aria-label="Filter nodes by status">
      {#each CHIPS as chip (chip.key)}
        <button
          type="button"
          class="chip"
          class:active={statusFilter === chip.key}
          aria-pressed={statusFilter === chip.key}
          onclick={() => setStatus(chip.key)}
        >
          {chip.label}
        </button>
      {/each}
    </div>
    <input
      class="tool-filter"
      type="search"
      placeholder="Filter by tool name…"
      aria-label="Filter nodes by tool name"
      data-testid="topology-tool-filter"
      bind:value={toolFilter}
    />
  </div>

  <div class="canvas-region">
    <!-- Canvas controls (top-right): zoom out / reset / zoom in + pause. -->
    <div class="controls" data-testid="topology-controls">
      <button type="button" class="ctrl" title="Zoom out" aria-label="Zoom out" onclick={zoomOut}>
        <ZoomOut size={14} aria-hidden="true" />
      </button>
      <button
        type="button"
        class="ctrl"
        title="Reset zoom"
        aria-label="Reset zoom"
        data-testid="topology-reset-zoom"
        onclick={resetZoom}
      >
        <Maximize size={14} aria-hidden="true" />
      </button>
      <button type="button" class="ctrl" title="Zoom in" aria-label="Zoom in" onclick={zoomIn}>
        <ZoomIn size={14} aria-hidden="true" />
      </button>
      <button
        type="button"
        class="ctrl"
        class:active={streamPaused}
        title={streamPaused ? 'Resume live updates' : 'Pause live updates'}
        aria-label={streamPaused ? 'Resume live updates' : 'Pause live updates'}
        aria-pressed={streamPaused}
        data-testid="topology-pause-stream"
        onclick={() => onstreamtoggle?.(!streamPaused)}
      >
        {#if streamPaused}
          <Play size={14} aria-hidden="true" />
        {:else}
          <Pause size={14} aria-hidden="true" />
        {/if}
      </button>
    </div>

    <!-- In-canvas status legend (corner) with per-state counts. -->
    <div class="legend" data-testid="topology-legend">
      {#each LEGEND as item (item.key)}
        <span class="legend-item">
          <span class="swatch" data-status={item.key} aria-hidden="true"></span>
          <span class="legend-label">{item.label}</span>
          <span class="legend-count" data-testid={`legend-count-${item.key}`}>
            {counts[item.key]}
          </span>
        </span>
      {/each}
    </div>

    <div class="zoom-viewport" style={`transform: scale(${zoom}); transform-origin: top left;`}>
      <EngineGraphCanvas
        graph={graph}
        selectedNodeID={selectedNode}
        onselect={(id) => onnodeclick?.(id)}
      />
    </div>
  </div>
</div>

<style>
  .topology-canvas {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    width: 100%;
  }

  .filter-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .chips {
    display: flex;
    gap: var(--space-1);
    flex-wrap: wrap;
  }

  .chip {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-pill);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .chip.active {
    color: var(--color-bg);
    background: var(--color-accent);
    border-color: var(--color-accent);
  }

  .tool-filter {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    min-width: var(--size-search-min);
  }

  .canvas-region {
    position: relative;
    overflow: hidden;
    border-radius: var(--radius-md);
  }

  .zoom-viewport {
    width: 100%;
  }

  .controls {
    position: absolute;
    top: var(--space-2);
    right: var(--space-2);
    z-index: 2;
    display: flex;
    gap: var(--space-1);
  }

  .ctrl {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--space-6);
    height: var(--space-6);
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    cursor: pointer;
  }

  .ctrl.active {
    color: var(--color-bg);
    background: var(--color-accent);
    border-color: var(--color-accent);
  }

  .legend {
    position: absolute;
    bottom: var(--space-2);
    left: var(--space-2);
    z-index: 2;
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    font-size: var(--text-2xs);
  }

  .legend-item {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    color: var(--color-text-muted);
  }

  .swatch {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: var(--radius-pill);
    background: var(--color-text-muted);
  }

  .swatch[data-status='running'] {
    background: var(--color-accent);
  }
  .swatch[data-status='completed'] {
    background: var(--color-success);
  }
  .swatch[data-status='paused'] {
    background: var(--color-warning);
  }
  .swatch[data-status='failed'] {
    background: var(--color-danger);
  }

  .legend-count {
    color: var(--color-text);
    font-variant-numeric: tabular-nums;
  }
</style>
