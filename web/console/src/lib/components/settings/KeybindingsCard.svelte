<script lang="ts">
  // Settings — Keybindings card (Phase 73m / D-129).
  //
  // Console-local. Reflects 72h's `keybindings` table — the operator's
  // per-action key-chord overrides over the Console default set.
  import type { KeybindingRow } from '$lib/db/index.js';

  let { keybindings }: { keybindings: KeybindingRow[] } = $props();
</script>

<div class="card-body" data-testid="settings-keybindings">
  {#if keybindings.length === 0}
    <p class="note">
      No keybinding overrides — the Console default chords apply. Add an override
      to remap an action.
    </p>
  {:else}
    <table class="kb-table">
      <thead>
        <tr><th>Action</th><th>Chord</th></tr>
      </thead>
      <tbody>
        {#each keybindings as kb (kb.id)}
          <tr data-testid="keybinding-row">
            <td>{kb.action}</td>
            <td><kbd>{kb.key_chord}</kbd></td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

<style>
  .card-body {
    font-size: var(--text-sm);
  }
  .kb-table {
    width: 100%;
    border-collapse: collapse;
  }
  th {
    text-align: left;
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    padding-bottom: var(--space-2);
  }
  td {
    padding: var(--space-1) var(--space-2) var(--space-1) var(--space-0);
    color: var(--color-text);
  }
  kbd {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    padding: var(--space-0) var(--space-1);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
  }
  .note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
