<script lang="ts">
  // ToolDetailPanel — the bottom-left selected-tool detail panel
  // (page-tools.md §12). Tabbed: Manifest / Inputs / Outputs / Recent
  // invocations / Approval. The Approval tab's Approve / Reject buttons
  // invoke the EXISTING shipped `approve` / `reject` Protocol methods
  // (Phase 54) — NO new approval method (page-tools.md §12 + §13's
  // "no parallel implementations" rule). Svelte 5 runes mode (D-092).
  import type { Tool, ToolManifest } from '$lib/protocol/tools.js';

  type Tab = 'manifest' | 'inputs' | 'outputs' | 'recent' | 'approval';

  let {
    tool = null,
    manifest = null,
    loading = false,
    onapprove,
    onreject
  }: {
    tool?: Tool | null;
    manifest?: ToolManifest | null;
    loading?: boolean;
    // The session deep-link helper routes the Approve / Reject action
    // through the shared chat module's Protocol client (Phase 54
    // `approve` / `reject`) — the panel never re-implements approval.
    onapprove: (toolID: string) => void;
    onreject: (toolID: string) => void;
  } = $props();

  let activeTab = $state<Tab>('manifest');

  const TABS: { id: Tab; label: string }[] = [
    { id: 'manifest', label: 'Manifest' },
    { id: 'inputs', label: 'Inputs' },
    { id: 'outputs', label: 'Outputs' },
    { id: 'recent', label: 'Recent invocations' },
    { id: 'approval', label: 'Approval' }
  ];

  function pretty(json: string): string {
    if (!json) return '(none declared)';
    try {
      return JSON.stringify(JSON.parse(json), null, 2);
    } catch {
      return json;
    }
  }
</script>

<section class="panel" data-testid="tools-detail-panel">
  {#if tool === null}
    <p class="empty" data-testid="tools-detail-empty">
      Select a tool from the catalog to inspect its descriptor.
    </p>
  {:else}
    <header>
      <h2 data-testid="tools-detail-name">{tool.name}</h2>
      <span class="sub">{tool.transport} · {tool.scope} · {tool.reliability_tier}</span>
    </header>

    <div class="tabs" role="tablist">
      {#each TABS as tab (tab.id)}
        <button
          type="button"
          role="tab"
          class="tab"
          class:tab-active={tab.id === activeTab}
          aria-selected={tab.id === activeTab}
          data-testid="tools-detail-tab"
          data-tab={tab.id}
          onclick={() => (activeTab = tab.id)}
        >
          {tab.label}
        </button>
      {/each}
    </div>

    <div class="tab-body" data-testid="tools-detail-tab-body">
      {#if loading}
        <p class="muted">Loading descriptor…</p>
      {:else if activeTab === 'manifest'}
        {#if manifest === null}
          <p class="muted">No manifest loaded.</p>
        {:else}
          <dl class="kv">
            <dt>Version</dt>
            <dd>{manifest.tool.version || '—'}</dd>
            <dt>Side effect</dt>
            <dd>{manifest.side_effect}</dd>
            <dt>OAuth binding</dt>
            <dd>{manifest.oauth_binding_scope || 'none'}</dd>
            <dt>Approval policy</dt>
            <dd>{manifest.tool.approval_policy}</dd>
            <dt>Retry attempts</dt>
            <dd>{manifest.retry_attempts}</dd>
            <dt>Timeout</dt>
            <dd>{manifest.timeout_ms} ms</dd>
            <dt>Loading mode</dt>
            <dd>{manifest.loading_mode}</dd>
          </dl>
        {/if}
      {:else if activeTab === 'inputs'}
        <pre data-testid="tools-detail-inputs">{manifest
            ? pretty(manifest.args_schema)
            : '—'}</pre>
      {:else if activeTab === 'outputs'}
        <pre data-testid="tools-detail-outputs">{manifest
            ? pretty(manifest.out_schema)
            : '—'}</pre>
      {:else if activeTab === 'recent'}
        <p class="muted">
          Recent invocations stream from the <code>tool.*</code> event topic;
          each row deep-links into the session's bottom dock.
        </p>
      {:else if activeTab === 'approval'}
        <p class="muted">
          Approval policy: <strong>{tool.approval_policy}</strong>.
        </p>
        <div class="approval-actions">
          <button
            type="button"
            class="btn btn-ok"
            data-testid="tools-approve"
            onclick={() => onapprove(tool.id)}
          >
            Approve
          </button>
          <button
            type="button"
            class="btn btn-danger"
            data-testid="tools-reject"
            onclick={() => onreject(tool.id)}
          >
            Reject
          </button>
        </div>
        <p class="hint">
          Approve / Reject invoke the shipped <code>approve</code> /
          <code>reject</code> Protocol methods (Phase 54).
        </p>
      {/if}
    </div>
  {/if}
</section>

<style>
  .panel {
    display: flex;
    flex-direction: column;
    min-height: var(--layout-detail-min-height);
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-width-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  .empty {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    text-align: center;
    margin: auto;
  }

  header {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin-bottom: var(--space-3);
  }

  h2 {
    margin: var(--space-0);
    font-size: var(--text-lg);
    color: var(--color-text);
  }

  .sub {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .tabs {
    display: flex;
    gap: var(--space-1);
    border-bottom: var(--border-width-thin) solid var(--color-border);
    margin-bottom: var(--space-3);
  }

  .tab {
    padding: var(--space-2) var(--space-3);
    background: transparent;
    border: none;
    border-bottom: var(--border-width-thin) solid transparent;
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .tab-active {
    color: var(--color-accent);
    border-bottom-color: var(--color-accent);
  }

  .tab-body {
    flex: 1;
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .hint {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    margin-top: var(--space-2);
  }

  pre {
    margin: var(--space-0);
    padding: var(--space-3);
    background: var(--color-bg);
    border-radius: var(--radius-sm);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
    overflow: auto;
  }

  .kv {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-2) var(--space-4);
    margin: var(--space-0);
  }

  .kv dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .kv dd {
    margin: var(--space-0);
    color: var(--color-text);
    font-size: var(--text-sm);
  }

  .approval-actions {
    display: flex;
    gap: var(--space-2);
    margin-top: var(--space-3);
  }

  .btn {
    padding: var(--space-2) var(--space-4);
    border-radius: var(--radius-sm);
    border: var(--border-width-thin) solid var(--color-border);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .btn-ok {
    background: var(--color-success-soft);
    color: var(--color-success);
    border-color: var(--color-success);
  }

  .btn-danger {
    background: var(--color-danger-soft);
    color: var(--color-danger);
    border-color: var(--color-danger);
  }
</style>
