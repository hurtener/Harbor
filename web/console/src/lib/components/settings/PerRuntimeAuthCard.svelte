<script lang="ts">
  // Settings — Per-Runtime Auth card (Phase 73m / D-129).
  //
  // Per-runtime auth-profile metadata (72h's `auth_profiles` table —
  // the encrypted-at-rest JWT blob's metadata; the blob itself is
  // opaque AES-GCM ciphertext 72h owns, never decrypted here).
  //
  // The `Rotate token` action is the ONE net-new Protocol method this
  // phase ships: `auth.rotate_token` (admin). CONVENTIONS.md §5 — no
  // stubbed action: the button either invokes the REAL method, or
  // renders disabled-with-tooltip when the connection lacks the admin
  // scope claim (D-079). The re-minted token is ONE-TIME-REVEAL: it is
  // shown once and dropped on dismiss; the operator copies it once.
  import type { AuthProfile } from '$lib/db/index.js';
  import type { RotateTokenState } from '$lib/settings/state.svelte.js';

  let {
    authProfiles,
    rotate
  }: {
    authProfiles: AuthProfile[];
    rotate: RotateTokenState;
  } = $props();
</script>

<div class="card-body" data-testid="settings-per-runtime-auth">
  {#if authProfiles.length === 0}
    <p class="note">
      No stored auth profiles. A per-runtime JWT is saved (encrypted at rest)
      when you attach to a runtime.
    </p>
  {:else}
    <ul class="auth-list">
      {#each authProfiles as ap (ap.id)}
        <li class="auth-row" data-testid="auth-profile-row">
          <span class="auth-issuer">{ap.issuer || 'unknown issuer'}</span>
          <span class="auth-alg">{ap.algorithm}</span>
          <span class="auth-exp">
            {ap.expires_at ? `expires ${new Date(ap.expires_at).toLocaleDateString()}` : 'no expiry cached'}
          </span>
        </li>
      {/each}
    </ul>
  {/if}

  <div class="rotate-block">
    <span class="rotate-label">Rotate Protocol token</span>
    {#if rotate.phase === 'revealed' && rotate.revealedToken}
      <div class="reveal" data-testid="rotate-token-reveal">
        <p class="reveal-note">
          Copy this token now — it is shown ONCE and cannot be displayed again.
        </p>
        <code class="token">{rotate.revealedToken}</code>
        <button
          type="button"
          class="row-action"
          data-testid="rotate-token-dismiss"
          onclick={() => rotate.dismiss()}
        >
          I&rsquo;ve copied it — dismiss
        </button>
      </div>
    {:else}
      <button
        type="button"
        class="primary"
        data-testid="rotate-token-btn"
        disabled={!rotate.hasAdminScope || rotate.phase === 'rotating'}
        title={rotate.hasAdminScope
          ? 'Rotate the Protocol-auth token for the connected runtime'
          : 'Rotating the Protocol token requires the admin scope claim'}
        onclick={() => void rotate.rotate()}
      >
        {rotate.phase === 'rotating' ? 'Rotating…' : 'Rotate token'}
      </button>
      {#if !rotate.hasAdminScope}
        <span class="hint" data-testid="rotate-token-disabled-hint">
          Requires the admin scope claim.
        </span>
      {/if}
      {#if rotate.phase === 'error' && rotate.error}
        <p class="form-error" data-testid="rotate-token-error">
          {rotate.error.code}: {rotate.error.message}
        </p>
      {/if}
    {/if}
  </div>
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }
  .auth-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .auth-row {
    display: flex;
    gap: var(--space-3);
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }
  .auth-issuer {
    color: var(--color-text);
  }
  .auth-alg,
  .auth-exp {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
  .rotate-block {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .rotate-label {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  .primary,
  .row-action {
    align-self: flex-start;
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
  }
  .primary {
    background: var(--color-accent);
    border-color: var(--color-accent);
    color: var(--color-bg);
  }
  .primary:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .row-action {
    background: var(--color-surface);
    color: var(--color-text-muted);
  }
  .reveal {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-2);
    border: var(--border-hairline);
    border-color: var(--color-warning);
    border-radius: var(--radius-sm);
    background: var(--color-warning-soft);
  }
  .reveal-note {
    margin: var(--space-0);
    color: var(--color-warning);
    font-size: var(--text-xs);
  }
  .token {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    word-break: break-all;
    color: var(--color-text);
  }
  .hint,
  .form-error {
    font-size: var(--text-xs);
  }
  .hint {
    color: var(--color-text-muted);
  }
  .form-error {
    color: var(--color-danger);
    margin: var(--space-0);
  }
</style>
