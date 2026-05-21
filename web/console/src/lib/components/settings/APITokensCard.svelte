<script lang="ts">
  // Settings — API Tokens card (Phase 73m / D-129).
  //
  // The operator's Console-local Personal Access Tokens (72h's
  // `pat_store` table — the token blob is opaque AES-GCM ciphertext
  // 72h owns; this card renders metadata only). PATs are one-time-
  // reveal at creation: the Console NEVER displays a raw token after
  // the create flow closes (acceptance criterion). The persisted form
  // is 72h's encrypted blob.
  import type { PATEntry } from '$lib/db/index.js';

  let { pats }: { pats: PATEntry[] } = $props();
</script>

<div class="card-body" data-testid="settings-api-tokens">
  {#if pats.length === 0}
    <p class="note">
      No Console-local Personal Access Tokens. A PAT is shown once at creation
      and stored encrypted at rest — the raw token is never displayed again.
    </p>
  {:else}
    <ul class="pat-list">
      {#each pats as pat (pat.id)}
        <li class="pat-row" data-testid="pat-row">
          <span class="pat-name">{pat.name}</span>
          <span class="pat-scope">{pat.scope_summary || 'scope unknown'}</span>
          <span class="pat-used">
            {pat.last_used_at
              ? `last used ${new Date(pat.last_used_at).toLocaleDateString()}`
              : 'never used'}
          </span>
        </li>
      {/each}
    </ul>
  {/if}
  <p class="note">
    PATs are encrypted at rest via the Console&rsquo;s WebCrypto helpers; the
    raw token is revealed exactly once at creation.
  </p>
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }
  .pat-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .pat-row {
    display: flex;
    gap: var(--space-3);
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }
  .pat-name {
    color: var(--color-text);
  }
  .pat-scope,
  .pat-used {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
  .note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
