<script lang="ts">
  // Settings — Time & Locale card (Phase 73m / D-129).
  //
  // Console-local. Reflects 72h's `profiles` table tz + locale columns.
  // A null column means "browser default" (the 72h schema convention).
  import type { Profile } from '$lib/db/index.js';

  let { profile }: { profile: Profile | null } = $props();

  const browserTZ =
    typeof Intl !== 'undefined' ? Intl.DateTimeFormat().resolvedOptions().timeZone : 'UTC';
  const browserLocale =
    typeof navigator !== 'undefined' ? navigator.language : 'en';
</script>

<div class="card-body" data-testid="settings-time-locale">
  <dl class="kv">
    <dt>Time zone</dt>
    <dd>{profile?.tz || `${browserTZ} (browser default)`}</dd>
    <dt>Locale</dt>
    <dd>{profile?.locale || `${browserLocale} (browser default)`}</dd>
  </dl>
  <p class="note">Date / time across the Console renders in this zone + locale.</p>
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }
  .kv {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-1) var(--space-4);
    margin: var(--space-0);
  }
  dt {
    color: var(--color-text-muted);
  }
  dd {
    margin: var(--space-0);
    color: var(--color-text);
  }
  .note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
