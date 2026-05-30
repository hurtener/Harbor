<script lang="ts">
  // Harbor Console — Playground KPI metadata strip (Phase 108 / D-167,
  // 108a fidelity pass).
  //
  // One INTEGRATED horizontal band (mock Image 5) — labelled columns
  // separated by hairline dividers, NOT a row of boxed tiles. Columns:
  // Session ID · Started · Duration · Tokens (in/out + sparkline) ·
  // Cost (ceiling bar) · Latency (p50, kept per D-Q1) · Identity · Scope.
  // Every value is real; a metric with no reading shows an em-dash, never
  // a fabricated number (CLAUDE.md §13).
  //
  // Design tokens only.

  import { onDestroy } from 'svelte';

  let {
    sessionID,
    startedAt,
    activeWorkMs,
    activeSinceMs,
    identityUser,
    identityTenant,
    scopeLabel,
    tokenCount,
    promptTokens,
    outputTokens,
    tokenSamples,
    costUSD,
    ceilingUSD,
    hasCostReading,
    turnLatencies
  }: {
    sessionID: string;
    /** ISO timestamp of the session's first turn, or null before any turn. */
    startedAt: string | null;
    /** Summed active-work time across all completed turns (foreground +
     *  background) in this session, in ms — the time the system was
     *  actually doing something (thinking + tool calls), NOT wall-clock. */
    activeWorkMs: number;
    /** Epoch ms a turn started while it is in flight (0 when idle). Lets
     *  Duration tick up live only while the system is actively working. */
    activeSinceMs: number;
    identityUser: string;
    identityTenant: string;
    /** A short scope summary (e.g. "admin"). */
    scopeLabel: string;
    tokenCount: number;
    promptTokens: number;
    outputTokens: number;
    tokenSamples: number[];
    costUSD: number;
    ceilingUSD: number | null;
    hasCostReading: boolean;
    turnLatencies: number[];
  } = $props();

  const SPARK_W = 64;
  const SPARK_H = 18;

  const sparkPath = $derived.by(() => {
    if (tokenSamples.length < 2) return '';
    const max = Math.max(...tokenSamples, 1);
    const min = Math.min(...tokenSamples, 0);
    const range = max - min || 1;
    const step = SPARK_W / (tokenSamples.length - 1);
    return tokenSamples
      .map((v, i) => {
        const x = i * step;
        const y = SPARK_H - ((v - min) / range) * SPARK_H;
        return `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`;
      })
      .join(' ');
  });

  const p50 = $derived.by(() => {
    if (turnLatencies.length < 1) return null;
    const sorted = [...turnLatencies].sort((a, b) => a - b);
    const mid = Math.floor(sorted.length / 2);
    return sorted.length % 2 === 0 ? (sorted[mid - 1] + sorted[mid]) / 2 : sorted[mid];
  });

  const costPercent = $derived.by(() => {
    if (ceilingUSD === null || ceilingUSD <= 0) return null;
    return (costUSD / ceilingUSD) * 100;
  });
  const costWarning = $derived(costPercent !== null && costPercent >= 80);

  function kfmt(n: number): string {
    if (n < 1000) return `${n}`;
    return `${(n / 1000).toFixed(1)}k`;
  }

  // Ticks once a second so Duration counts up live WHILE a turn is in
  // flight; when idle the displayed value is the static active-work sum.
  let nowMs = $state(Date.now());
  const ticker = setInterval(() => (nowMs = Date.now()), 1000);
  onDestroy(() => clearInterval(ticker));

  const startedLabel = $derived.by(() => {
    if (startedAt === null) return '—';
    try {
      return new Intl.DateTimeFormat(undefined, {
        month: 'short',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit'
      }).format(new Date(startedAt));
    } catch {
      return '—';
    }
  });

  // Duration = total active-work time = the summed per-turn durations
  // (foreground + background) plus, while a turn is in flight, the live
  // elapsed of the current turn. NOT wall-clock since the session opened —
  // idle time between turns is excluded.
  const durationLabel = $derived.by(() => {
    const live = activeSinceMs > 0 ? Math.max(0, nowMs - activeSinceMs) : 0;
    const ms = activeWorkMs + live;
    if (ms <= 0 && startedAt === null) return '—';
    const s = Math.floor(ms / 1000);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const sec = s % 60;
    if (h > 0) return `${h}h ${m}m`;
    return `${m}m ${sec.toString().padStart(2, '0')}s`;
  });

  let copied = $state(false);
  async function copySession(): Promise<void> {
    try {
      await navigator.clipboard.writeText(sessionID);
      copied = true;
      setTimeout(() => (copied = false), 1200);
    } catch {
      /* clipboard unavailable */
    }
  }
</script>

