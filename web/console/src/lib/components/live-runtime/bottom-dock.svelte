<script lang="ts">
  // Harbor Console — Live Runtime bottom dock (Phase 108d Stage 3 extraction).
  //
  // The page's bottom dock: the `<EventStreamDock>` (left pane) + the
  // `{#if selectedNode}<PerTaskDetailPane>{:else}<RunComposer>{/if}` right
  // pane. Extracted verbatim from `routes/(console)/live-runtime/+page.svelte`
  // — same DOM, same testids, same child components. The page owns the
  // reactive state + the Protocol dispatch; this component renders the panes
  // and forwards the callbacks (ontracetoggle / onverb / onprioritize) back.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import EventStreamDock from '$lib/components/live-runtime/event-stream-dock.svelte';
  import PerTaskDetailPane from '$lib/components/live-runtime/per-task-detail-pane.svelte';
  import RunComposer, {
    type ComposerVerb
  } from '$lib/components/live-runtime/composer/run-composer.svelte';
  import type { Event } from '$lib/protocol/events.js';
  import type { TaskDetail } from '$lib/protocol/tasks.js';
  import type { StreamState } from '$lib/events/subscription.svelte.js';

  let {
    events,
    streamState,
    traceOn,
    traceRunID,
    selectedNode,
    detail,
    detailLoading,
    traceEvents,
    canControl,
    composerPending,
    composerResult,
    disconnected,
    ontracetoggle,
    onverb,
    onprioritize
  }: {
    /** The page window of recent events for the dock's stream pane. */
    events: Event[];
    /** The SSE stream lifecycle state. */
    streamState: StreamState;
    /** Whether the Trace tab is active. */
    traceOn: boolean;
    /** The run-correlation key the Trace tab narrows against. */
    traceRunID: string;
    /** The selected topology node, or null (composer when null). */
    selectedNode: string | null;
    /** The selected node's task detail, or null. */
    detail: TaskDetail | null;
    /** Whether the per-task detail is loading. */
    detailLoading: boolean;
    /** The run-scoped event slice the Trace tab renders. */
    traceEvents: Event[];
    /** Whether the operator holds the elevated control scope. */
    canControl: boolean;
    /** Whether a composer verb is in flight. */
    composerPending: boolean;
    /** The last composer dispatch result, or null. */
    composerResult: { ok: boolean; message: string } | null;
    /** The disconnected predicate — disables the composer. */
    disconnected: boolean;
    /** Trace-toggle callback. */
    ontracetoggle: (next: boolean) => void;
    /** Composer verb-dispatch callback. */
    onverb: (verb: ComposerVerb, text: string) => void;
    /** Per-task prioritize callback. */
    onprioritize: (id: string, priority: number) => void;
  } = $props();
</script>

<div class="bottom-dock">
  <EventStreamDock
    events={events}
    streamState={streamState}
    traceOn={traceOn}
    traceRunID={traceRunID}
    ontracetoggle={ontracetoggle}
  />
  {#if selectedNode !== null}
    <PerTaskDetailPane
      detail={detail}
      loading={detailLoading}
      traceEvents={traceEvents}
      canControl={canControl}
      onprioritize={onprioritize}
    />
  {:else}
    <RunComposer
      canControl={canControl}
      pending={composerPending}
      result={composerResult}
      {disconnected}
      onverb={onverb}
    />
  {/if}
</div>

<style>
  .bottom-dock {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4);
    align-items: start;
  }
</style>
