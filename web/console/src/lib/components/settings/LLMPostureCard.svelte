<script lang="ts">
  // Settings — LLM-Provider Posture card (Phase 73m / D-129).
  //
  // Read-only. Consumes 72g's `llm.posture` Protocol method (D-112) —
  // the bound LLM provider / model / region + the `mock_mode` flag.
  // When `mock_mode` is true the dev-mock banner renders (per the
  // acceptance criteria + D-089). Console-driven provider swap is
  // post-V1 (page-settings.md §10).
  import type { LLMPostureResponse } from '$lib/protocol/settings.js';
  import MockModeBanner from './MockModeBanner.svelte';

  let { llm }: { llm: LLMPostureResponse | null } = $props();
</script>

<div class="card-body" data-testid="settings-llm-posture">
  {#if llm?.mock_mode}
    <MockModeBanner />
  {/if}
  {#if llm === null}
    <p class="muted">LLM posture unavailable.</p>
  {:else}
    <dl class="kv">
      <dt>Provider</dt>
      <dd data-testid="llm-provider">{llm.provider || '—'}</dd>
      <dt>Model</dt>
      <dd>{llm.model || '—'}</dd>
      <dt>Region / endpoint</dt>
      <dd class="muted">{llm.region || '—'}</dd>
      <dt>Mode</dt>
      <dd data-testid="llm-mode">{llm.mock_mode ? 'mock (dev-only)' : 'live'}</dd>
    </dl>
  {/if}
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
  .muted {
    color: var(--color-text-muted);
  }
</style>
