<script lang="ts">
  // Settings — the dev-mock LLM banner (Phase 73m / D-129, D-089).
  //
  // Rendered in the Governance Posture + LLM-Provider Posture cards when
  // 72g's `llm.posture` response carries `mock_mode: true`. The text
  // matches the `harbor dev` / `harbor console` stderr banner VERBATIM
  // (the §13 amendment): `DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION`.
  import { MOCK_MODE_BANNER } from '$lib/protocol/settings.js';
</script>

<div class="mock-banner" role="alert" data-testid="mock-mode-banner">
  <span class="mock-text">[{MOCK_MODE_BANNER}]</span>
</div>

<style>
  .mock-banner {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-color: var(--color-danger);
    border-radius: var(--radius-sm);
    background: var(--color-danger-soft);
    color: var(--color-danger);
    font-size: var(--text-sm);
    font-weight: 600;
  }
  .mock-text {
    letter-spacing: var(--tracking-wide);
  }
</style>
