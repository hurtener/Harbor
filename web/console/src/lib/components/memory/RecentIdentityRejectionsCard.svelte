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
  // Recent-identity-rejections status card — the right-rail card that
  // surfaces `memory.identity_rejected` events (D-033). Svelte 5 runes.
  let {
    rejections,
    onInspectEvents
  }: {
    rejections: IdentityRejection[];
    onInspectEvents?: () => void;
  } = $props();
</script>

<section class="card" aria-label="Recent identity rejections">
  <header>
    <h2>Recent identity rejections</h2>
    {#if onInspectEvents}
      <button type="button" class="link" onclick={onInspectEvents}>
        Inspect events
      </button>
    {/if}
  </header>
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
          <code class="identity"
            >{r.tenant} / {r.user} / {r.session}</code
          >
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .card {
    background: var(--color-surface);
    border: var(--border-width-hairline) solid var(--color-border);
    border-radius: var(--radius-md);
    padding: var(--space-4);
  }

  header {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    margin-bottom: var(--space-3);
  }

  h2 {
    font-size: var(--text-base);
    margin: var(--space-0);
  }

  .link {
    background: transparent;
    border: none;
    color: var(--color-accent);
    font-size: var(--text-xs);
    cursor: pointer;
    padding: var(--space-0);
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

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
