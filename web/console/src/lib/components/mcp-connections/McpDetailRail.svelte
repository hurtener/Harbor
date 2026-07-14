<script lang="ts">
  // Harbor Console — MCP Connections per-server right-rail (Phase 108m /
  // D-185; supersedes the pre-chrome separate tabbed-detail route).
  //
  // The selected-server detail: ONE packed `.panel.card` that fills the right
  // column and scrolls INTERNALLY (never page-level) — the Tools-108k /
  // Agents-108l `*DetailRail` pattern. Composition (mock order): header
  // (name + state badge + transport + endpoint + last-discovery + tool /
  // resource / prompt / OAuth counts + Refresh discovery / Test connection /
  // Raw-HTML toggle) / tab strip (Tools | Resources | Prompts | OAuth
  // bindings | Policy) / the selected tab / Recent events card (live) /
  // Binding-scope summary.
  //
  // Every datum + action is real-wired (PAGE-POLISH §3 — live-verified
  // against the validation runtime's `youtube` MCP server):
  //   - header + counts ← `mcp.servers.get`
  //   - Tools tab ← `tools.list` filtered to this server (owner === name)
  //   - Resources / Prompts / OAuth bindings / Policy ← the matching
  //     `mcp.servers.*` method (lazy on tab select)
  //   - Refresh discovery ← REAL `mcp.servers.refresh_discovery` (honest
  //     re-read counts); Test connection ← REAL `mcp.servers.probe` (the
  //     actual reachable/error outcome — never a faked OK, §13)
  //   - Raw-HTML toggle ← REAL admin `mcp.servers.set_raw_html_trust`
  //     (disabled-with-tooltip without the admin claim, D-079); OAuth
  //     Connect/Revoke ← REAL `mcp.servers.refresh_binding` /
  //     `revoke_binding` (admin-gated) + deep-link to the Tools binding
  //     surface (no parallel OAuth path, §13)
  //   - Recent events ← LIVE `events.subscribe` projection (honest-empty)
  //
  // The youtube server advertises 0 resources / 0 prompts / 0 bindings, so
  // those tabs render their HONEST empty copy — never a fabricated row.
  //
  // Page-specific; Svelte 5 runes (D-092); design tokens only.
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import McpRecentEvents from './McpRecentEvents.svelte';
  import { mcpStatusKind, mcpStateLabel, relativeTime } from '$lib/mcp-connections/derive.js';
  import { oauthKind, approvalKind } from '$lib/tools/derive.js';
  import { MCP_DETAIL_TABS, type McpDetailState } from '$lib/mcp-connections/state.svelte.js';

  let {
    detail,
    onclose
  }: {
    /** The per-server detail controller (the page owns one instance). */
    detail: McpDetailState;
    /** Closes the rail back to the idle overview (the header ✕). */
    onclose: () => void;
  } = $props();

  const rawHtmlGate = 'Requires the admin scope claim — raw-HTML trust is an admin Protocol method (D-079).';
  const oauthGate = 'Requires the admin scope claim — binding admin verbs are admin Protocol methods (D-079).';

  async function copyName(): Promise<void> {
    if (detail.server === null) return;
    try {
      await navigator.clipboard.writeText(detail.server.name);
    } catch {
      // clipboard denied (insecure context / no permission) — non-fatal
    }
  }
</script>

