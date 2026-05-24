<script lang="ts" module>
  // Harbor Console — shared StatusChip (D-121, CONVENTIONS.md §3).
  //
  // One status pill. The `kind` prop maps over the status token scale —
  // a page never hand-rolls a coloured pill. The audit found per-page
  // status pills forked across all five pages; this is the one.

  /** The closed set of status kinds, mapped onto the status token scale. */
  export type StatusKind = 'success' | 'warning' | 'danger' | 'accent' | 'neutral';
</script>

<script lang="ts">
  let {
    kind = 'neutral',
    label,
    desaturated = false
  }: {
    /** The status kind — selects the token-driven colour. */
    kind?: StatusKind;
    /** The pill text. */
    label: string;
    /** When true, the chip drops its kind-coloured styling and renders
        in the neutral palette (Phase 83r / N8). Pages set this on
        chips whose state is meaningless when the Console is
        disconnected — the colour without a backing Runtime is noise. */
    desaturated?: boolean;
  } = $props();
</script>

<span
  class="status-chip"
  data-kind={desaturated ? 'neutral' : kind}
  data-desaturated={desaturated ? 'true' : undefined}
>{label}</span>

<style>
  .status-chip {
    display: inline-flex;
    align-items: center;
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    font-weight: 600;
    line-height: 1;
    white-space: nowrap;
  }

  .status-chip[data-kind='success'] {
    color: var(--color-success);
    background: var(--color-success-soft);
  }

  .status-chip[data-kind='warning'] {
    color: var(--color-warning);
    background: var(--color-warning-soft);
  }

  .status-chip[data-kind='danger'] {
    color: var(--color-danger);
    background: var(--color-danger-soft);
  }

  .status-chip[data-kind='accent'] {
    color: var(--color-accent);
    background: var(--color-accent-soft);
  }

  .status-chip[data-kind='neutral'] {
    color: var(--color-text-muted);
    background: var(--color-surface-raised);
  }
</style>
