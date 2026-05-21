<script lang="ts">
  // Settings — Runtime Info card (Phase 73m / D-129).
  //
  // Read-only. Consumes 72f's `runtime.info` Protocol method (D-111) —
  // build identity, Protocol version, capabilities, uptime. Phase 73m
  // ships NO new method here; it composes the shipped surface.
  import type { RuntimeInfo } from '$lib/protocol/settings.js';

  let { info }: { info: RuntimeInfo | null } = $props();

  function uptime(seconds: number): string {
    const h = Math.floor(seconds / 3600);
    const m = Math.floor((seconds % 3600) / 60);
    return h > 0 ? `${h}h ${m}m` : `${m}m`;
  }
</script>

<div class="card-body" data-testid="settings-runtime-info">
  {#if info === null}
    <p class="muted">Runtime info unavailable.</p>
  {:else}
    <dl class="kv">
      <dt>Instance</dt>
      <dd>{info.display_name || info.instance_id}</dd>
      <dt>Build version</dt>
      <dd>{info.build_version}</dd>
      <dt>Build commit</dt>
      <dd class="mono">{info.build_commit}</dd>
      <dt>Go toolchain</dt>
      <dd>{info.build_go_version}</dd>
      <dt>Protocol version</dt>
      <dd data-testid="runtime-protocol-version">{info.protocol_version}</dd>
      <dt>Uptime</dt>
      <dd>{uptime(info.uptime_seconds)}</dd>
      <dt>Capabilities</dt>
      <dd>{info.capabilities.length} advertised</dd>
    </dl>
  {/if}
</div>

<style>
  .card-body {
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
  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
  .muted {
    color: var(--color-text-muted);
  }
</style>
