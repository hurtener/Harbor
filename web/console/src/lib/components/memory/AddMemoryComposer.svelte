<script lang="ts">
  // Harbor Console — Memory page admin add-memory composer
  // (Phase 108n / D-186). Appends an operator-supplied conversation turn to
  // the caller's session memory via the REAL admin `memory.put` Protocol
  // method — audited, identity-mandatory (D-079). Disabled-with-tooltip
  // without the `admin` claim; never a fabricated success (§13).
  let {
    canAdmin,
    busy,
    onadd
  }: {
    canAdmin: boolean;
    busy: boolean;
    onadd: (userMessage: string, assistantResponse: string) => void;
  } = $props();

  let userMessage = $state('');
  let assistantResponse = $state('');

  const gate = 'Requires the admin scope claim — memory.put is an admin Protocol method (D-079).';

  function submit(): void {
    if (!canAdmin || userMessage.trim().length === 0) return;
    onadd(userMessage.trim(), assistantResponse.trim());
    userMessage = '';
    assistantResponse = '';
  }
</script>

<div class="composer" data-testid="memory-add-composer">
  <input
    class="composer-input"
    type="text"
    placeholder="User message…"
    bind:value={userMessage}
    data-testid="memory-add-user"
    disabled={!canAdmin || busy}
    title={canAdmin ? undefined : gate}
  />
  <input
    class="composer-input"
    type="text"
    placeholder="Assistant response…"
    bind:value={assistantResponse}
    data-testid="memory-add-assistant"
    disabled={!canAdmin || busy}
    title={canAdmin ? undefined : gate}
  />
  <button
    type="button"
    class="composer-add"
    data-testid="memory-add-submit"
    disabled={!canAdmin || busy || userMessage.trim().length === 0}
    title={canAdmin ? 'Append a memory turn (audited)' : gate}
    onclick={submit}
  >
    {busy ? 'Adding…' : 'Add memory turn'}
  </button>
  {#if !canAdmin}
    <p class="gate" data-testid="memory-add-gated">{gate}</p>
  {/if}
</div>

<style>
  .composer {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .composer-input {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
  }

  .composer-input:disabled {
    opacity: 0.5;
  }

  .composer-add {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
    align-self: flex-start;
  }

  .composer-add:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .gate {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
