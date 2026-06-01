<script lang="ts">
  // Harbor Console — Live Runtime cockpit Topology panel (Phase 108e / D-177).
  //
  // A CAPABILITY-GATED panel: the cockpit renders it only when the runtime
  // advertises `topology_snapshot` (panels.ts). It wraps the shipped
  // `topology-canvas.svelte` fed by `topology.snapshot`. When `available` is
  // false (a planner/RunLoop runtime — the dominant V1 shape — that returns
  // `unknown_method`, D-164) the panel renders a COMPACT honest card, never a
  // tall hero void. The exact string "Topology view not available" is kept so
  // the phase smokes can anchor on it (D-164 copy generalised by D-177).
  //
  // No fabrication: a runtime that emits no per-node state leaves the canvas
  // legend reading zeros (CLAUDE.md §13).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import Workflow from '@lucide/svelte/icons/workflow';
  import TopologyCanvas from '$lib/components/live-runtime/topology-canvas.svelte';
  import type { TopologyProjection } from '$lib/protocol/topology.js';
  import type { NodeState } from '$lib/live-runtime/topology-adapter.js';

  let {
    available,
    projection,
    selectedNode = null,
    nodeStates = {},
    streamPaused = false,
    onnodeclick,
    onstreamtoggle
  }: {
    /** Whether the runtime advertises `topology_snapshot`. */
    available: boolean;
    /** The engine topology projection, or null until loaded. */
    projection: TopologyProjection | null;
    /** The selected node id. */
    selectedNode?: string | null;
    /** Console-derived per-node run states (legend + failed styling). */
    nodeStates?: Record<string, NodeState>;
    /** Console-side live-mirroring pause toggle. */
    streamPaused?: boolean;
    /** Node-click handler. */
    onnodeclick?: (node: string) => void;
    /** Stream-pause toggle handler. */
    onstreamtoggle?: (next: boolean) => void;
  } = $props();
</script>

{#if available && projection !== null}
  <TopologyCanvas
    projection={projection}
    selectedNode={selectedNode}
    onnodeclick={onnodeclick}
    nodeStates={nodeStates}
    streamPaused={streamPaused}
    onstreamtoggle={onstreamtoggle}
  />
{:else}
  <div class="topo-empty" data-testid="topology-panel-empty">
    <span class="empty-icon"><Workflow size={20} aria-hidden="true" /></span>
    <p class="empty-headline">Topology view not available</p>
    <p class="empty-detail">
      This runtime is planner/RunLoop-shaped, not engine-graph-shaped, so it
      exposes no node topology.
    </p>
  </div>
{/if}

<style>
  .topo-empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-5) var(--space-2);
    text-align: center;
  }

  .empty-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--size-avatar-md);
    height: var(--size-avatar-md);
    border-radius: var(--radius-md);
    margin-bottom: var(--space-1);
    background: var(--color-accent-soft);
    color: var(--color-accent);
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