<div class="kpi-strip" data-testid="kpi-strip">
  <!-- Session ID -->
  <div class="kpi-col" data-testid="kpi-session">
    <span class="kpi-label">Session ID</span>
    <button
      type="button"
      class="kpi-value mono session-btn"
      title={copied ? 'Copied!' : 'Copy session id'}
      onclick={() => void copySession()}
    >
      {sessionID || '—'}
    </button>
  </div>

  <!-- Started -->
  <div class="kpi-col" data-testid="kpi-started">
    <span class="kpi-label">Started</span>
    <span class="kpi-value tabular">{startedLabel}</span>
  </div>

  <!-- Duration -->
  <div class="kpi-col" data-testid="kpi-duration">
    <span class="kpi-label">Duration</span>
    <span class="kpi-value tabular">{durationLabel}</span>
  </div>

  <!-- Tokens -->
  <div class="kpi-col" data-testid="kpi-tokens">
    <span class="kpi-label">Tokens</span>
    <div class="kpi-value-row">
      <span class="kpi-value tabular">{hasCostReading ? tokenCount.toLocaleString() : '—'}</span>
      {#if tokenSamples.length >= 2}
        <svg class="sparkline" viewBox="0 0 {SPARK_W} {SPARK_H}" width={SPARK_W} height={SPARK_H} aria-hidden="true">
          <path d={sparkPath} fill="none" stroke="var(--color-success)" stroke-width="1.5" />
        </svg>
      {/if}
    </div>
    <span class="kpi-sub tabular">
      {hasCostReading ? `${kfmt(promptTokens)} in / ${kfmt(outputTokens)} out` : 'no turns yet'}
    </span>
  </div>

  <!-- Cost -->
  <div class="kpi-col" data-testid="kpi-cost">
    <span class="kpi-label">Cost</span>
    <span class="kpi-value tabular">{hasCostReading ? `$${costUSD.toFixed(4)}` : '—'}</span>
    {#if costPercent !== null}
      <div class="ceiling">
        <div class="ceiling-track">
          <div class="ceiling-fill" class:warn={costWarning} style:width="{Math.min(costPercent, 100)}%"></div>
        </div>
        <span class="kpi-sub tabular" class:warn-text={costWarning}>
          {Math.round(costPercent)}% of ${ceilingUSD!.toFixed(2)}
        </span>
      </div>
    {:else}
      <span class="kpi-sub">{hasCostReading ? 'no ceiling set' : '—'}</span>
    {/if}
  </div>

  <!-- Latency (kept per D-Q1) -->
  <div class="kpi-col" data-testid="kpi-latency">
    <span class="kpi-label">Latency</span>
    <span class="kpi-value tabular">{p50 !== null ? `${Math.round(p50)} ms` : '—'}</span>
    <span class="kpi-sub">p50 · {turnLatencies.length} turn{turnLatencies.length === 1 ? '' : 's'}</span>
  </div>

  <!-- Identity -->
  <div class="kpi-col" data-testid="kpi-identity">
    <span class="kpi-label">Identity</span>
    <span class="kpi-value">{identityUser || '—'}</span>
  </div>

  <!-- Scope -->
  <div class="kpi-col" data-testid="kpi-scope">
    <span class="kpi-label">Scope</span>
    <span class="kpi-value scope-value">Tenant: {identityTenant || '—'}{scopeLabel ? ` · ${scopeLabel}` : ''}</span>
  </div>
</div>

<style>
  .kpi-strip {
    display: flex;
    align-items: stretch;
    padding: var(--space-3) var(--space-4);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    background: var(--color-surface);
  }

  /* 108a — every column grows to fill the band evenly (mock Image 5),
     instead of packing to the left and wrapping the in/out sub-text. */
  .kpi-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
    flex: 1 1 0;
    padding: 0 var(--space-4);
    border-right: var(--border-hairline);
  }

  .kpi-col:first-child {
    padding-left: 0;
  }

  .kpi-col:last-child {
    border-right: none;
    padding-right: 0;
  }

  .kpi-label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wider);
    color: var(--color-text-muted);
  }

  .kpi-value-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .kpi-value {
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
    line-height: 1.2;
  }

  .session-btn {
    background: none;
    border: none;
    padding: var(--space-0);
    cursor: pointer;
    text-align: left;
    max-width: var(--size-session-max-width);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .session-btn:hover {
    color: var(--color-accent);
  }

  .scope-value {
    font-size: var(--text-sm);
    font-weight: 500;
  }

  .kpi-sub {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .kpi-sub.warn-text {
    color: var(--color-warning);
  }

  .mono {
    font-family: var(--font-mono);
  }

  .tabular {
    font-variant-numeric: var(--font-variant-tabular);
  }

  .sparkline {
    flex-shrink: 0;
    opacity: 0.9;
  }

  .ceiling {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .ceiling-track {
    height: var(--space-1);
    width: 100%;
    min-width: var(--size-chip-min-width);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .ceiling-fill {
    height: 100%;
    background: var(--color-success);
    border-radius: var(--radius-sm);
  }

  .ceiling-fill.warn {
    background: var(--color-warning);
  }
</style>
