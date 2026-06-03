<script lang="ts">
  // Harbor Console — Agents detail header (Phase 73e / D-124; control
  // verbs wired live in Phase 108l / D-184). The detail-mode row-1: name +
  // health badge + status pill + version_hash + the five fleet-control
  // buttons + "Open in Playground" + copy-id. Page-specific component;
  // composes the shared StatusChip + the page's ControlButtons.
  import { StatusChip } from '$lib/components/ui';
  import ControlButtons from './ControlButtons.svelte';
  import { healthKind, statusKind, type ControlVerb } from '$lib/agents/derive.js';
  import type { Agent } from '$lib/protocol/agents.js';

  let {
    agent,
    canControl,
    controlBusy = null,
    controlResult = null,
    onverb
  }: {
    agent: Agent;
    /** True when the connection carries the elevated control claim (D-066). */
    canControl: boolean;
    /** The control verb whose call is in flight, or null. */
    controlBusy?: ControlVerb | null;
    /** The inline result of the last control call (honest re-read status). */
    controlResult?: { ok: boolean; message: string } | null;
    /** Invoked with the verb when a control button is clicked. */
    onverb: (verb: ControlVerb) => void;
  } = $props();

  function copyID(): void {
    void globalThis.navigator?.clipboard?.writeText(agent.id);
  }
</script>

<header class="detail-header" data-testid="agent-detail-header">
  <div class="title-row">
    <div class="title-block">
      <h1 data-testid="agent-detail-name">{agent.name || agent.id}</h1>
      <StatusChip kind={healthKind(agent.health)} label={agent.health} />
      <StatusChip kind={statusKind(agent.status)} label={agent.status} />
    </div>
    <div class="meta-row">
      <span class="version mono" data-testid="agent-detail-version">
        {agent.version_hash || 'no version'}
      </span>
      <button
        type="button"
        class="meta-btn"
        data-testid="agent-copy-id"
        onclick={copyID}
        title="Copy agent ID"
      >
        Copy ID
      </button>
      <a
        class="meta-btn"
        data-testid="agent-open-playground"
        href="/playground"
      >
        Open in Playground
      </a>
    </div>
  </div>

  <ControlButtons {canControl} busy={controlBusy} {onverb} />

  {#if controlResult !== null}
    <p
      class="control-result"
      class:ok={controlResult.ok}
      class:err={!controlResult.ok}
      data-testid="agent-control-result"
    >
      {controlResult.message}
    </p>
  {/if}
</header>

<style>
  .detail-header {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .title-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .title-block {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  h1 {
    margin: var(--space-0);
    font-size: var(--text-xl);
    color: var(--color-text);
  }

  .meta-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .version {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .meta-btn {
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    cursor: pointer;
    text-decoration: none;
  }

  .mono {
    font-family: var(--font-mono);
    overflow-wrap: anywhere;
  }

  .control-result {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .control-result.ok {
    color: var(--color-success);
  }

  .control-result.err {
    color: var(--color-danger);
  }
</style>
