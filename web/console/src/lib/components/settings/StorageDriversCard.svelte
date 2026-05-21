<script lang="ts">
  // Settings — Storage Drivers card (Phase 73m / D-129).
  //
  // Read-only. Consumes 72f's `runtime.drivers` Protocol method (D-111)
  // — the configured driver name + optional posture mode per
  // persistence-shaped subsystem. The DSN is NEVER on the wire (72f
  // strips it); this card renders driver names only.
  import type { RuntimeDrivers } from '$lib/protocol/settings.js';

  let { drivers }: { drivers: RuntimeDrivers | null } = $props();
</script>

<div class="card-body" data-testid="settings-storage-drivers">
  {#if drivers === null || drivers.subsystems.length === 0}
    <p class="muted">No driver posture reported.</p>
  {:else}
    <table class="driver-table">
      <thead>
        <tr><th>Subsystem</th><th>Driver</th><th>Mode</th></tr>
      </thead>
      <tbody>
        {#each drivers.subsystems as d (d.subsystem)}
          <tr data-testid="storage-driver-row">
            <td>{d.subsystem}</td>
            <td class="mono">{d.driver}</td>
            <td class="muted">{d.mode || '—'}</td>
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
  .driver-table {
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
  .mono {
    font-family: var(--font-mono);
  }
  .muted {
    color: var(--color-text-muted);
  }
</style>
