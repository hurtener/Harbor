<script lang="ts">
  // Selected-item detail — the right-rail card that renders one memory
  // record's full detail from `memory.get` (Phase 73j / D-118).
  //
  // The value viewer renders the post-redaction JSON value. When the
  // detail carries a `value_artifact` (the value crossed the D-026
  // heavy-content threshold), the viewer renders a `Truncated` badge +
  // an `Open artifact` link instead of inline bytes — the link invokes
  // the already-shipped `artifacts.get` surface. Svelte 5 runes (D-092).
  import type { MemoryItemDetail } from '$lib/protocol-memory';

  let {
    detail,
    loading,
    error,
    onOpenArtifact,
    onInspectEvents
  }: {
    detail: MemoryItemDetail | null;
    loading: boolean;
    error: string | null;
    onOpenArtifact?: (artifactID: string) => void;
    onInspectEvents?: () => void;
  } = $props();

  /** Pretty-prints the post-redaction value for the JSON viewer. */
  const prettyValue = $derived.by(() => {
    if (!detail?.value) return '';
    try {
      return JSON.stringify(JSON.parse(detail.value), null, 2);
    } catch {
      return detail.value;
    }
  });
</script>

<section class="card" aria-label="Selected item detail">
  <h2>Selected item</h2>
  {#if loading}
    <p class="muted">Loading…</p>
  {:else if error}
    <p class="error" role="alert">{error}</p>
  {:else if !detail}
    <p class="muted">Select a memory row to inspect its detail.</p>
  {:else}
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

    <h3>Value</h3>
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
      <pre class="value-viewer">{prettyValue}</pre>
    {/if}

    {#if onInspectEvents}
      <button type="button" class="link" onclick={onInspectEvents}>
        Inspect related events
      </button>
    {/if}
  {/if}
</section>

<style>
  .card {
    background: var(--color-surface);
    border: var(--border-width-hairline) solid var(--color-border);
    border-radius: var(--radius-md);
    padding: var(--space-4);
  }

  h2 {
    font-size: var(--text-base);
    margin: var(--space-0) var(--space-0) var(--space-3);
  }

  h3 {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    margin: var(--space-4) var(--space-0) var(--space-2);
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
    border: var(--border-width-hairline) solid var(--color-border);
    border-radius: var(--radius-sm);
    padding: var(--space-3);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    overflow: auto;
    max-height: var(--size-value-viewer-max);
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
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .error {
    color: var(--color-danger);
    font-size: var(--text-sm);
  }
</style>
