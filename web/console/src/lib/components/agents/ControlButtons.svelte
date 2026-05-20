<script lang="ts">
  // Harbor Console — Agents-page control buttons (Phase 73e / D-124).
  //
  // The five fleet-control verbs the Agents detail header exposes:
  // Pause / Drain / Restart / Force-Stop / Deregister. Per page-agents.md
  // §9 + D-066, every control verb requires the elevated control-scope
  // claim — strictly higher than ordinary identity scope ("a leaked
  // read-only token must not be able to force-stop a fleet").
  //
  // # Scope-claim degradation (CONVENTIONS.md §5, CLAUDE.md §13)
  //
  // A control button is either ENABLED (the operator carries the control
  // claim — it invokes the shipped `registry.*` control verb) or rendered
  // DISABLED-WITH-TOOLTIP explaining the missing claim. It NEVER fakes a
  // success. The control-scope claim resolves through `connection.ts`'s
  // `hasScope`; the Console gates the verbs on `auth.ScopeAdmin` (D-079 —
  // the closed two-scope set; agent control/admin gates on `admin`).
  //
  // The `oninvoke` callback is the seam the page wires to the shipped
  // registry control surface. When `controlEnabled` is false the buttons
  // are inert — `oninvoke` is never reached.

  /** The five canonical fleet-control verbs (D-066). */
  export type ControlVerb =
    | 'pause'
    | 'drain'
    | 'restart'
    | 'force_stop'
    | 'deregister';

  let {
    controlEnabled,
    oninvoke
  }: {
    /** True iff the operator carries the elevated control-scope claim. */
    controlEnabled: boolean;
    /** Invoked with the chosen verb when a control button is clicked. */
    oninvoke: (verb: ControlVerb) => void;
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

  const DISABLED_TOOLTIP =
    'Requires the elevated control-scope claim (D-066) — request an admin-scoped token';
</script>

<div class="controls" data-testid="agent-control-buttons">
  {#each VERBS as spec (spec.verb)}
    <button
      type="button"
      class="control"
      class:danger={spec.danger}
      data-testid={`agent-control-${spec.verb}`}
      data-control-verb={spec.verb}
      disabled={!controlEnabled}
      aria-disabled={!controlEnabled}
      title={controlEnabled ? `${spec.label} this agent` : DISABLED_TOOLTIP}
      onclick={() => controlEnabled && oninvoke(spec.verb)}
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
