<script lang="ts" module>
  // Harbor Console — Tools-specific tabbed detail panel (D-121,
  // CONVENTIONS.md §3/§5).
  //
  // The per-tool detail is genuinely Tools-specific, so it stays in
  // `components/tools/` (CONVENTIONS.md §3) — but it composes the shared
  // `ui/` primitives (`StatusChip`) underneath rather than forking pills
  // or chrome. Tabbed: Manifest / Inputs / Outputs / Recent invocations /
  // Approval. Svelte 5 runes mode (D-092); design tokens only.
  //
  // # §13 — no stubbed action presented as done
  //
  // The Approval tab's Approve / Reject controls call the REAL Protocol
  // method `tools.set_approval_policy` (it sets the tool's `ToolPolicy`
  // approval gate: Approve → `auto`, Reject → `denied`). The method is
  // ADMIN-scoped (D-079); when the connection lacks the `admin` claim the
  // controls render disabled-with-tooltip — never a fake feedback string.

  /** The detail-panel tab identifiers. */
  export type ToolDetailTab =
    | 'manifest'
    | 'inputs'
    | 'outputs'
    | 'recent'
    | 'approval';
</script>

<script lang="ts">
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import type { Tool, ToolManifest, ToolApprovalPolicy } from '$lib/protocol/tools.js';

  let {
    tool = null,
    manifest = null,
    loading = false,
    canAdmin = false,
    approvalPending = false,
    approvalResult = null,
    onsetpolicy
  }: {
    /** The selected catalog row, or null when nothing is selected. */
    tool?: Tool | null;
    /** The loaded full manifest for the selected tool. */
    manifest?: ToolManifest | null;
    /** True while the manifest is loading. */
    loading?: boolean;
    /**
     * True when the connection carries the verified `admin` scope claim
     * (D-079) that `tools.set_approval_policy` requires. When false the
     * Approve / Reject controls render disabled-with-tooltip.
     */
    canAdmin?: boolean;
    /** True while a `set_approval_policy` call is in flight. */
    approvalPending?: boolean;
    /**
     * The outcome of the last `set_approval_policy` call — a real
     * Protocol result, not a fabricated feedback string. `null` until a
     * call completes.
     */
    approvalResult?: { ok: boolean; message: string } | null;
    /**
     * Invokes the real `tools.set_approval_policy` Protocol method with
     * the requested policy. The page owns the call + its async state.
     */
    onsetpolicy?: (toolID: string, policy: ToolApprovalPolicy) => void;
  } = $props();

  let activeTab = $state<ToolDetailTab>('manifest');

  const TABS: { id: ToolDetailTab; label: string }[] = [
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

  // Disabled-with-tooltip reason for the Approve / Reject controls when
  // the operator lacks the admin scope (CONVENTIONS.md §5).
  const adminGateReason =
    'Requires the admin scope claim — set_approval_policy is an admin Protocol method (D-079).';
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
            <dd><StatusChip kind="neutral" label={manifest.tool.approval_policy} /></dd>
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
        <p class="muted" data-testid="tools-detail-recent">
          Recent invocations stream from the <code>tool.*</code> event topic;
          each row deep-links into the session's bottom dock. The event-stream
          subscription lands with the Events page surface.
        </p>
      {:else if activeTab === 'approval'}
        <p class="muted">
          Current approval policy: <StatusChip
            kind="neutral"
            label={tool.approval_policy}
          />.
        </p>
        <div class="approval-actions">
          <button
            type="button"
            class="btn btn-ok"
            data-testid="tools-approve"
            disabled={!canAdmin || approvalPending}
            title={canAdmin ? undefined : adminGateReason}
            onclick={() => onsetpolicy?.(tool.id, 'auto')}
          >
            Approve (set auto)
          </button>
          <button
            type="button"
            class="btn btn-danger"
            data-testid="tools-reject"
            disabled={!canAdmin || approvalPending}
            title={canAdmin ? undefined : adminGateReason}
            onclick={() => onsetpolicy?.(tool.id, 'denied')}
          >
            Reject (set denied)
          </button>
        </div>
        {#if !canAdmin}
          <p class="hint" data-testid="tools-approval-gated">{adminGateReason}</p>
        {:else}
          <p class="hint">
            Approve / Reject invoke the real <code>tools.set_approval_policy</code>
            Protocol method (D-079, admin-scoped).
          </p>
        {/if}
        {#if approvalPending}
          <p class="hint" data-testid="tools-approval-pending">Applying policy…</p>
        {:else if approvalResult !== null}
          <p
            class="hint"
            class:hint-ok={approvalResult.ok}
            class:hint-err={!approvalResult.ok}
            data-testid="tools-approval-result"
          >
            {approvalResult.message}
          </p>
        {/if}
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
    border: var(--border-hairline);
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
    border-bottom: var(--border-hairline);
    margin-bottom: var(--space-3);
  }

  .tab {
    padding: var(--space-2) var(--space-3);
    background: transparent;
    border: none;
    border-bottom: var(--border-hairline-width) solid transparent;
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .tab-active {
    color: var(--color-accent);
    border-bottom-color: var(--color-accent);
  }

  .tab-body {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .muted {
    color: var(--color-text-muted);
  }

  .kv {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-2) var(--space-4);
    margin: var(--space-0);
  }

  .kv dt {
    color: var(--color-text-muted);
  }

  .kv dd {
    margin: var(--space-0);
  }

  pre {
    margin: var(--space-0);
    padding: var(--space-3);
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    overflow-x: auto;
  }

  .approval-actions {
    display: flex;
    gap: var(--space-2);
    margin: var(--space-3) var(--space-0);
  }

  .btn {
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
    background: var(--color-surface-raised);
    color: var(--color-text);
  }

  .btn:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .btn-ok {
    color: var(--color-success);
  }

  .btn-danger {
    color: var(--color-danger);
  }

  .hint {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    margin: var(--space-1) var(--space-0) var(--space-0);
  }

  .hint-ok {
    color: var(--color-success);
  }

  .hint-err {
    color: var(--color-danger);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
