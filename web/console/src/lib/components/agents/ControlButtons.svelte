<script lang="ts">
  // Harbor Console — Agents-page control buttons (Phase 73e / D-124).
  //
  // The five fleet-control verbs the Agents detail header exposes:
  // Pause / Drain / Restart / Force-Stop / Deregister.
  //
  // # No `registry.*` Protocol surface yet — buttons are inert (D-132)
  //
  // The shipped `registry.*` control verbs (D-066) are an IN-PROCESS Go
  // API; there is NO Protocol method a Console client can call to
  // pause / drain / restart / force-stop / deregister an agent. The
  // Wave 13 §17.5 checkpoint (D-132 / F4) pinned that wiring these
  // buttons to a `controlFeedback` string was a fake-success path
  // (CLAUDE.md §13 "test stubs as production defaults" / "silent
  // degradation").
  //
  // Until a `registry.*` Protocol surface exists, every control button
  // is rendered DISABLED-WITH-TOOLTIP — REGARDLESS of the operator's
  // scope claim. The scope claim is irrelevant while there is no method
  // to call; re-enabling the buttons is the job of the future
  // fleet-control Protocol-surface phase, which lands the methods AND
  // flips these buttons live in the same wave (CLAUDE.md §13
  // primitive-with-consumer).

  /** The five canonical fleet-control verbs (D-066). */
  type ControlVerb =
    | 'pause'
    | 'drain'
    | 'restart'
    | 'force_stop'
    | 'deregister';

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

  const DISABLED_TOOLTIP =
    'Fleet-control Protocol surface lands in a later wave — no registry.* method exists yet (D-132)';
</script>

<div class="controls" data-testid="agent-control-buttons">
  {#each VERBS as spec (spec.verb)}
    <button
      type="button"
      class="control"
      class:danger={spec.danger}
      data-testid={`agent-control-${spec.verb}`}
      data-control-verb={spec.verb}
      disabled
      aria-disabled="true"
      title={DISABLED_TOOLTIP}
    >
      {spec.label}
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
