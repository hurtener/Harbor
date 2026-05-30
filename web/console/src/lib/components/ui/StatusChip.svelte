<script lang="ts" module>
  // Harbor Console — shared StatusChip (D-121, CONVENTIONS.md §3, Phase 108 / D-167).
  //
  // One status pill. The `kind` prop maps over the chip token scale.
  // Pre-existing kinds (`online` / `offline` / `error`) alias onto the
  // new palette for backward compatibility.

  /** The closed set of status kinds, mapped onto the chip token scale. */
  export type StatusKind = 'success' | 'warning' | 'danger' | 'accent' | 'neutral' | 'info';

  /** Legacy aliases for pre-Phase-108 consumers. */
  type LegacyKind = 'online' | 'offline' | 'error';

  export type ChipKind = StatusKind | LegacyKind;

  function normalizeKind(kind: ChipKind): StatusKind {
    switch (kind) {
      case 'online':
        return 'success';
      case 'offline':
        return 'neutral';
      case 'error':
        return 'danger';
      default:
        return kind;
    }
  }
</script>

<script lang="ts">
  let {
    kind = 'neutral',
    label,
    desaturated = false
  }: {
    /** The status kind — selects the token-driven colour. */
    kind?: ChipKind;
    /** The pill text. */
    label: string;
    /** When true, the chip drops its kind-coloured styling and renders
        in the neutral palette (Phase 83r / N8). Pages set this on
        chips whose state is meaningless when the Console is
        disconnected — the colour without a backing Runtime is noise. */
    desaturated?: boolean;
  } = $props();

  const resolvedKind = $derived(normalizeKind(kind));
</script>

<span
  class="status-chip"
  data-kind={desaturated ? 'neutral' : resolvedKind}
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
    border: var(--border-hairline);
  }

  .status-chip[data-kind='info'] {
    color: var(--chip-info-fg);
    background: var(--chip-info-bg);
    border-color: var(--chip-info-border);
  }

  .status-chip[data-kind='success'] {
    color: var(--chip-success-fg);
    background: var(--chip-success-bg);
    border-color: var(--chip-success-border);
  }

  .status-chip[data-kind='warning'] {
    color: var(--chip-warning-fg);
    background: var(--chip-warning-bg);
    border-color: var(--chip-warning-border);
  }

  .status-chip[data-kind='danger'] {
    color: var(--chip-danger-fg);
    background: var(--chip-danger-bg);
    border-color: var(--chip-danger-border);
  }

  .status-chip[data-kind='accent'] {
    color: var(--chip-accent-fg);
    background: var(--chip-accent-bg);
    border-color: var(--chip-accent-border);
  }

  .status-chip[data-kind='neutral'] {
    color: var(--chip-neutral-fg);
    background: var(--chip-neutral-bg);
    border-color: var(--chip-neutral-border);
  }
</style>
