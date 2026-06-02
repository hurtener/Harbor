<script lang="ts">
  // Harbor Console — Background Jobs per-job Progress tab (Phase 108j /
  // D-182). The Progress sub-panel of the right-rail detail: the planner's
  // numeric `task.progress` bar + the derived ETA estimate + the run's
  // state-transition timeline. Every datum is honest:
  //   - the progress bar reads the planner's [0,1] hint; absent → an
  //     indeterminate "—" (never a fabricated 0%);
  //   - the ETA is the planner's own progress projected over elapsed wall
  //     time, labelled an estimate; no hint → "Unknown" (deriveETA);
  //   - the timeline is the run's REAL `task.*` lifecycle events from the
  //     run-scoped SSE; the SSE is live-only (no backlog on a fresh
  //     connect), so a run that finished before the rail opened shows an
  //     honest empty timeline.
  // Page-specific; Svelte 5 runes (D-092); design tokens only.
  import { deriveETA, projectStateTimeline } from '$lib/background-jobs/derive.js';
  import { formatRelative } from '$lib/sessions/format.js';
  import type { TaskRunStream } from '$lib/tasks/run-stream.svelte.js';
  import type { TaskRow } from '$lib/protocol/tasks.js';

  let {
    row,
    stream
  }: {
    /** The selected job row (its `progress` hint + `started_at`). */
    row: TaskRow | null;
    /** The run-scoped event stream — its lifecycle events feed the timeline. */
    stream: TaskRunStream | null;
  } = $props();

  const progressPct = $derived(
    row?.progress === undefined ? null : Math.round(Math.max(0, Math.min(1, row.progress)) * 100)
  );
  // ETA is recomputed each render against the wall clock — the planner's
  // own progress projected forward, never a fabricated value.
  const eta = $derived(
    row === null ? { known: false, label: 'Unknown', remainingMs: null } : deriveETA(row, Date.now())
  );
  const timeline = $derived(stream === null ? [] : projectStateTimeline(stream.runEvents));
  const streamState = $derived(stream?.streamState ?? 'idle');
</script>

<div class="progress-tab" data-testid="bg-rail-progress">
  <dl class="kv">
    <div class="progress-row">
      <dt>Progress</dt>
      <dd>
        {#if progressPct === null}
          <span class="muted" title="No planner progress hint emitted">— indeterminate</span>
        {:else}
          <span class="bar" aria-label={`${progressPct}% complete`}>
            <span class="fill" style:width={`${progressPct}%`}></span>
          </span>
          <span class="pct">{progressPct}%</span>
        {/if}
      </dd>
    </div>
    <div>
      <dt>ETA</dt>
      <dd data-testid="bg-rail-eta">
        {eta.label}{#if eta.known && eta.label !== 'Done'}<span class="est"> (est.)</span>{/if}
      </dd>
    </div>
  </dl>

  <h4 class="timeline-head">
    State transitions
    <span class="stream-state" data-state={streamState}>
      {streamState === 'open' ? '● live' : streamState === 'connecting' ? '○ connecting' : streamState}
    </span>
  </h4>
  {#if timeline.length === 0}
    <p class="muted" data-testid="bg-rail-timeline-empty">
      No state transitions on the live stream. The run-scoped SSE streams
      events going forward with no backlog — a job that transitioned before
      this rail opened shows none here; generate activity while it is open.
    </p>
  {:else}
    <ol class="timeline" data-testid="bg-rail-timeline">
      {#each timeline as step (step.at + step.status)}
        <li class="step">
          <span class="dot" aria-hidden="true"></span>
          <span class="step-status">{step.status}</span>
          <span class="step-at">{formatRelative(step.at)}</span>
        </li>
      {/each}
    </ol>
  {/if}
</div>

<style>
  .progress-tab {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .kv {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin: var(--space-0);
  }

  .kv > div {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .kv dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .kv dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
    text-align: right;
  }

  .progress-row dd {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex: 1;
    justify-content: flex-end;
  }

  .bar {
    flex: 1;
    max-width: var(--space-12);
    height: var(--space-1);
    background: var(--color-surface-raised);
    border-radius: var(--radius-pill);
    overflow: hidden;
  }

  .fill {
    display: block;
    height: 100%;
    background: var(--color-accent);
  }

  .pct {
    font-size: var(--text-xs);
  }

  .est {
    color: var(--color-text-muted);
    font-weight: 400;
  }

  .timeline-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin: var(--space-0);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .stream-state {
    font-size: var(--text-xs);
    font-weight: 400;
    text-transform: none;
    letter-spacing: normal;
    color: var(--color-text-muted);
  }

  .timeline {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin: var(--space-0);
    padding: var(--space-0);
    list-style: none;
  }

  .step {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .dot {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: var(--radius-pill);
    background: var(--color-accent);
    flex-shrink: 0;
  }

  .step-status {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .step-at {
    margin-left: auto;
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .muted {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
