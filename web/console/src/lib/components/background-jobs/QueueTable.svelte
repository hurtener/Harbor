<script lang="ts">
  // Harbor Console — Background Jobs page queue table (Phase 73h /
  // D-128).
  //
  // A typed wrapper over the shared `ui/DataTable` primitive — it does
  // NOT fork a table (CONVENTIONS.md §3). It renders the queue in the
  // page-spec §12 column order: checkbox / Job ID / Title / Parent
  // session / Status / Progress mini-bar / Started / Last activity /
  // Tags / orphan badge. Page-specific — lives in
  // `components/background-jobs/`. Svelte 5 runes (D-092); design
  // tokens only.
  import DataTable, { type DataTableColumn } from '$lib/components/ui/DataTable.svelte';
  import StatusChip, { type StatusKind } from '$lib/components/ui/StatusChip.svelte';
  import OrphanBadge from './OrphanBadge.svelte';
  import type { TaskRow } from '$lib/protocol/tasks.js';

  let {
    rows,
    selected,
    activeId,
    orphans,
    onselect,
    onselectionchange
  }: {
    /** The background-job rows for the current page. */
    rows: TaskRow[];
    /** The set of selected job IDs. */
    selected: Set<string>;
    /** The currently-open job ID (drives the right-rail). */
    activeId: string | null;
    /** The orphan-flagged job IDs from the Console-side detector. */
    orphans: ReadonlySet<string>;
    /** Emitted when a row is clicked (opens the right-rail detail). */
    onselect: (id: string) => void;
    /** Emitted when the row selection changes. */
    onselectionchange: (next: Set<string>) => void;
  } = $props();

  // Column config — the page-spec §12 mockup order. The checkbox column
  // is the DataTable's built-in selection model; the row-action menu is
  // folded into the bulk toolbar + right-click, so it is not a column.
  const COLUMNS: DataTableColumn[] = [
    { key: 'id', label: 'Job ID' },
    { key: 'title', label: 'Title' },
    { key: 'parent_session', label: 'Parent session' },
    { key: 'status', label: 'Status' },
    { key: 'progress', label: 'Progress' },
    { key: 'started', label: 'Started' },
    { key: 'last_activity', label: 'Last activity' },
    { key: 'tags', label: 'Tags' }
  ];

  const STATUS_KINDS: Record<string, StatusKind> = {
    pending: 'neutral',
    running: 'accent',
    paused: 'warning',
    complete: 'success',
    failed: 'danger',
    cancelled: 'neutral'
  };

  function rowKey(r: unknown): string {
    return (r as TaskRow).id;
  }

  // A relative-time label. The detector + table are pure per-render, so
  // the label is recomputed each render against `Date.now()`.
  function relativeLabel(iso: string): string {
    const then = Date.parse(iso);
    if (Number.isNaN(then)) return iso;
    const deltaSec = Math.round((Date.now() - then) / 1000);
    if (deltaSec < 60) return `${deltaSec}s ago`;
    if (deltaSec < 3600) return `${Math.round(deltaSec / 60)}m ago`;
    if (deltaSec < 86_400) return `${Math.round(deltaSec / 3600)}h ago`;
    return `${Math.round(deltaSec / 86_400)}d ago`;
  }

  // The Progress mini-bar percentage; `null` when the planner emitted
  // no hint (the bar renders indeterminate).
  function progressPct(p: number | undefined): number | null {
    if (p === undefined) return null;
    const clamped = Math.max(0, Math.min(1, p));
    return Math.round(clamped * 100);
  }
</script>

<DataTable
  columns={COLUMNS}
  rows={rows}
  {rowKey}
  selectable
  {selected}
  onselectionchange={(s) => onselectionchange(s)}
  onrowclick={(r) => onselect((r as TaskRow).id)}
>
  {#snippet row(r)}
    {@const job = r as TaskRow}
    {@const pct = progressPct(job.progress)}
    <td
      class="cell-id"
      class:row-active={job.id === activeId}
      data-testid="bg-job-row"
      data-job-id={job.id}
    >
      <code>{job.id}</code>
    </td>
    <td class="cell-title" data-testid="bg-job-title">
      {job.description || job.query || '—'}
    </td>
    <td>
      <a
        class="session-link"
        href={`/sessions/${job.parent_session_id}`}
        data-testid="bg-job-parent-session"
        onclick={(e) => e.stopPropagation()}
      >
        {job.parent_session_id}
      </a>
    </td>
    <td>
      <StatusChip kind={STATUS_KINDS[job.status] ?? 'neutral'} label={job.status} />
    </td>
    <td data-testid="bg-job-progress">
      {#if pct === null}
        <span class="progress-indeterminate" title="No planner progress hint">—</span>
      {:else}
        <span class="progress-track" aria-label={`${pct}% complete`}>
          <span class="progress-fill" style:width={`${pct}%`}></span>
        </span>
        <span class="progress-label">{pct}%</span>
      {/if}
    </td>
    <td>{relativeLabel(job.started_at)}</td>
    <td>{relativeLabel(job.last_activity_at)}</td>
    <td data-testid="bg-job-tags">
      <span class="tag-row">
        {#each job.tags ?? [] as tag (tag)}
          <span class="tag">{tag}</span>
        {/each}
        <OrphanBadge row={job} orphan={orphans.has(job.id)} />
      </span>
    </td>
  {/snippet}
</DataTable>

<style>
  .cell-id code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
  }

  .row-active {
    color: var(--color-accent);
    font-weight: 600;
  }

  .cell-title {
    max-width: var(--size-card-min);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .session-link {
    color: var(--color-accent);
    font-size: var(--text-sm);
    text-decoration: none;
  }

  .progress-track {
    display: inline-block;
    width: var(--size-progress-track);
    height: var(--space-2);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    overflow: hidden;
    vertical-align: middle;
  }

  .progress-fill {
    display: block;
    height: 100%;
    background: var(--color-accent);
  }

  .progress-label {
    margin-left: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .progress-indeterminate {
    color: var(--color-text-muted);
  }

  .tag-row {
    display: inline-flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    align-items: center;
  }

  .tag {
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }
</style>
