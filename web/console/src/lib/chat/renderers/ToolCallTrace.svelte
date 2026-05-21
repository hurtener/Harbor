<script lang="ts">
  // Chat-bubble tool-call-trace renderer — Phase 73n / D-130.
  //
  // Registered into the Phase 73l canonical renderer registry
  // (`$lib/chat/renderers/index.ts`) under the synthetic MIME
  // `application/vnd.harbor.tool-call-trace`. It renders a tool-call
  // trace card inside an agent chat bubble.
  //
  // It fetches the trace JSON from its resolved (presigned, D-026) `src`
  // — the same fetch-from-presigned-URL pattern the Phase 73l MIME
  // renderers use. The trace payload is a redacted summary (CLAUDE.md
  // §7 — never raw tool arguments).
  import type { RendererProps } from './index.js';

  let { mime, src, filename }: RendererProps = $props();

  interface TraceEntry {
    tool: string;
    status: string;
    summary: string;
    run_id?: string;
  }

  let entries = $state<TraceEntry[]>([]);
  let error = $state('');
  let loading = $state(true);

  async function load(url: string): Promise<void> {
    loading = true;
    error = '';
    try {
      const resp = await fetch(url);
      if (!resp.ok) {
        throw new Error(`HTTP ${resp.status}`);
      }
      const parsed: unknown = await resp.json();
      entries = Array.isArray(parsed) ? (parsed as TraceEntry[]) : [];
    } catch (e) {
      error = e instanceof Error ? e.message : 'failed to load trace';
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void load(src);
  });
</script>

<div class="tool-call-trace" data-renderer-source="tool-call-trace" data-mime={mime}>
  {#if loading}
    <p class="status">Loading {filename ?? 'tool-call trace'}…</p>
  {:else if error}
    <p class="status status-error">Could not load trace: {error}</p>
  {:else if entries.length === 0}
    <p class="status">No tool calls in this trace.</p>
  {:else}
    <ul class="trace-list">
      {#each entries as entry, i (i)}
        <li class="trace-entry" data-status={entry.status}>
          <span class="tool-name">{entry.tool}</span>
          <span class="trace-status" data-status={entry.status}>{entry.status}</span>
          <span class="trace-summary">{entry.summary}</span>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .tool-call-trace {
    width: 100%;
  }

  .trace-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .trace-entry {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    padding: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .tool-name {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--color-text);
    font-weight: 600;
  }

  .trace-status {
    font-size: var(--text-xs);
    text-transform: uppercase;
    color: var(--color-text-muted);
  }

  .trace-status[data-status='failed'] {
    color: var(--color-danger);
  }

  .trace-status[data-status='succeeded'] {
    color: var(--color-success);
  }

  .trace-summary {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .status {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .status-error {
    color: var(--color-danger);
  }
</style>
