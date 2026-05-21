<script lang="ts">
  // Settings — About card (Phase 73m / D-129).
  //
  // Static Console content: Console version, the negotiated Protocol
  // version (from 72f's `runtime.info`), and the license (Apache-2.0
  // per D-014). No Protocol mutation.
  import type { RuntimeInfo } from '$lib/protocol/settings.js';

  let { info }: { info: RuntimeInfo | null } = $props();

  /** The Console build version — Console-local static content. */
  const CONSOLE_VERSION = 'v0.0.0-dev';
</script>

<div class="card-body" data-testid="settings-about">
  <dl class="kv">
    <dt>Console version</dt>
    <dd>{CONSOLE_VERSION}</dd>
    <dt>Negotiated Protocol</dt>
    <dd>{info?.protocol_version || '—'}</dd>
    <dt>License</dt>
    <dd>Apache-2.0</dd>
  </dl>
  <p class="note">
    Harbor Console — a Protocol client of the Harbor Runtime. The Console reads
    only canonical Protocol events / state, never runtime internals.
  </p>
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
