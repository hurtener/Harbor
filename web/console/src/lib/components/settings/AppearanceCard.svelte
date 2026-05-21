<script lang="ts">
  // Settings — Appearance card (Phase 73m / D-129).
  //
  // Console-local. Reflects 72h's `profiles` table (theme / density /
  // motion). The card renders the operator's current preferences;
  // persisting an edit is a write to 72h's `profiles` table — this
  // phase consumes 72h's schema, it ships no new table.
  import type { Profile } from '$lib/db/index.js';

  let { profile }: { profile: Profile | null } = $props();

  const THEMES = ['light', 'dark', 'system'] as const;
  const DENSITIES = ['comfortable', 'compact'] as const;
  const MOTIONS = ['full', 'reduced'] as const;
</script>

<div class="card-body" data-testid="settings-appearance">
  <div class="field">
    <span class="field-label">Theme</span>
    <div class="options">
      {#each THEMES as theme (theme)}
        <span class="option" class:active={profile?.theme === theme}>{theme}</span>
      {/each}
    </div>
  </div>
  <div class="field">
    <span class="field-label">Density</span>
    <div class="options">
      {#each DENSITIES as d (d)}
        <span class="option" class:active={profile?.density === d}>{d}</span>
      {/each}
    </div>
  </div>
  <div class="field">
    <span class="field-label">Motion</span>
    <div class="options">
      {#each MOTIONS as m (m)}
        <span class="option" class:active={profile?.motion === m}>{m}</span>
      {/each}
    </div>
  </div>
  {#if profile === null}
    <p class="note">No saved profile — Console defaults apply.</p>
  {/if}
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .field-label {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  .options {
    display: flex;
    gap: var(--space-2);
  }
  .option {
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    color: var(--color-text-muted);
  }
  .option.active {
    background: var(--color-surface-raised);
    border-color: var(--color-accent);
    color: var(--color-text);
  }
  .note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
