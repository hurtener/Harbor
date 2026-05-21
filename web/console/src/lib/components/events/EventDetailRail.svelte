<script lang="ts">
  // Events page — right-rail Event Details card (Phase 73g / D-125).
  //
  // The sticky, full-height Event Details card shown when a table row is
  // selected. Sub-sections in page-events.md §12 mockup order: Source /
  // Identity / Payload (json) / Quick Actions. Each identity component is
  // copyable. A heavy payload renders the `Truncated` badge + `Open
  // artifact` link (D-026 — never inline bytes). Composes the shared
  // `RailCard`. Page-specific component: `components/events/` per
  // CONVENTIONS.md §3. Svelte 5 runes (D-092); tokens only.
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import TruncatedPayloadLink from './TruncatedPayloadLink.svelte';
  import { categoryKind, categoryOf } from '$lib/events/taxonomy.js';
  import { isEventArtifactRef } from '$lib/protocol/events.js';
  import type { Event } from '$lib/protocol/events.js';
  import type { ArtifactsNamespace } from '$lib/protocol/client.js';

  let {
    event,
    artifacts,
    onpin
  }: {
    /** The selected event, or null when no row is selected. */
    event: Event | null;
    /** The `artifacts.*` namespace for resolving heavy payloads. */
    artifacts: ArtifactsNamespace;
    /** Pins a facet chip — Quick Actions (`Filter by …`). */
    onpin?: (axis: 'type' | 'tenant' | 'user' | 'session' | 'run', value: string) => void;
  } = $props();

  /** Copies a string to the clipboard (no-op when unavailable). */
  async function copy(value: string): Promise<void> {
    if (typeof navigator !== 'undefined' && navigator.clipboard) {
      await navigator.clipboard.writeText(value);
    }
  }

  /** Pretty-prints the typed payload — only for NON-heavy payloads. */
  function prettyPayload(payload: unknown): string {
    return JSON.stringify(payload, null, 2);
  }
</script>

{#if event}
  <RailCard title="Event">
    <p class="event-name" data-testid="rail-event-name">{event.type}</p>
    <StatusChip kind={categoryKind(categoryOf(event.type))} label={categoryOf(event.type)} />
    <button
      type="button"
      class="copy-btn"
      data-testid="copy-event-id"
      onclick={() => void copy(String(event.sequence))}
    >
      seq #{event.sequence} ⧉
    </button>
  </RailCard>

  <RailCard title="Source">
    <dl class="rail-grid">
      <dt>Subsystem</dt>
      <dd class="mono">{event.extra?.source ?? categoryOf(event.type)}</dd>
      <dt>Severity</dt>
      <dd>{event.extra?.severity ?? 'info'}</dd>
      <dt>Occurred</dt>
      <dd class="mono">{event.occurred_at}</dd>
    </dl>
  </RailCard>

  <RailCard title="Identity">
    <dl class="rail-grid">
      {#each [{ k: 'tenant_id', v: event.tenant }, { k: 'user_id', v: event.user }, { k: 'session_id', v: event.session }, { k: 'run_id', v: event.run }, { k: 'task_id', v: event.extra?.task_id }] as field (field.k)}
        {#if field.v}
          <dt>{field.k}</dt>
          <dd class="mono copyable">
            <button
              type="button"
              class="copy-btn"
              data-testid={`copy-${field.k}`}
              onclick={() => void copy(field.v ?? '')}
            >
              {field.v} ⧉
            </button>
          </dd>
        {/if}
      {/each}
    </dl>
  </RailCard>

  <RailCard title="Payload (json)">
    {#if event.payload === undefined}
      <p class="rail-hint">No payload.</p>
    {:else if isEventArtifactRef(event.payload)}
      <!-- Heavy payload: a reference, NEVER inline bytes (D-026). -->
      <TruncatedPayloadLink
        payloadRef={event.payload}
        {artifacts}
        scope={{ tenant: event.tenant, user: event.user, session: event.session }}
      />
    {:else}
      <pre class="payload-json" data-testid="payload-json">{prettyPayload(event.payload)}</pre>
    {/if}
  </RailCard>

  <RailCard title="Quick Actions">
    <div class="quick-actions">
      <button
        type="button"
        class="quick-action"
        data-testid="qa-filter-type"
        onclick={() => onpin?.('type', event.type)}
      >
        Filter by event type
      </button>
      <button
        type="button"
        class="quick-action"
        data-testid="qa-filter-session"
        onclick={() => onpin?.('session', event.session)}
      >
        Filter by session
      </button>
      <button
        type="button"
        class="quick-action"
        data-testid="qa-filter-tenant"
        onclick={() => onpin?.('tenant', event.tenant)}
      >
        Filter by tenant
      </button>
      {#if event.run}
        <button
          type="button"
          class="quick-action"
          data-testid="qa-filter-run"
          onclick={() => onpin?.('run', event.run ?? '')}
        >
          Filter by run
        </button>
      {/if}
      <a class="quick-action" data-testid="qa-open-session" href={`/sessions/${event.session}`}>
        Open session
      </a>
      <button
        type="button"
        class="quick-action"
        data-testid="qa-open-trace"
        disabled
        title="OTel trace deep-link is post-V1 (page-events.md §10)"
      >
        Open trace
      </button>
    </div>
  </RailCard>
{:else}
  <RailCard title="Event detail">
    <p class="rail-hint" data-testid="rail-empty-hint">
      No event selected — pick an event row to see its detail.
    </p>
  </RailCard>
{/if}

<style>
  .event-name {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-family: var(--font-mono);
    font-weight: 600;
    color: var(--color-text);
    word-break: break-all;
  }

  .rail-grid {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
  }

  .rail-grid dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .rail-grid dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  .rail-grid dd.mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    word-break: break-all;
  }

  .copy-btn {
    background: transparent;
    border: none;
    padding: var(--space-0);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-accent);
    cursor: pointer;
  }

  .copy-btn:hover {
    text-decoration: underline;
  }

  .payload-json {
    margin: var(--space-0);
    max-height: var(--size-value-viewer-max);
    overflow: auto;
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
  }

  .quick-actions {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .quick-action {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    text-align: left;
    text-decoration: none;
    cursor: pointer;
  }

  .quick-action:hover:not(:disabled) {
    border-color: var(--color-accent);
  }

  .quick-action:disabled {
    color: var(--color-text-muted);
    cursor: not-allowed;
  }

  .rail-hint {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
