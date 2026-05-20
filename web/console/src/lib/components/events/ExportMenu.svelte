<script lang="ts">
  // Events page — Export ▾ menu (Phase 73g / D-125).
  //
  // The Export control writes the currently-filtered, already-loaded
  // page of events to an NDJSON (default) or CSV file. This is a
  // Console-LOCAL action (D-061): it serialises events the page already
  // received; it never round-trips to the Protocol and never mutates a
  // runtime entity. Heavy payloads are emitted by reference, never
  // inlined (D-026 — the `export.ts` serialisers preserve `artifact_ref`).
  // Svelte 5 runes (D-092); tokens only.
  import {
    exportEventsCSV,
    exportEventsNDJSON,
    exportMeta,
    triggerDownload,
    type ExportFormat
  } from '$lib/events/export.js';
  import type { Event } from '$lib/protocol/harbor.js';

  let {
    events
  }: {
    /** The currently-filtered page of events to serialise. */
    events: Event[];
  } = $props();

  /** Whether the format dropdown is open. */
  let open = $state(false);
  /** A transient progress line shown in the bottom dock. */
  let progress = $state('');

  function doExport(format: ExportFormat): void {
    open = false;
    progress = `Exporting ${events.length} events as ${format.toUpperCase()}…`;
    const content = format === 'csv' ? exportEventsCSV(events) : exportEventsNDJSON(events);
    const { mime, ext } = exportMeta(format);
    const stamp = new Date().toISOString().replace(/[:.]/g, '-');
    triggerDownload(`harbor-events-${stamp}.${ext}`, mime, content);
    progress = '';
  }
</script>

<div class="export-menu" data-testid="export-menu">
  <button
    type="button"
    class="export-trigger"
    data-testid="export-trigger"
    aria-expanded={open}
    onclick={() => (open = !open)}
  >
    Export ▾
  </button>
  {#if open}
    <div class="export-options" role="menu">
      <button
        type="button"
        class="export-option"
        data-testid="export-ndjson"
        role="menuitem"
        onclick={() => doExport('ndjson')}
      >
        NDJSON
      </button>
      <button
        type="button"
        class="export-option"
        data-testid="export-csv"
        role="menuitem"
        onclick={() => doExport('csv')}
      >
        CSV
      </button>
    </div>
  {/if}
  {#if progress !== ''}
    <span class="export-progress" data-testid="export-progress">{progress}</span>
  {/if}
</div>

<style>
  .export-menu {
    position: relative;
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }

  .export-trigger {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .export-trigger:hover {
    border-color: var(--color-accent);
  }

  .export-options {
    position: absolute;
    top: 100%;
    right: var(--space-0);
    z-index: 1;
    margin-top: var(--space-1);
    display: flex;
    flex-direction: column;
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    min-width: var(--size-progress-track);
  }

  .export-option {
    background: transparent;
    color: var(--color-text);
    border: none;
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    text-align: left;
    cursor: pointer;
  }

  .export-option:hover {
    background: var(--color-accent-soft);
  }

  .export-progress {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
