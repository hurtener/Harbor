<script lang="ts" module>
  // One rendered identity-rejection — the Console projection of a
  // `memory.identity_rejected` event (D-033). The `<missing>`
  // substitution is preserved VERBATIM: the Console NEVER masks the
  // rejection and offers NO "view rejected memory anyway" affordance
  // (§13 forbidden practice).
  export interface IdentityRejection {
    /** The rejected MemoryStore operation name. */
    operation: string;
    /** The static reason naming the missing identity component(s). */
    reason: string;
    /** The event's identity triple — carries the `<missing>` sentinel. */
    tenant: string;
    user: string;
    session: string;
    /** Wall-clock time the rejection was observed. */
    occurredAt: string;
  }
</script>

<script lang="ts">
  // Recent-identity-rejections card body — the right-rail content
  // rendered inside a shared `RailCard` (D-121, CONVENTIONS.md §3). It
  // surfaces `memory.identity_rejected` events (D-033). The page wraps
  // this in `<RailCard title="Recent identity rejections">`; this
  // component owns only the card body. Svelte 5 runes mode (D-092).
  let {
    rejections,
    onInspectEvents
  }: {
    rejections: IdentityRejection[];
    onInspectEvents?: () => void;
  } = $props();
</script>

<div class="rejections-body" aria-label="Recent identity rejections">
  {#if rejections.length === 0}
    <p class="muted">No identity rejections in this scope.</p>
  {:else}
    <ul class="rejections">
      {#each rejections as r, i (i)}
        <li>
          <div class="row">
            <span class="op">{r.operation}</span>
            <time>{r.occurredAt}</time>
          </div>
          <p class="reason">{r.reason}</p>
          <!-- D-033: the identity triple is rendered VERBATIM, including
               the `<missing>` sentinel. No partial-identity substitution
               Console-side; no affordance to view the rejected memory. -->
          <code class="identity">{r.tenant} / {r.user} / {r.session}</code>
        </li>
      {/each}
    </ul>
  {/if}
  {#if onInspectEvents}
    <button type="button" class="link" onclick={onInspectEvents}>
      Inspect events
    </button>
  {/if}
</div>

<style>
  .rejections-body {
    display: grid;
    gap: var(--space-3);
  }

  .rejections {
    list-style: none;
    padding: var(--space-0);
    margin: var(--space-0);
    display: grid;
    gap: var(--space-3);
  }

  .row {
    display: flex;
    justify-content: space-between;
    font-size: var(--text-sm);
  }

  .op {
    font-family: var(--font-mono);
    color: var(--color-warning);
  }

  time {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .reason {
    margin: var(--space-1) var(--space-0);
    font-size: var(--text-sm);
  }

  .identity {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .link {
    background: transparent;
    border: none;
    color: var(--color-accent);
    font-size: var(--text-xs);
    cursor: pointer;
    padding: var(--space-0);
    text-align: left;
    justify-self: start;
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
