<script lang="ts">
  // Harbor Console — selected-run summary panel (Phase 73i / D-117).
  //
  // Renders `flows.runs.describe` for the selected run: run-id header +
  // status pill + per-node timeline + final-output preview. A heavy
  // output (D-026) arrives as an `output_ref` ArtifactRef — the panel
  // renders an `Open artifact` link, NEVER inlines heavy bytes.
  import type { FlowRunDescription } from '$lib/flows/types';
  import { formatDurationMS } from '$lib/flows/format';

  interface Props {
    description: FlowRunDescription | null;
    onopensession: (runID: string) => void;
    onopenartifact: (artifactID: string) => void;
  }

  const { description, onopensession, onopenartifact }: Props = $props();
</script>

<section class="run-summary" data-testid="run-summary-panel">
  <h3>Run summary</h3>
  {#if !description}
    <p class="muted" data-testid="run-summary-empty">
      Select a run from the history to see its trace.
    </p>
  {:else}
    <div class="run-head">
      <span class="run-id mono" data-testid="run-summary-id">
        {description.run.run_id}
      </span>
      <span class={`status status-${description.run.status}`}>
        {description.run.status}
      </span>
      <button
        class="ghost"
        data-testid="run-open-session"
        onclick={() => onopensession(description.run.run_id)}
      >
        Open session
      </button>
    </div>

    <h4>Per-node timeline</h4>
    <ul class="timeline" data-testid="run-node-timeline">
      {#each description.node_states as node (node.node_id)}
        <li data-testid="run-node">
          <span class="mono">{node.node_id}</span>
          <span class={`status status-${node.status}`}>{node.status}</span>
          <span class="muted">{formatDurationMS(node.duration_ms)}</span>
          {#if node.retries && node.retries > 0}
            <span class="retries">{node.retries} retries</span>
          {/if}
        </li>
      {/each}
    </ul>

    <h4>Final output</h4>
    {#if description.output_ref}
      <button
        class="artifact-link"
        data-testid="run-open-artifact"
        onclick={() => onopenartifact(description.output_ref!.id)}
      >
        Open artifact ({description.output_ref.size_bytes ?? 0} bytes)
      </button>
    {:else if description.output_preview}
      <pre class="output" data-testid="run-output-preview">{description.output_preview}</pre>
    {:else}
      <p class="muted">This run produced no output.</p>
    {/if}
  {/if}
</section>

<style>
  .run-summary {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-4);
  }

  h3 {
    font-size: var(--text-sm);
    margin: var(--space-0) var(--space-0) var(--space-3);
  }

  h4 {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    margin: var(--space-4) var(--space-0) var(--space-2);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .run-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .run-id {
    flex: 1;
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .timeline {
    list-style: none;
    padding: var(--space-0);
    margin: var(--space-0);
  }

  .timeline li {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-0);
  }

  .status {
    font-size: var(--text-xs);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .status-succeeded {
    background: var(--color-success);
    color: var(--color-bg);
  }

  .status-failed {
    background: var(--color-danger);
    color: var(--color-bg);
  }

  .status-running {
    background: var(--color-accent);
    color: var(--color-bg);
  }

  .status-cancelled {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
  }

  .retries {
    color: var(--color-warning);
    font-size: var(--text-xs);
  }

  .artifact-link,
  .ghost {
    background: none;
    color: var(--color-accent);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .output {
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    overflow-x: auto;
  }
</style>
