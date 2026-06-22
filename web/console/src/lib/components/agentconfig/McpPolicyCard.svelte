<script lang="ts">
  // Harbor Console — agent-config MCP pause/resume + per-tool disable (92d).
  //
  // Edits the desired-state tool-exposure: per-server pause + per-tool
  // disable. The set is seeded from the active config; saving replaces ONLY
  // the exposure section (`agent_config.set_tool_exposure`) and emits
  // `mcp.connection.paused` / `.resumed`, which the panel's live event feed
  // reflects. The operator adds servers/tools by name (the exposure model is
  // exclusion-based — a paused server's tools drop from the next run while
  // the transport stays warm). Writes are admin-gated (disabled-with-tooltip
  // — the 92b precedent). Page-specific; Tokens only; Svelte 5 runes (D-092).
  import { StatusChip } from '$lib/components/ui';
  import type { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';

  let { panel }: { panel: AgentConfigPanelState } = $props();

  let serverInput = $state('');
  let toolInput = $state('');

  function addServer(): void {
    const name = serverInput.trim();
    if (name === '' || panel.pausedServers.includes(name)) return;
    panel.toggleServerPaused(name);
    serverInput = '';
  }
  function addTool(): void {
    const key = toolInput.trim();
    if (key === '' || panel.disabledTools.includes(key)) return;
    panel.toggleToolDisabled(key);
    toolInput = '';
  }
</script>

<div class="card-body" data-testid="agentcfg-mcp-policy">
  <p class="note">
    Pause MCP servers (their tools drop from the next run; the live transport
    stays warm) or disable individual tools (keyed <code>source_tool</code>).
    Saving records a revision and emits the pause/resume events shown live below.
  </p>

  <div class="policy-grid">
    <section class="policy-col">
      <h4 class="policy-title">Paused servers</h4>
      <div class="chip-row" data-testid="agentcfg-paused-servers">
        {#each panel.pausedServers as server (server)}
          <span class="removable">
            <StatusChip kind="warning" label={server} />
            <button
              type="button"
              class="chip-x"
              aria-label={`Resume ${server}`}
              disabled={!panel.hasAdminScope}
              onclick={() => panel.toggleServerPaused(server)}>×</button
            >
          </span>
        {:else}
          <span class="muted">No servers paused.</span>
        {/each}
      </div>
      <div class="add-row">
        <input
          type="text"
          placeholder="server name"
          data-testid="agentcfg-paused-server-input"
          bind:value={serverInput}
          disabled={!panel.hasAdminScope}
          onkeydown={(e) => e.key === 'Enter' && (e.preventDefault(), addServer())}
        />
        <button
          type="button"
          class="ghost"
          data-testid="agentcfg-paused-server-add"
          disabled={!panel.hasAdminScope || serverInput.trim() === ''}
          onclick={addServer}>Pause</button
        >
      </div>
    </section>

    <section class="policy-col">
      <h4 class="policy-title">Disabled tools</h4>
      <div class="chip-row" data-testid="agentcfg-disabled-tools">
        {#each panel.disabledTools as tool (tool)}
          <span class="removable">
            <StatusChip kind="danger" label={tool} />
            <button
              type="button"
              class="chip-x"
              aria-label={`Enable ${tool}`}
              disabled={!panel.hasAdminScope}
              onclick={() => panel.toggleToolDisabled(tool)}>×</button
            >
          </span>
        {:else}
          <span class="muted">No tools disabled.</span>
        {/each}
      </div>
      <div class="add-row">
        <input
          type="text"
          placeholder="source_tool"
          data-testid="agentcfg-disabled-tool-input"
          bind:value={toolInput}
          disabled={!panel.hasAdminScope}
          onkeydown={(e) => e.key === 'Enter' && (e.preventDefault(), addTool())}
        />
        <button
          type="button"
          class="ghost"
          data-testid="agentcfg-disabled-tool-add"
          disabled={!panel.hasAdminScope || toolInput.trim() === ''}
          onclick={addTool}>Disable</button
        >
      </div>
    </section>
  </div>

  <div class="actions">
    <button
      type="button"
      class="primary"
      data-testid="agentcfg-exposure-save"
      disabled={!panel.hasAdminScope || panel.exposurePhase === 'saving'}
      title={panel.hasAdminScope
        ? 'Save the MCP pause / per-tool policy (records a revision)'
        : 'Setting MCP policy requires the admin scope claim'}
      onclick={() => void panel.saveExposure()}
    >
      {panel.exposurePhase === 'saving' ? 'Saving…' : 'Save policy'}
    </button>
    {#if !panel.hasAdminScope}
      <span class="hint" data-testid="agentcfg-exposure-disabled-hint">
        Requires the admin scope claim.
      </span>
    {/if}
    {#if panel.exposureSaved}
      <span class="saved" data-testid="agentcfg-exposure-saved">Saved — effective next run.</span>
    {/if}
  </div>
  {#if panel.exposureError}
    <p class="form-error" data-testid="agentcfg-exposure-error">
      {panel.exposureError.code}: {panel.exposureError.message}
    </p>
  {/if}

  {#if panel.mcpEvents.length > 0}
    <section class="events">
      <h4 class="policy-title">Live connection events (runtime-wide)</h4>
      <ul class="event-list" data-testid="agentcfg-mcp-events">
        {#each panel.mcpEvents.slice(0, 6) as ev (ev.sequence)}
          <li class="event-row">
            <code class="event-type">{ev.type}</code>
            <span class="event-time">{new Date(ev.occurred_at).toLocaleTimeString()}</span>
          </li>
        {/each}
      </ul>
    </section>
  {/if}
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }
  .note {
    margin: var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
  .policy-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-3);
  }
  .policy-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .policy-title {
    margin: var(--space-0);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }
  .chip-row {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    min-height: var(--space-5);
    align-items: center;
  }
  .removable {
    display: inline-flex;
    align-items: center;
    gap: var(--space-05);
  }
  .chip-x {
    border: none;
    background: transparent;
    color: var(--color-text-muted);
    cursor: pointer;
    font-size: var(--text-sm);
    line-height: 1;
    padding: var(--space-0);
  }
  .chip-x:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
  .add-row {
    display: flex;
    gap: var(--space-1);
  }
  input {
    flex: 1;
    min-width: 0;
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
    color: var(--color-text);
    font-size: var(--text-sm);
  }
  input:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .primary {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
    background: var(--color-accent);
    border-color: var(--color-accent);
    color: var(--color-bg);
  }
  .ghost {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    cursor: pointer;
    background: var(--color-surface);
    color: var(--color-text);
  }
  .primary:disabled,
  .ghost:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .saved {
    font-size: var(--text-xs);
    color: var(--color-success);
  }
  .form-error {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-danger);
  }
  .events {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding-top: var(--space-2);
    border-top: var(--border-hairline);
  }
  .event-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-05);
  }
  .event-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }
  .event-type {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
  }
  .event-time {
    font-size: var(--text-2xs);
    color: var(--color-text-muted);
  }
</style>
