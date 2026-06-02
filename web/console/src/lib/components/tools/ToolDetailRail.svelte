<script lang="ts">
  // Harbor Console — Tools page per-tool right-rail (Phase 108k / D-183;
  // supersedes the pre-chrome `ToolDetailTabs` + stacked RailCards).
  //
  // The selected-tool detail: ONE packed `.panel.card` that fills the
  // right column and scrolls INTERNALLY (never page-level) — the
  // Events-108h / Background-Jobs-108j `*DetailRail` pattern, NOT a stack
  // of RailCards that page-scrolls. Composition (mockup order): header
  // (name + transport/scope + side-effect / OAuth / approval badges + copy
  // + close) / tab strip (Manifest | Inputs | Outputs | Recent invocations
  // | Approval) / the selected tab / Statistics / Content size & display
  // mode / Source provenance / Run history.
  //
  // Every datum is real-wired (PAGE-POLISH §3 — live-verified):
  //   - header badges + Manifest / Inputs / Outputs ← `tools.describe`
  //   - Statistics (error-rate gauge + status pill) ← `tools.metrics`
  //   - Content size histogram + DisplayMode ← `tools.content_stats`
  //   - Approval Approve / Reject ← the REAL admin `tools.set_approval_policy`
  //     (Approve → auto, Reject → denied) — disabled-with-tooltip without
  //     the `admin` claim (D-079); never a fabricated success (§13).
  //
  // metrics / content_stats are honest-empty for a never-invoked tool (the
  // live probe confirmed every youtube_* tool starts all-zero) — the stat
  // cards render their own honest "no invocations yet" copy, never a
  // fabricated rate/latency. `tools.invoke` is NOT shipped at V1 (D-132),
  // so there is no "try this tool" form here.
  //
  // Page-specific; Svelte 5 runes (D-092); design tokens only. It composes
  // the body-only stat cards (StatusErrorRateCard / ContentSizeCard /
  // RunHistoryStrip) and the shared `ui/StatusChip` — no forked chrome.
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import StatusErrorRateCard from './StatusErrorRateCard.svelte';
  import ContentSizeCard from './ContentSizeCard.svelte';
  import RunHistoryStrip from './RunHistoryStrip.svelte';
  import { oauthKind, approvalKind } from '$lib/tools/derive.js';
  import type {
    Tool,
    ToolManifest,
    ToolMetrics,
    ToolContentStats,
    ToolMetricsWindow,
    ToolApprovalPolicy
  } from '$lib/protocol/tools.js';

  let {
    tool,
    manifest,
    metrics,
    contentStats,
    loading,
    detailError,
    metricsWindow,
    onwindow,
    canAdmin,
    approvalPending,
    approvalResult,
    onsetpolicy,
    onclose
  }: {
    /** The selected catalog row; drives the header + the badges. */
    tool: Tool | null;
    /** The `tools.describe` manifest; null while loading / on error. */
    manifest: ToolManifest | null;
    /** The `tools.metrics` reply for the selected window. */
    metrics: ToolMetrics | null;
    /** The `tools.content_stats` reply. */
    contentStats: ToolContentStats | null;
    /** Whether the detail load is in flight. */
    loading: boolean;
    /** A detail-load error (`{code,message}`), or null. */
    detailError: { code: string; message: string } | null;
    /** The active metrics window (1h / 24h / 7d). */
    metricsWindow: ToolMetricsWindow;
    /** Re-fetch metrics for a new window. */
    onwindow: (w: ToolMetricsWindow) => void;
    /** True when the connection carries the `admin` claim (D-079). */
    canAdmin: boolean;
    /** True while a `set_approval_policy` call is in flight. */
    approvalPending: boolean;
    /** The last real `set_approval_policy` outcome, or null. */
    approvalResult: { ok: boolean; message: string } | null;
    /** Invokes the real `tools.set_approval_policy` admin method. */
    onsetpolicy: (toolID: string, policy: ToolApprovalPolicy) => void;
    /** Closes the rail (clears the selection) — the header ✕. */
    onclose: () => void;
  } = $props();

  type Tab = 'manifest' | 'inputs' | 'outputs' | 'recent' | 'approval';
  let active = $state<Tab>('manifest');

  const TABS: { key: Tab; label: string }[] = [
    { key: 'manifest', label: 'Manifest' },
    { key: 'inputs', label: 'Inputs' },
    { key: 'outputs', label: 'Outputs' },
    { key: 'recent', label: 'Recent invocations' },
    { key: 'approval', label: 'Approval' }
  ];

  function pretty(json: string): string {
    if (!json) return '(none declared)';
    try {
      return JSON.stringify(JSON.parse(json), null, 2);
    } catch {
      return json;
    }
  }

  const adminGateReason =
    'Requires the admin scope claim — set_approval_policy is an admin Protocol method (D-079).';

  async function copyID(): Promise<void> {
    if (tool === null) return;
    try {
      await navigator.clipboard.writeText(tool.id);
    } catch {
      // clipboard denied (insecure context / no permission) — non-fatal
    }
  }
