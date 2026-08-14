<script lang="ts">
  // Harbor Console — read-only effective-composition preview (D-414 /
  // D-415, the HA-66 composition-preview consumer).
  //
  // Renders what the strict run-start composer WOULD compose for the
  // selected agent WITHOUT materialising anything: no lifecycle
  // creation, no admin pack verb, no revision write, no skill / artifact
  // store write. The data comes from the read-only
  // `agent_config.composition.preview` Protocol method (the SAME surface
  // the `harbor composition-preview` CLI consumes — one composition
  // path, §13). The typed outcome (available | unavailable | conflict |
  // retired), the deterministic set hashes, and the items with their
  // exact boot|revision|both provenance + canonical semantic hash ride
  // verbatim — never a blank state (D-311).
  //
  // Boot-only items render READ-ONLY (a chip, no remove affordance). The
  // "Remove durable revision shadow" button is only offered for a real
  // durable revision shadow (source revision | both) and goes through
  // the durable-revision-authoring verb agent_config.agent_packs.remove
  // — removing the shadow leaves a boot-declared baseline untouched. A
  // typed boot-owned refusal from the runtime stays visible (never
  // swallowed — §13). Writes are admin-gated (disabled-with-tooltip).
  //
  // Built against CONVENTIONS.md (D-121): tokens only, Svelte 5 runes
  // (D-092), StatusChip from the shared ui/ inventory, no fetch.
  import { StatusChip } from '$lib/components/ui';
  import { previewSourceIsBootOnly } from '$lib/agentconfig/state.svelte.js';
  import type { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';

  let { panel }: { panel: AgentConfigPanelState } = $props();

  /** The chip kind for a typed outcome. Unavailable is non-oracular —
   * neutral, never a scary red. */
  function outcomeKind(outcome: string | undefined): 'success' | 'warning' | 'danger' | 'neutral' {
    switch (outcome) {
      case 'available':
        return 'success';
      case 'conflict':
        return 'warning';
      case 'retired':
        return 'danger';
      default:
        return 'neutral';
    }
  }

  /** The chip kind for a provenance marker. */
  function sourceKind(source: string | undefined): 'accent' | 'info' | 'neutral' {
    switch (source) {
      case 'boot':
        return 'accent';
      case 'revision':
        return 'info';
      default:
        return 'neutral';
    }
  }

  /** shortHash renders a copy-friendly hash (first 12 chars). */
  function shortHash(h: string): string {
    return h.length > 12 ? `${h.slice(0, 12)}…` : h;
  }

  /** The exact boot|revision|both provenance label. */
  function sourceLabel(source: string | undefined): string {
    return source === 'boot' || source === 'revision' || source === 'both' ? source : 'unknown';
  }
</script>

<div class="card-body" data-testid="agentcfg-composition-preview">
  <p class="note">
    Read-only preview of the effective operator-tier composition for
    <span class="agent-name">{panel.agentId}</span> — what the next run&rsquo;s strict
    composer would compose. Nothing is materialised: durable revisions are authored
    through the Skills area and <code>agent_packs.list</code>, never here.
  </p>

  {#if panel.previewPhase === 'loading'}
    <p class="note" data-testid="agentcfg-preview-loading">Resolving composition&hellip;</p>
  {:else if panel.previewPhase === 'error'}
    <p class="form-error" data-testid="agentcfg-preview-error">
      {panel.previewError?.code}: {panel.previewError?.message}
    </p>
    <div class="actions">
      <button type="button" class="ghost" data-testid="agentcfg-preview-retry" onclick={() => void panel.loadCompositionPreview()}>
        Retry
      </button>
    </div>
  {:else if panel.preview}
    {@const preview = panel.preview}
    <div class="outcome-row" data-testid="agentcfg-preview-outcome">
      <StatusChip kind={outcomeKind(preview.outcome)} label={preview.outcome} />
      {#if preview.outcome === 'available' && preview.items?.length}
        <span class="note">{preview.items.length} effective item(s)</span>
      {/if}
      {#if preview.widened}
        <span class="note">widened read (audited)</span>
      {/if}
    </div>

    {#if preview.outcome === 'available'}
      <ul class="hash-list" data-testid="agentcfg-preview-hashes">
        {#if preview.boot_pack_set_hash}
          <li><span class="hash-key">boot_pack_set_hash</span><span class="hash-val">{preview.boot_pack_set_hash}</span></li>
        {/if}
        {#if preview.combined_hash}
          <li><span class="hash-key">combined_hash</span><span class="hash-val">{preview.combined_hash}</span></li>
        {/if}
        {#if preview.revision_hash}
          <li><span class="hash-key">revision_hash</span><span class="hash-val">{preview.revision_hash}</span></li>
        {/if}
        {#if preview.revision_id}
          <li><span class="hash-key">revision_id</span><span class="hash-val">{preview.revision_id}</span></li>
        {/if}
        {#if preview.content_hash}
          <li><span class="hash-key">content_hash</span><span class="hash-val">{preview.content_hash}</span></li>
        {/if}
      </ul>

      {#if !preview.items?.length}
        <p class="note" data-testid="agentcfg-preview-empty">No effective operator-tier items — the composed tier is empty.</p>
      {:else}
        <ul class="item-list" data-testid="agentcfg-preview-items">
          {#each preview.items as item (item.name)}
            {@const bootOnly = previewSourceIsBootOnly(item.source)}
            <li class="item-row" data-testid="agentcfg-preview-item">
              <div class="item-main">
                <span class="item-name">{item.skill.title || item.name}</span>
                <StatusChip kind={sourceKind(item.source)} label={sourceLabel(item.source)} />
                {#if bootOnly}
                  <StatusChip kind="neutral" label="boot-declared (read-only)" />
                {/if}
                <span class="item-hash" title={item.semantic_hash}>{shortHash(item.semantic_hash)}</span>
                {#if item.skill.trigger}
                  <span class="item-trigger">{item.skill.trigger}</span>
                {/if}
              </div>
              {#if bootOnly}
                <span class="readonly-label" data-testid="agentcfg-preview-item-readonly">No durable shadow</span>
              {:else}
                <button
                  type="button"
                  class="ghost danger"
                  data-testid="agentcfg-preview-remove-shadow"
                  disabled={!panel.hasAdminScope || panel.previewRemoveBusy !== ''}
                  title={panel.hasAdminScope
                    ? 'Remove the durable revision shadow (the boot baseline, if any, is untouched)'
                    : 'Removing a durable revision shadow requires the admin scope claim'}
                  onclick={() => void panel.removePreviewShadow(item.name)}
                >
                  {panel.previewRemoveBusy === item.name ? 'Removing…' : 'Remove shadow'}
                </button>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}
    {:else if preview.outcome === 'unavailable'}
      <p class="note" data-testid="agentcfg-preview-unavailable">
        No boot-declared composition is readable for this (tenant, agent) — or the
        caller is not entitled to the target (non-oracular).
      </p>
    {:else if preview.outcome === 'conflict'}
      <p class="note" data-testid="agentcfg-preview-conflict">
        The strict composer refused a boot/revision conflict
        {#if preview.conflict_name} — first offending name: <code>{preview.conflict_name}</code>{/if}.
        A canonical name&rsquo;s semantic content differs across the boot baseline and the
        active revision; never a silent overwrite.
      </p>
    {:else if preview.outcome === 'retired'}
      <p class="note" data-testid="agentcfg-preview-retired">
        The effective agent&rsquo;s terminal lifecycle tombstone is installed — the
        composition is no longer readable.
      </p>
    {:else}
      <p class="form-error" data-testid="agentcfg-preview-unexpected">
        Unexpected outcome {preview.outcome} — the Runtime answered outside the typed set.
      </p>
    {/if}

    {#if panel.previewRemoveError}
      <p class="form-error" data-testid="agentcfg-preview-remove-error">
        {panel.previewRemoveError.code}: {panel.previewRemoveError.message}
      </p>
    {/if}

    <div class="actions">
      <button type="button" class="ghost" data-testid="agentcfg-preview-refresh" onclick={() => void panel.loadCompositionPreview()}>
        Refresh preview
      </button>
    </div>
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
  .agent-name {
    font-weight: 600;
    color: var(--color-text);
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    padding: var(--space-0) var(--space-1);
  }
  .outcome-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .hash-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .hash-list li {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }
  .hash-key {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    min-width: var(--size-rail);
    flex-shrink: 0;
  }
  .hash-val {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
    overflow-wrap: anywhere;
  }
  .item-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .item-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
  }
  .item-main {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    min-width: 0;
  }
  .item-name {
    font-weight: 600;
    color: var(--color-text);
  }
  .item-hash {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .item-trigger {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .readonly-label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
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
  .ghost.danger {
    color: var(--color-danger);
  }
  .ghost:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .form-error {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-danger);
  }
</style>
