<script lang="ts">
  // Harbor Console — Sessions table Identity column cell (Phase 73c /
  // D-122). Consumes Phase 72b's `IdentityScope` impersonation triplet
  // (`actor` / `requester` / `impersonating`, D-107) per Brief 11 §PG-5.
  //
  // When the row is a normal non-impersonated run, only the actor triple
  // (here the row's own user@tenant) renders. When the row's
  // `IdentityScope.impersonating` is non-empty (an admin-initiated
  // run), the cell renders the verified `actor` triple PLUS a separate
  // `impersonating` chip naming the target identity the run executed
  // under. This is the same-wave consumer that discharges Phase 72b's
  // binding cross-reference (§13 primitive-with-consumer).
  //
  // Sessions-specific component. Svelte 5 runes (D-092); tokens only.
  import type { SessionIdentityScope } from '$lib/sessions/types.js';

  let { identity }: { identity: SessionIdentityScope } = $props();

  /** The verified actor identity — the `actor` triple when impersonating,
   *  otherwise the row's own (tenant, user). */
  const actor = $derived(identity.actor ?? identity);
  /** The impersonated target, present only for admin-initiated runs. */
  const impersonating = $derived(identity.impersonating);
</script>

<div class="identity" data-testid="identity-cell">
  <span class="actor" data-testid="identity-actor">
    {actor.user}<span class="at">@</span>{actor.tenant}
  </span>
  {#if impersonating}
    <span class="impersonating-chip" data-testid="identity-impersonating">
      ↳ impersonating {impersonating.user}@{impersonating.tenant}
    </span>
  {/if}
</div>

<style>
  .identity {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .actor {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .at {
    color: var(--color-text-muted);
  }

  .impersonating-chip {
    font-size: var(--text-xs);
    color: var(--color-bg);
    background: var(--color-warning);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    width: fit-content;
  }
</style>