</script>

<section class="panel card tool-rail" data-testid="tools-right-rail">
  {#if tool === null}
    <p class="rail-empty" data-testid="tools-detail-empty">
      Select a tool from the catalog to inspect its descriptor — schema,
      policy, statistics, content profile, provenance and run history.
    </p>
  {:else}
    <header class="rail-header">
      <h2 class="rail-name" data-testid="tools-detail-name" title={tool.id}>{tool.name}</h2>
      <span class="rail-sub">{tool.transport} · {tool.scope} · {tool.reliability_tier}</span>
      <div class="badge-row">
        {#if manifest !== null}
          <StatusChip kind="neutral" label={manifest.side_effect} />
        {/if}
        <StatusChip kind={oauthKind(tool.oauth_status)} label={`OAuth: ${tool.oauth_status}`} />
        <StatusChip kind={approvalKind(tool.approval_policy)} label={tool.approval_policy} />
      </div>
      <div class="header-actions">
        <button type="button" class="icon-btn" data-testid="tools-rail-copy-id" aria-label="Copy tool id" onclick={() => void copyID()}>⧉</button>
        <button type="button" class="icon-btn close" data-testid="tools-rail-close" aria-label="Close detail" onclick={onclose}>✕</button>
      </div>
    </header>

    <div class="tab-strip" role="tablist">
      {#each TABS as tab (tab.key)}
        <button
          type="button"
          class="tab"
          class:active={active === tab.key}
          role="tab"
          aria-selected={active === tab.key}
          data-testid="tools-detail-tab"
          data-tab={tab.key}
          onclick={() => (active = tab.key)}
        >
          {tab.label}
        </button>
      {/each}
    </div>

    <div class="tab-body" data-testid="tools-detail-tab-body">
      {#if loading}
        <p class="muted">Loading descriptor…</p>
      {:else if active === 'manifest'}
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
            <dd><StatusChip kind={approvalKind(manifest.tool.approval_policy)} label={manifest.tool.approval_policy} /></dd>
            <dt>Retry attempts</dt>
            <dd>{manifest.retry_attempts}</dd>
            <dt>Timeout</dt>
            <dd>{manifest.timeout_ms} ms</dd>
            <dt>Loading mode</dt>
            <dd>{manifest.loading_mode}</dd>
            <dt>Examples</dt>
            <dd>{(manifest.examples ?? []).length > 0 ? `${manifest.examples.length} declared` : 'none declared'}</dd>
          </dl>
        {/if}
      {:else if active === 'inputs'}
        <pre data-testid="tools-detail-inputs">{manifest ? pretty(manifest.args_schema) : '—'}</pre>
      {:else if active === 'outputs'}
        <pre data-testid="tools-detail-outputs">{manifest ? pretty(manifest.out_schema) : '—'}</pre>
      {:else if active === 'recent'}
        <p class="muted" data-testid="tools-detail-recent">
          Recent invocations stream from the <code>tool.*</code> event topic
          (the Events page surface); each row deep-links into the originating
          session's bottom dock. No durable read-back is wired here — see the
          Run history strip below for the selected-tool summary.
        </p>
      {:else if active === 'approval'}
        <p class="muted">
          Current approval policy:
          <StatusChip kind={approvalKind(tool.approval_policy)} label={tool.approval_policy} />.
        </p>
        <div class="approval-actions">
          <button
            type="button"
            class="btn btn-ok"
            data-testid="tools-approve"
            disabled={!canAdmin || approvalPending}
            title={canAdmin ? undefined : adminGateReason}
            onclick={() => onsetpolicy(tool.id, 'auto')}
          >
            Approve (set auto)
          </button>
          <button
            type="button"
            class="btn btn-danger"
            data-testid="tools-reject"
            disabled={!canAdmin || approvalPending}
            title={canAdmin ? undefined : adminGateReason}
            onclick={() => onsetpolicy(tool.id, 'denied')}
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

    {#if detailError !== null}
      <p class="hint hint-err" data-testid="tools-detail-error">
        {detailError.code}: {detailError.message}
      </p>
    {/if}

    <!-- Statistics ──────────────────────────────────────────────── -->
    <section class="rail-section">
      <h4 class="section-head">Statistics</h4>
      <StatusErrorRateCard {metrics} window={metricsWindow} {onwindow} />
    </section>

    <!-- Content size & display mode ─────────────────────────────── -->
    <section class="rail-section">
      <h4 class="section-head">Content size &amp; display mode</h4>
      <ContentSizeCard stats={contentStats} />
    </section>

    <!-- Source provenance ───────────────────────────────────────── -->
    <section class="rail-section" data-testid="tools-provenance">
      <h4 class="section-head">Source provenance</h4>
      <dl class="kv">
        <dt>Transport</dt>
        <dd>{tool.transport}</dd>
        <dt>Source</dt>
        <dd>{tool.owner || '—'}</dd>
        <dt>Scope</dt>
        <dd>{tool.scope}</dd>
        <dt>OAuth binding</dt>
        <dd>{manifest?.oauth_binding_scope || 'none'}</dd>
      </dl>
    </section>

    <!-- Run history ─────────────────────────────────────────────── -->
    <section class="rail-section">
      <h4 class="section-head">Run history</h4>
      <RunHistoryStrip {tool} {metrics} />
    </section>
  {/if}
</section>

<style>
  /* ONE packed card that fills the right column and scrolls INTERNALLY
     (never page-level) — the Events-108h / BG-108j *DetailRail pattern. */
  .tool-rail {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 0;
    overflow-y: auto;
  }

  .rail-header {
    display: grid;
    grid-template-columns: 1fr auto;
    grid-template-areas:
      'name actions'
      'sub actions'
      'badges badges';
    gap: var(--space-1) var(--space-2);
    align-items: start;
  }

  .rail-name {
    grid-area: name;
    margin: var(--space-0);
    font-size: var(--text-lg);
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .rail-sub {
    grid-area: sub;
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .badge-row {
    grid-area: badges;
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }

  .header-actions {
    grid-area: actions;
    display: flex;
    gap: var(--space-1);
  }

  .icon-btn {
    background: none;
    border: none;
    color: var(--color-text-muted);
    cursor: pointer;
    font-size: var(--text-sm);
    padding: var(--space-0);
  }

  .tab-strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
    flex-shrink: 0;
  }

  .tab {
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    background: transparent;
    border: none;
    border-bottom: var(--border-hairline-width) solid transparent;
    cursor: pointer;
  }

  .tab.active {
    color: var(--color-accent);
    border-bottom-color: var(--color-accent);
    font-weight: 600;
  }

  .tab-body {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .kv {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
  }

  .kv dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .kv dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    text-align: right;
    overflow: hidden;
    text-overflow: ellipsis;
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
    max-height: var(--layout-rail-list-max);
  }

  .approval-actions {
    display: flex;
    gap: var(--space-2);
    margin: var(--space-3) var(--space-0);
  }

  .btn {
    padding: var(--space-1) var(--space-3);
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

  .rail-section {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    border-top: var(--border-hairline);
    padding-top: var(--space-3);
  }

  .section-head {
    margin: var(--space-0);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .muted {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
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

  .rail-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
