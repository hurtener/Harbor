<script lang="ts">
  // Harbor Console — Playground trace toggle (Phase 73n / D-130).
  //
  // A toggle that reveals the active run's execution trace. When ON,
  // the page fetches the Phase 74 `topology.snapshot` (D-114) for the
  // active run and renders the node graph as a compact trace summary.
  //
  // The toggle is a pure presentational control: it owns the on/off
  // state display + the rendered snapshot, but the page does the
  // `topology.snapshot` round-trip and passes the projection in.
  //
  // Design tokens only.

  /** A minimal topology node shape — matches `TopologyProjection`. */
  export interface TraceNode {
    id: string;
    kind: string;
  }

  let {
    enabled,
    nodes = [],
    loading = false,
    error = '',
    ontoggle
  }: {
    enabled: boolean;
    /** Topology nodes from the active run's `topology.snapshot`. */
    nodes?: TraceNode[];
    loading?: boolean;
    error?: string;
    ontoggle: (next: boolean) => void;
  } = $props();
</script>

<div class="trace-toggle" data-testid="playground-trace-toggle">
  <label class="toggle-row">
    <input
      type="checkbox"
      data-testid="trace-toggle-checkbox"
      checked={enabled}
      onchange={(e) => ontoggle((e.currentTarget as HTMLInputElement).checked)}
    />
    <span class="toggle-label">Show execution trace</span>
  </label>

  {#if enabled}
    <div class="trace-body" data-testid="trace-body">
      {#if loading}
        <p class="status">Loading topology…</p>
      {:else if error !== ''}
        <p class="status status-error">Trace unavailable: {error}</p>
      {:else if nodes.length === 0}
        <p class="status">No topology nodes for the active run.</p>
      {:else}
        <ul class="trace-nodes">
          {#each nodes as node (node.id)}
            <li class="trace-node" data-kind={node.kind}>
              <span class="node-name">{node.id}</span>
              <span class="node-kind">{node.kind}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
</div>

<style>
  .trace-toggle {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .toggle-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .toggle-label {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .trace-body {
    padding: var(--space-2);
    background: var(--color-surface);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-sm);
  }

  .trace-nodes {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .trace-node {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .node-name {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
  }

  .node-kind {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .status {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .status-error {
    color: var(--color-danger);
  }
</style>
