<script lang="ts">
  // Harbor Console — Compare-versions diff (Phase 73i / D-117).
  //
  // A pure CLIENT-SIDE diff of two `flows.describe` snapshots (D-061 —
  // Console-local; there is NO Protocol-side comparison method). The
  // operator pins a snapshot via `Save snapshot`; this component diffs
  // a saved snapshot against the live description's node/edge sets.
  //
  // It is a comparison VIEW, not an edit affordance — it never mutates
  // a flow (D-063).
  import type { FlowDescription } from '$lib/flows/types';

  interface Props {
    open: boolean;
    base: FlowDescription | null;
    head: FlowDescription | null;
    onclose: () => void;
  }

  const { open, base, head, onclose }: Props = $props();

  function nodeIDs(d: FlowDescription | null): Set<string> {
    return new Set((d?.nodes ?? []).map((n) => n.id));
  }

  const diff = $derived.by(() => {
    const baseIDs = nodeIDs(base);
    const headIDs = nodeIDs(head);
    const added = [...headIDs].filter((id) => !baseIDs.has(id)).sort();
    const removed = [...baseIDs].filter((id) => !headIDs.has(id)).sort();
    const common = [...headIDs].filter((id) => baseIDs.has(id)).sort();
    return { added, removed, common };
  });
</script>

{#if open}
  <section class="compare" data-testid="compare-versions">
    <header>
      <h3>Compare versions (Console-local)</h3>
      <button class="ghost" data-testid="compare-close" onclick={onclose}>Close</button>
    </header>
    {#if !base || !head}
      <p class="muted" data-testid="compare-need-snapshot">
        Save a snapshot first, then compare it against the live flow.
      </p>
    {:else}
      <dl class="diff">
        <div class="added">
          <dt>Added nodes ({diff.added.length})</dt>
          <dd>{diff.added.length ? diff.added.join(', ') : '—'}</dd>
        </div>
        <div class="removed">
          <dt>Removed nodes ({diff.removed.length})</dt>
          <dd>{diff.removed.length ? diff.removed.join(', ') : '—'}</dd>
        </div>
        <div>
          <dt>Unchanged nodes ({diff.common.length})</dt>
          <dd>{diff.common.length ? diff.common.join(', ') : '—'}</dd>
        </div>
      </dl>
    {/if}
  </section>
{/if}

<style>
  .compare {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-4);
  }

  header {
    display: flex;
    justify-content: space-between;
    align-items: center;
    margin-bottom: var(--space-3);
  }

  h3 {
    font-size: var(--text-sm);
    margin: var(--space-0);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .diff div {
    margin-bottom: var(--space-2);
  }

  dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  dd {
    margin: var(--space-0);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .added dd {
    color: var(--color-success);
  }

  .removed dd {
    color: var(--color-danger);
  }

  .ghost {
    background: none;
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }
</style>
