<script lang="ts">
  // Harbor Console — Agents-page fleet-control buttons (Phase 108l /
  // D-184, supersedes the D-132/F4 disabled-with-tooltip stub).
  //
  // The five fleet-control verbs the Agents detail header exposes: Pause /
  // Drain / Restart / Force-Stop / Deregister. They now call the REAL
  // admin-gated `agents.{pause,drain,restart,force_stop,deregister}`
  // Protocol methods (D-184) — the controller owns the call; this component
  // is the presentation seam.
  //
  // # Control-scope gating (D-066, CONVENTIONS.md §5, CLAUDE.md §13)
  //
  // The verbs require the elevated control claim (`admin`). Without it every
  // button is DISABLED-WITH-TOOLTIP — never a fabricated success. With it,
  // clicking a button invokes `onverb(verb)`; the destructive verbs
  // (Force-Stop / Deregister) gate behind a confirm in the controller. While
  // a call is in flight the issuing button shows a busy label and the whole
  // group is disabled so two verbs can't race.
  import type { ControlVerb } from '$lib/agents/derive.js';

  let {
    canControl,
    busy = null,
    onverb
  }: {
    /** True when the connection carries the elevated control claim (D-066). */
    canControl: boolean;
    /** The verb whose call is in flight, or null when idle. */
    busy?: ControlVerb | null;
    /** Invoked with the verb when an enabled button is clicked. */
    onverb: (verb: ControlVerb) => void;
  } = $props();

  interface VerbSpec {
    verb: ControlVerb;
    label: string;
    danger: boolean;
  }

  const VERBS: VerbSpec[] = [
    { verb: 'pause', label: 'Pause', danger: false },
    { verb: 'drain', label: 'Drain', danger: false },
    { verb: 'restart', label: 'Restart', danger: false },
    { verb: 'force_stop', label: 'Force-Stop', danger: true },
    { verb: 'deregister', label: 'Deregister', danger: true }
  ];

  const NO_SCOPE_TOOLTIP =
    'Requires the admin control claim — fleet-control verbs are admin-gated (D-066).';
</script>

<div class="controls" data-testid="agent-control-buttons">
  {#each VERBS as spec (spec.verb)}
    <button
      type="button"
      class="control"
      class:danger={spec.danger}
      data-testid={`agent-control-${spec.verb}`}
      data-control-verb={spec.verb}
      disabled={!canControl || busy !== null}
      aria-disabled={!canControl || busy !== null}
      title={canControl ? `${spec.label} this agent` : NO_SCOPE_TOOLTIP}
      onclick={() => onverb(spec.verb)}
    >
      {busy === spec.verb ? '…' : spec.label}
    </button>
  {/each}
</div>

<style>
  .controls {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .control {
    font-size: var(--text-sm);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    cursor: pointer;
  }

  .control.danger {
    color: var(--color-danger);
  }

  .control:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
</style>
