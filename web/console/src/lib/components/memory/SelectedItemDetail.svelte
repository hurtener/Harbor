<script lang="ts">
  // Selected-item detail body — the right-rail content rendered inside a
  // shared `RailCard` (D-121, CONVENTIONS.md §3). It renders one memory
  // record's full detail from `memory.get`. The page wraps this in a
  // nested `<PageState>` (CONVENTIONS.md §4 — a rail gets its own async
  // boundary) inside `<RailCard title="Selected item">`; this component
  // owns only the loaded-detail body.
  //
  // The value viewer renders the post-redaction value. The Runtime marshals
  // the record value as a Go `[]byte`, which `encoding/json` emits as BASE64 —
  // so `decodeMemoryValue` (Phase 108n / D-186) base64-decodes to UTF-8, THEN
  // pretty-prints the JSON. The pre-chrome viewer `JSON.parse`d the raw base64
  // and rendered gibberish; this fixes the §3 wire-shape bug. When the detail
  // carries a `value_artifact` (the value crossed the D-026 heavy-content
  // threshold), the viewer renders a `Truncated` badge + an `Open artifact`
  // link instead of inline bytes — the link invokes the already-shipped
  // `artifacts.get` surface. Svelte 5 runes (D-092).
  import type { MemoryItemDetail } from '$lib/protocol/memory-types';
  import { decodeMemoryValue } from '$lib/memory/derive.js';

  let {
    detail,
    canEvict = false,
    onOpenArtifact,
    onInspectEvents,
    onEvict
  }: {
    detail: MemoryItemDetail;
    /** True when the connection carries the admin claim (D-079). */
    canEvict?: boolean;
    onOpenArtifact?: (artifactID: string) => void;
    onInspectEvents?: () => void;
    /** Evicts this record via the REAL admin memory.delete (Phase 108n). */
    onEvict?: (key: string) => void;
  } = $props();

  const evictGate = 'Requires the admin scope claim — memory.delete is an admin Protocol method (D-079).';

  /** Decodes (base64 → UTF-8 → pretty JSON) the value for the viewer. */
  const decoded = $derived(decodeMemoryValue(detail.value));

  async function copyValue(): Promise<void> {
    try {
      await navigator.clipboard.writeText(decoded.text);
    } catch {
      // clipboard denied (insecure context / no permission) — non-fatal
    }
  }
</script>

<div class="detail-body" aria-label="Selected item detail">
  <dl class="meta">
    <div><dt>Key</dt><dd class="mono">{detail.item.key}</dd></div>
    <div>
      <dt>Identity</dt>
      <dd class="mono"
        >{detail.item.identity.tenant} / {detail.item.identity.user} / {detail
          .item.identity.session}</dd
      >
    </div>
    <div><dt>Strategy</dt><dd>{detail.item.strategy}</dd></div>
    <div><dt>Scope</dt><dd>{detail.item.scope}</dd></div>
    <div><dt>Driver</dt><dd class="mono">{detail.item.driver}</dd></div>
    <div><dt>Size</dt><dd>{detail.item.size_bytes} bytes</dd></div>
  </dl>

  <div class="value-head">
    <h4>Value</h4>
    {#if !detail.value_artifact && detail.value}
      <button
        type="button"
        class="link"
        data-testid="memory-detail-copy-value"
        onclick={() => void copyValue()}
      >
        Copy value
      </button>
    {/if}
  </div>
  {#if detail.value_artifact}
    <!-- D-026: heavy value — NOT inlined. The Truncated badge + the
         Open-artifact link route through the shipped artifacts.get. -->
    <div class="heavy-value">
      <span class="badge">Truncated</span>
      <p class="muted">
        Value exceeds the heavy-content threshold and is stored by
        reference.
      </p>
      {#if onOpenArtifact}
        <button
          type="button"
          class="link"
          onclick={() => onOpenArtifact?.(detail.value_artifact?.id ?? '')}
        >
          Open artifact
        </button>
      {/if}
    </div>
  {:else}
    <pre class="value-viewer" data-testid="memory-detail-value">{decoded.text}</pre>
  {/if}

  <div class="detail-actions">
    {#if onInspectEvents}
      <button type="button" class="link" onclick={onInspectEvents}>
        Inspect related events
      </button>
    {/if}
    {#if onEvict}
      <button
        type="button"
        class="evict"
        data-testid="memory-detail-evict"
        disabled={!canEvict}
        title={canEvict ? 'Evict this memory turn (audited)' : evictGate}
        onclick={() => onEvict?.(detail.item.key)}
      >
        Evict turn
      </button>
    {/if}
  </div>
</div>

<style>
  .detail-body {
    display: grid;
    gap: var(--space-2);
  }

  h4 {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    margin: var(--space-2) var(--space-0) var(--space-1);
  }

  .value-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .meta {
    display: grid;
    gap: var(--space-2);
    margin: var(--space-0);
  }

  .meta div {
    display: flex;
    justify-content: space-between;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }

  dt {
    color: var(--color-text-muted);
  }

  dd {
    margin: var(--space-0);
    text-align: right;
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    overflow-wrap: anywhere;
  }

  .value-viewer {
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-3);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    overflow: auto;
    max-height: var(--size-value-viewer-max);
    margin: var(--space-0);
  }

  .heavy-value {
    display: grid;
    gap: var(--space-2);
  }

  .badge {
    display: inline-block;
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    background: var(--color-warning);
    color: var(--color-bg);
    width: fit-content;
  }

  .link {
    background: transparent;
    border: none;
    color: var(--color-accent);
    font-size: var(--text-sm);
    cursor: pointer;
    padding: var(--space-0);
    text-align: left;
    justify-self: start;
  }

  .detail-actions {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .evict {
    background: var(--color-surface-raised);
    color: var(--color-danger);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .evict:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
