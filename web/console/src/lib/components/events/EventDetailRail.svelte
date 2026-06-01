<script lang="ts">
  // Events page — Event Details card (Phase 73g / D-125; Phase 108h rework
  // to the mock's single packed card, page-events.md §12). ONE carded
  // panel (not a stack of RailCards) so the detail packs into the viewport
  // without page-level scroll — it scrolls internally instead. Sections in
  // mockup order: header (severity + type + seq) / Identity / Source /
  // Payload (json, Copy JSON) / Quick Actions. A heavy payload renders the
  // `Open artifact` link (D-026 — never inline bytes). Page-specific
  // component: `components/events/` per CONVENTIONS.md §3. Svelte 5 runes
  // (D-092); tokens only.
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import TruncatedPayloadLink from './TruncatedPayloadLink.svelte';
  import { categoryKind, categoryOf } from '$lib/events/taxonomy.js';
  import { isEventArtifactRef } from '$lib/protocol/events.js';
  import type { Event } from '$lib/protocol/events.js';
  import type { ArtifactsNamespace } from '$lib/protocol/client.js';

  let {
    event,
    artifacts,
    onpin,
    onclose
  }: {
    /** The selected event, or null when no row is selected. */
    event: Event | null;
    /** The `artifacts.*` namespace for resolving heavy payloads. */
    artifacts: ArtifactsNamespace;
    /** Pins a facet chip — Quick Actions (`Filter by …`). */
    onpin?: (axis: 'type' | 'tenant' | 'user' | 'session' | 'run', value: string) => void;
    /** Closes the detail (clears the selection) — the header ✕. */
    onclose?: () => void;
  } = $props();

  /** The severity treatment — an ERROR pill for danger-category events. */
  const severity = $derived.by(() => {
    const kind = event === null ? 'neutral' : categoryKind(categoryOf(event.type));
    const explicit = event?.extra?.severity;
    if (explicit) return { label: explicit.toUpperCase(), kind };
    return { label: kind === 'danger' ? 'ERROR' : 'INFO', kind };
  });

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

<section class="panel card detail-card" data-testid="event-detail">
  {#if event}
    {@const idFields = [
      { k: 'tenant_id', v: event.tenant },
      { k: 'user_id', v: event.user },
      { k: 'session_id', v: event.session },
      { k: 'run_id', v: event.run },
      { k: 'task_id', v: event.extra?.task_id }
    ]}
    <header class="detail-head">
      <div class="head-main">
        <div class="head-line">
          <span class="sev" data-kind={severity.kind} data-testid="event-severity">{severity.label}</span>
          <p class="event-name" data-testid="rail-event-name">{event.type}</p>
        </div>
        <div class="head-meta">
          <StatusChip kind={categoryKind(categoryOf(event.type))} label={categoryOf(event.type)} />
          <button type="button" class="copy-btn" data-testid="copy-event-id" onclick={() => void copy(String(event.sequence))}>
            seq #{event.sequence} ⧉
          </button>
          <span class="occurred mono">{event.occurred_at}</span>
        </div>
      </div>
      {#if onclose}
        <button type="button" class="close" data-testid="event-detail-close" aria-label="Close detail" onclick={onclose}>✕</button>
      {/if}
    </header>

    <section class="grp">
      <h4 class="grp-title">Identity</h4>
      <dl class="kv">
        {#each idFields as field (field.k)}
          {#if field.v}
            <dt>{field.k}</dt>
            <dd class="mono">
              <button type="button" class="copy-btn" data-testid={`copy-${field.k}`} onclick={() => void copy(field.v ?? '')}>
                {field.v} ⧉
              </button>
            </dd>
          {/if}
        {/each}
      </dl>
    </section>

    <section class="grp">
      <h4 class="grp-title">Source</h4>
      <dl class="kv">
        <dt>Subsystem</dt>
        <dd class="mono">{event.extra?.source ?? categoryOf(event.type)}</dd>
        {#if event.extra?.transport}
          <dt>Transport</dt>
          <dd class="mono">{event.extra.transport}</dd>
        {/if}
        <dt>Severity</dt>
        <dd>{event.extra?.severity ?? 'info'}</dd>
      </dl>
    </section>

    <section class="grp">
      <div class="grp-head">
        <h4 class="grp-title">Payload (json)</h4>
        {#if event.payload !== undefined && !isEventArtifactRef(event.payload)}
          <button type="button" class="copy-json" data-testid="copy-payload" onclick={() => void copy(prettyPayload(event.payload))}>
            Copy JSON
          </button>
        {/if}
      </div>
      {#if event.payload === undefined}
        <p class="hint">No payload.</p>
      {:else if isEventArtifactRef(event.payload)}
        <TruncatedPayloadLink
          payloadRef={event.payload}
          {artifacts}
          scope={{ tenant: event.tenant, user: event.user, session: event.session }}
        />
      {:else}
        <pre class="payload-json" data-testid="payload-json">{prettyPayload(event.payload)}</pre>
      {/if}
    </section>

    <section class="grp">
      <h4 class="grp-title">Quick Actions</h4>
      <div class="quick-actions">
        <button type="button" class="quick-action" data-testid="qa-filter-type" onclick={() => onpin?.('type', event.type)}>
          Filter by event type
        </button>
        <button type="button" class="quick-action" data-testid="qa-filter-session" onclick={() => onpin?.('session', event.session)}>
          Filter by session
        </button>
        <button type="button" class="quick-action" data-testid="qa-filter-tenant" onclick={() => onpin?.('tenant', event.tenant)}>
          Filter by tenant
        </button>
        {#if event.run}
          <button type="button" class="quick-action" data-testid="qa-filter-run" onclick={() => onpin?.('run', event.run ?? '')}>
            Filter by run
          </button>
        {/if}
        <a class="quick-action" data-testid="qa-open-session" href={`/sessions/${event.session}`}>Open session</a>
        <button type="button" class="quick-action" data-testid="qa-open-trace" disabled title="OTel trace deep-link is post-V1 (page-events.md §10)">
          Open trace
        </button>
      </div>
    </section>
  {:else}
    <p class="hint" data-testid="rail-empty-hint">No event selected — pick an event row to see its detail.</p>
  {/if}
</section>

<style>
  /* One packed card that fills the right column and scrolls INTERNALLY
     (never page-level) — Phase 108h fixes the multi-card page-scroll. */
  .detail-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    min-height: 0;
    overflow-y: auto;
  }

  .detail-head {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .head-main {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-width: 0;
  }

  .head-line {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }

  .sev {
    flex-shrink: 0;
    font-size: var(--text-xs);
    font-weight: 600;
    letter-spacing: var(--tracking-wide);
    padding: var(--space-0) var(--space-1);
    border-radius: var(--radius-sm);
    color: var(--color-text-muted);
  }

  .sev[data-kind='danger'] {
    color: var(--color-danger);
    background: var(--color-danger-soft);
  }

  .sev[data-kind='warning'] {
    color: var(--color-warning);
    background: var(--color-warning-soft);
  }

  .event-name {
    margin: var(--space-0);
    font-family: var(--font-mono);
    font-weight: 600;
    color: var(--color-text);
    word-break: break-all;
  }

  .head-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .occurred {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .close {
    flex-shrink: 0;
    background: none;
    border: none;
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .close:hover {
    color: var(--color-text);
  }

  .grp {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .grp-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }

  .grp-title {
    margin: var(--space-0);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .kv {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
  }

  .kv dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .kv dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    min-width: 0;
  }

  .kv dd.mono {
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

  .copy-json {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
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
    background: var(--color-bg);
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

  .hint {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
