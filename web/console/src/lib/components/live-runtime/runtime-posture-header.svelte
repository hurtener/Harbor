<script lang="ts">
  // Harbor Console — Live Runtime cockpit posture header (Phase 108e / D-177).
  //
  // Row 1 of the cockpit: the single-runtime identity + posture banner.
  //   - left: the runtime label (agent-registry display name when available,
  //     else the runtime host) + the advertised capability chips (from
  //     `runtime.info.capabilities`);
  //   - right: a connection dot, the Protocol version, and a Refresh button.
  //
  // The capability chips are the runtime's advertised `runtime.info`
  // capability set — never fabricated; an empty set renders an honest
  // "no capabilities advertised" note. The connection dot reflects whether
  // the Console is attached.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import RotateCw from '@lucide/svelte/icons/rotate-cw';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';

  let {
    runtimeLabel,
    protocolVersion,
    capabilities,
    disconnected,
    onRefresh
  }: {
    /** The runtime label — agent-registry name or the runtime host. */
    runtimeLabel: string;
    /** The Protocol version from `runtime.info`, or empty until resolved. */
    protocolVersion: string;
    /** The advertised capability names (from `runtime.info.capabilities`). */
    capabilities: string[];
    /** True when the Console is not attached to a runtime. */
    disconnected: boolean;
    /** Re-runs the page load. */
    onRefresh: () => void;
  } = $props();
</script>

<section class="panel card posture-header" data-testid="runtime-posture-header">
  <div class="posture-left">
    <div class="runtime-line">
      <span class="runtime-label" data-testid="posture-runtime-label">{runtimeLabel}</span>
    </div>
    {#if capabilities.length > 0}
      <div class="cap-chips" data-testid="posture-capability-chips">
        {#each capabilities as cap (cap)}
          <span class="cap-chip" data-testid="posture-capability-chip">{cap}</span>
        {/each}
      </div>
    {:else}
      <p class="cap-empty" data-testid="posture-capability-empty">
        No capabilities advertised
      </p>
    {/if}
  </div>

  <div class="posture-right">
    <span
      class="conn-dot"
      data-state={disconnected ? 'disconnected' : 'connected'}
      data-testid="posture-connection-dot"
      title={disconnected ? 'Not attached to a runtime' : 'Attached'}
      aria-hidden="true"
    ></span>
    <span class="conn-label" data-testid="posture-connection-label">
      {disconnected ? 'Disconnected' : 'Connected'}
    </span>
    {#if protocolVersion !== ''}
      <span class="protocol" data-testid="posture-protocol-version">
        Protocol {protocolVersion}
      </span>
    {/if}
    <button
      type="button"
      class="refresh"
      data-testid="live-runtime-refresh"
      disabled={disconnected}
      title={disconnected ? DISCONNECTED_TOOLTIP : 'Refresh'}
      onclick={onRefresh}
    >
      <RotateCw size={14} aria-hidden="true" /> Refresh
    </button>
  </div>
</section>

<style>
  .card {
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .posture-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    flex-wrap: wrap;
  }

  .posture-left {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-width: var(--space-0);
  }

  .runtime-label {
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .cap-chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .cap-chip {
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-accent);
    background: var(--color-accent-soft);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .cap-empty {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .posture-right {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .conn-dot {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: var(--radius-pill);
    background: var(--color-success);
  }

  .conn-dot[data-state='disconnected'] {
    background: var(--color-text-muted);
  }

  .conn-label,
  .protocol {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .refresh {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .refresh:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