<section class="panel card mcp-rail" data-testid="mcp-detail-rail">
  {#if detail.status === 'loading'}
    <p class="muted" data-testid="mcp-detail-loading">Loading server detail…</p>
  {:else if detail.status === 'error'}
    <div class="rail-error" data-testid="mcp-detail-error">
      <p class="err-headline">{detail.error?.code === 'not_found' ? 'Server not found' : 'Could not load server'}</p>
      <p class="err-detail">
        {detail.error?.code === 'not_found'
          ? 'It may have been removed from the runtime config.'
          : `${detail.error?.code}: ${detail.error?.message}`}
      </p>
      <button type="button" class="btn" onclick={onclose}>Back to overview</button>
    </div>
  {:else if detail.server !== null}
    {@const srv = detail.server}
    <header class="rail-header">
      <h2 class="rail-name" data-testid="rail-server-name" title={srv.name}>{srv.name}</h2>
      <span class="rail-sub">{srv.transport} · last connect {relativeTime(srv.last_discovery_at)}</span>
      <div class="badge-row">
        <StatusChip kind={mcpStatusKind(srv.state)} label={mcpStateLabel(srv.state)} />
        {#if srv.raw_html_trusted}
          <StatusChip kind="warning" label="Raw HTML trusted" />
        {/if}
      </div>
      <div class="header-actions">
        <button type="button" class="icon-btn" data-testid="mcp-rail-copy-name" aria-label="Copy server name" onclick={() => void copyName()}>⧉</button>
        <button type="button" class="icon-btn close" data-testid="mcp-rail-close" aria-label="Close detail" onclick={onclose}>✕</button>
      </div>
    </header>

    <p class="endpoint mono" data-testid="mcp-rail-endpoint">{srv.url_or_command}</p>

    <dl class="count-grid">
      <div><dt>Tools</dt><dd>{srv.tool_count}</dd></div>
      <div><dt>Resources</dt><dd>{srv.resource_count}</dd></div>
      <div><dt>Prompts</dt><dd>{srv.prompt_count}</dd></div>
      <div><dt>OAuth</dt><dd>{srv.oauth_binding_count}</dd></div>
    </dl>

    <div class="head-actions">
      <button
        type="button"
        class="btn"
        data-testid="refresh-discovery"
        disabled={detail.actionBusy !== null}
        onclick={() => void detail.refreshDiscovery()}
      >
        {detail.actionBusy === 'refresh' ? 'Refreshing…' : 'Refresh discovery'}
      </button>
      <button
        type="button"
        class="btn"
        data-testid="test-connection"
        disabled={detail.actionBusy !== null}
        onclick={() => void detail.probe()}
      >
        {detail.actionBusy === 'probe' ? 'Testing…' : 'Test connection'}
      </button>
      <button
        type="button"
        class="btn"
        data-testid="raw-html-toggle"
        disabled={!detail.isAdmin || detail.actionBusy !== null}
        title={detail.isAdmin ? 'Toggle raw-HTML rendering trust for this server (audited)' : rawHtmlGate}
        onclick={() => void detail.setRawHTMLTrust(!srv.raw_html_trusted)}
      >
        {srv.raw_html_trusted ? 'Raw HTML: trusted' : 'Raw HTML: default-deny'}
      </button>
    </div>

    {#if detail.refreshResult !== null}
      <p class="result" class:ok={detail.refreshResult.ok} class:err={!detail.refreshResult.ok} data-testid="refresh-result">
        {detail.refreshResult.message}
      </p>
    {/if}
    {#if detail.probeResult !== null}
      <p class="result" class:ok={detail.probeResult.ok} class:err={!detail.probeResult.ok} data-testid="probe-result">
        {detail.probeResult.ok
          ? `Reachable — round-trip ${detail.probeResult.latency_ms} ms.`
          : `Unreachable — ${detail.probeResult.error ?? 'transport error'}.`}
      </p>
    {/if}
    {#if detail.rawHtmlResult !== null}
      <p class="result" class:ok={detail.rawHtmlResult.ok} class:err={!detail.rawHtmlResult.ok} data-testid="raw-html-result">
        {detail.rawHtmlResult.message}
      </p>
    {/if}

    <!-- Tab strip ─────────────────────────────────────────────────── -->
    <div class="tab-strip" role="tablist" aria-label="Server detail tabs">
      {#each MCP_DETAIL_TABS as tab (tab.id)}
        <button
          type="button"
          class="tab"
          class:active={detail.activeTab === tab.id}
          role="tab"
          aria-selected={detail.activeTab === tab.id}
          data-testid={`tab-${tab.id}`}
          onclick={() => void detail.selectTab(tab.id)}
        >
          {tab.label}
        </button>
      {/each}
    </div>

    <div class="tab-body" data-testid={`tab-body-${detail.activeTab}`}>
      {#if detail.activeTab === 'tools'}
        {#if detail.toolsStatus === 'loading'}
          <p class="muted">Loading tools…</p>
        {:else if detail.tools.length === 0}
          <p class="muted">This server exposes no tools.</p>
        {:else}
          <ul class="tool-list">
            {#each detail.tools as t (t.id)}
              <li data-testid="server-tool-row">
                <a class="tool-name" href={`/tools/${t.id}`}>{t.name}</a>
                <span class="tool-chips">
                  <StatusChip kind={approvalKind(t.approval_policy)} label={t.approval_policy} />
                  <StatusChip kind={oauthKind(t.oauth_status)} label={`OAuth: ${t.oauth_status}`} />
                </span>
              </li>
            {/each}
          </ul>
        {/if}
        <a class="deep-link" data-testid="tools-deep-link" href={`/tools?server=${srv.name}`}>
          Open all in Tools page →
        </a>
      {:else if detail.activeTab === 'resources'}
        {#if detail.resources.length === 0}
          <p class="muted" data-testid="resources-empty">No resources advertised by this server.</p>
        {:else}
          <ul class="entity-list">
            {#each detail.resources as r (r.uri)}
              <li data-testid="resource-row">
                <span class="mono">{r.uri}</span>
                {#if r.mime_type}<span class="entity-meta">{r.mime_type}</span>{/if}
              </li>
            {/each}
          </ul>
        {/if}
      {:else if detail.activeTab === 'prompts'}
        {#if detail.prompts.length === 0}
          <p class="muted" data-testid="prompts-empty">No prompts advertised by this server.</p>
        {:else}
          <ul class="entity-list">
            {#each detail.prompts as p (p.name)}
              <li data-testid="prompt-row">
                <span class="mono">{p.name}</span>
                {#if p.description}<span class="entity-meta">{p.description}</span>{/if}
              </li>
            {/each}
          </ul>
        {/if}
      {:else if detail.activeTab === 'oauth'}
        {#if detail.bindings.length === 0}
          <p class="muted" data-testid="bindings-empty">
            No OAuth bindings configured for this server. When a tool requires
            OAuth, manage the binding from the Tools page.
          </p>
        {:else}
          <ul class="entity-list">
            {#each detail.bindings as b (b.principal_id + b.binding_scope)}
              <li class="binding-row" data-testid="binding-row">
                <span class="binding-meta">
                  <span class="binding-scope">{b.binding_scope}</span>
                  <span class="mono">{b.principal_id}</span>
                </span>
                <span class="binding-actions">
                  <button
                    type="button"
                    class="btn btn-sm"
                    data-testid="binding-connect"
                    disabled={!detail.isAdmin}
                    title={detail.isAdmin ? 'Start an OAuth re-bind flow for this principal' : oauthGate}
                    onclick={() => void detail.connectBinding(b.principal_id)}
                  >
                    Reconnect
                  </button>
                  <button
                    type="button"
                    class="btn btn-sm btn-danger"
                    data-testid="binding-revoke"
                    disabled={!detail.isAdmin}
                    title={detail.isAdmin ? 'Revoke this OAuth binding' : oauthGate}
                    onclick={() => void detail.revokeBinding(b.principal_id)}
                  >
                    Revoke
                  </button>
                </span>
              </li>
            {/each}
          </ul>
        {/if}
      {:else if detail.activeTab === 'policy'}
        <dl class="policy-grid" data-testid="policy-grid">
          <dt>Timeout</dt>
          <dd>{detail.toolPolicy ? `${detail.toolPolicy.timeout_ms} ms` : '—'}</dd>
          <dt>Max retries</dt>
          <dd>{detail.toolPolicy?.max_retries ?? '—'}</dd>
          <dt>Concurrency cap</dt>
          <dd>{detail.toolPolicy ? detail.toolPolicy.concurrency_cap || 'unbounded' : '—'}</dd>
        </dl>
        <p class="muted">Read-only at V1 — edit lands with the post-V1 policy admin UI.</p>
      {/if}
    </div>

    {#if detail.lastActionError !== null}
      <p class="result err" data-testid="action-error" role="alert">
        {detail.lastActionError.code}: {detail.lastActionError.message}
      </p>
    {/if}

    <!-- Recent events (live) ──────────────────────────────────────── -->
    <section class="rail-section">
      <h4 class="section-head">Recent events</h4>
      <McpRecentEvents entries={detail.recentEvents} streamState={detail.streamState} />
    </section>

    <!-- Binding-scope summary ─────────────────────────────────────── -->
    <section class="rail-section" data-testid="binding-scope-summary">
      <h4 class="section-head">Binding-scope summary</h4>
      {#if detail.bindingsSummary.length === 0}
        <p class="muted">No OAuth bindings on this server.</p>
      {:else}
        <dl class="policy-grid">
          {#each detail.bindingsSummary as s (s.binding_scope)}
            <dt>{s.binding_scope}</dt>
            <dd>{s.count}</dd>
          {/each}
        </dl>
      {/if}
    </section>

    <!-- Discovered OAuth requirement (D-297) ──────────────────────── -->
    {#if srv.oauth_requirement}
      {@const req = srv.oauth_requirement}
      <section class="rail-section" data-testid="oauth-requirement">
        <h4 class="section-head">Discovered OAuth requirement</h4>
        <p class="muted oauth-req-caveat" data-testid="oauth-requirement-unverified">
          Unverified — advertised by the connected server. Harbor never runs the
          flow, holds a token, or dials a discovered endpoint.
        </p>
        {#if req.authorization_servers.length === 0}
          <p class="muted" data-testid="oauth-requirement-partial">
            No authorization server resolved. The metadata chain surfaced
            partially — see the discovery status below.
          </p>
        {:else}
          {#each req.authorization_servers as as (as.issuer + as.source_url)}
            <dl class="policy-grid" data-testid="oauth-requirement-as">
              <dt>Issuer</dt>
              <dd class="mono">{as.issuer || '—'}</dd>
              <dt>Authorize</dt>
              <dd class="mono">{as.authorization_endpoint || '—'}</dd>
              <dt>Token</dt>
              <dd class="mono">{as.token_endpoint || '—'}</dd>
              <dt>Scopes</dt>
              <dd>{as.scopes_supported.length > 0 ? as.scopes_supported.join(', ') : 'none advertised'}</dd>
              <dt>PKCE</dt>
              <dd>{as.code_challenge_methods_supported.length > 0 ? as.code_challenge_methods_supported.join(', ') : 'not advertised'}</dd>
              {#if as.registration_endpoint}
                <dt>Registration</dt>
                <dd class="mono">{as.registration_endpoint}</dd>
              {/if}
            </dl>
          {/each}
        {/if}
        <dl class="policy-grid">
          <dt>Source</dt>
          <dd>{req.source} · discovered {relativeTime(req.discovered_at)}</dd>
          <dt>Source URL</dt>
          <dd class="mono">{req.source_url}</dd>
        </dl>
        {#if req.status.some((s) => !s.ok)}
          <ul class="oauth-req-status" data-testid="oauth-requirement-status">
            {#each req.status.filter((s) => !s.ok) as st (st.step + st.target)}
              <li class="muted">{st.step}: {st.reason}{st.detail ? ` — ${st.detail}` : ''}</li>
            {/each}
          </ul>
        {/if}
        <!-- See-it-here / fix-it-there: the allowance WRITE is single-homed on
             the Agent Config page (beside diff/rollback). This page only
             deep-links to it — never a second write form (D-302 / D-062). -->
        {#if req.status.some((s) => !s.ok && s.reason === 'needs_allowance')}
          <p class="muted oauth-req-caveat" data-testid="oauth-requirement-allowance-hint">
            An authorization-server origin needs an allowance grant. Discovery
            origins are edited on the agent’s configuration (beside diff /
            rollback), not here. A boot-declared (yaml) server is edited in
            <code>harbor.yaml</code> and applied by a restart.
            <a
              class="allowance-link"
              href={`/agent-config?connection=${encodeURIComponent(srv.name)}&grant_origin=${encodeURIComponent(req.status.find((s) => !s.ok && s.reason === 'needs_allowance')?.target ?? '')}`}
              data-testid="oauth-requirement-grant-link"
            >
              Grant on Agent Config →
            </a>
          </p>
        {/if}
      </section>
    {/if}

    <!-- Content shapes / display modes ────────────────────────────── -->
    <section class="rail-section">
      <h4 class="section-head">Content shapes &amp; display modes</h4>
      <dl class="policy-grid">
        <dt>Content shapes</dt>
        <dd>{detail.contentShapes.length > 0 ? detail.contentShapes.join(', ') : 'none advertised'}</dd>
        <dt>Display modes</dt>
        <dd>{detail.displayModes.length > 0 ? detail.displayModes.join(', ') : 'none advertised'}</dd>
      </dl>
    </section>
  {/if}
</section>

<style>
  /* ONE packed card that fills the right column and scrolls INTERNALLY
     (never page-level) — the Tools-108k / Agents-108l *DetailRail pattern.
     The `.card` padding in +page.svelte is Svelte-scoped to that component,
     so the rail supplies its own inset + `scrollbar-gutter: stable` so the
     scrollbar never overlays right-aligned values. */
  .mcp-rail {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 0;
    overflow-y: auto;
    padding: var(--space-3);
    scrollbar-gutter: stable;
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

  .endpoint {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    word-break: break-all;
  }

  .count-grid {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: var(--space-2);
    margin: var(--space-0);
  }

  .count-grid div {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2);
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .count-grid dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .count-grid dd {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
    font-variant-numeric: tabular-nums;
  }

  .head-actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .btn {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    cursor: pointer;
    background: var(--color-surface-raised);
    color: var(--color-text);
  }

  .btn:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .btn-sm {
    padding: var(--space-1) var(--space-2);
  }

  .btn-danger {
    color: var(--color-danger);
  }

  .result {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .result.ok {
    color: var(--color-success);
  }

  .result.err {
    color: var(--color-danger);
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
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .tool-list,
  .entity-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .tool-list li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-0);
    border-bottom: var(--border-hairline);
  }

  .tool-name {
    font-size: var(--text-sm);
    color: var(--color-accent);
    text-decoration: none;
  }

  .tool-chips {
    display: flex;
    gap: var(--space-1);
    flex-wrap: wrap;
  }

  .entity-list li {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-0);
    border-bottom: var(--border-hairline);
  }

  .entity-meta {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .binding-row {
    flex-direction: row !important;
    align-items: center;
    justify-content: space-between;
  }

  .binding-meta {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .binding-scope {
    display: inline-block;
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
  }

  .binding-actions {
    display: flex;
    gap: var(--space-1);
  }

  .deep-link {
    font-size: var(--text-xs);
    color: var(--color-accent);
    text-decoration: none;
  }

  .policy-grid {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-1) var(--space-4);
    margin: var(--space-0);
  }

  .policy-grid dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .policy-grid dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
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

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .rail-error {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: var(--space-2);
  }

  .err-headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .err-detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .oauth-req-caveat {
    margin: var(--space-0) var(--space-0) var(--space-2);
  }

  .oauth-req-status {
    margin: var(--space-2) var(--space-0) var(--space-0);
    padding-inline-start: var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--text-sm);
  }
</style>
