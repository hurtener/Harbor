<script lang="ts">
  // Events page — truncated-payload `Open artifact` link
  // (Phase 73g / D-125).
  //
  // When an event's payload exceeds the heavy-content threshold
  // (RFC §6.5 / D-026) the runtime emits an `artifact_ref` rather than
  // inline bytes. This component renders a `Truncated` badge plus an
  // `Open artifact` link; clicking it resolves the artifact via the
  // already-shipped `artifacts.get_ref` Protocol method (Phase 73l) and
  // opens the resolved URL.
  //
  // # Heavy bytes NEVER appear inline (D-026, CLAUDE.md §13)
  //
  // This component receives an `EventArtifactRef` — a reference, not
  // bytes. It NEVER reads payload bytes off the event; it resolves a URL
  // and hands the operator a link. The §13 raw-heavy-content rule, read
  // at the Console edge. Svelte 5 runes (D-092); tokens only.
  import type { ArtifactsNamespace } from '$lib/protocol/harbor.js';
  import type { EventArtifactRef } from '$lib/protocol/harbor.js';
  import type {
    ArtifactScope,
    ArtifactsGetRefRequest,
    ArtifactsGetRefResponse
  } from '$lib/protocol.js';

  let {
    payloadRef,
    artifacts,
    scope
  }: {
    /** The heavy-payload reference (id + mime + size — never bytes). */
    payloadRef: EventArtifactRef;
    /** The `artifacts.*` namespace from the page's HarborClient. */
    artifacts: ArtifactsNamespace;
    /** The identity-scoped artifact scope the event belongs to. */
    scope: ArtifactScope;
  } = $props();

  /** The resolution state. */
  let resolveState = $state<'idle' | 'resolving' | 'resolved' | 'error'>('idle');
  /** The resolved presigned / fetch URL. */
  let resolvedURL = $state<string | null>(null);
  /** The error message when resolution fails. */
  let errorMessage = $state<string | null>(null);

  async function resolve(): Promise<void> {
    resolveState = 'resolving';
    errorMessage = null;
    try {
      const req: ArtifactsGetRefRequest = {
        scope,
        id: payloadRef.artifact_ref.id
      };
      const resp = await artifacts.getRef<ArtifactsGetRefResponse>(
        req as unknown as Record<string, unknown>
      );
      resolvedURL = resp.presigned_url ?? null;
      resolveState = 'resolved';
    } catch (e) {
      // Fail loudly — never silently degrade to "no link" (CLAUDE.md §13).
      const pe = e as { code?: unknown; message?: unknown };
      errorMessage =
        typeof pe?.message === 'string'
          ? `${typeof pe.code === 'string' ? pe.code : 'error'}: ${pe.message}`
          : 'Failed to resolve artifact';
      resolveState = 'error';
    }
  }
</script>

<div class="truncated-payload" data-testid="truncated-payload">
  <span class="truncated-badge" data-testid="truncated-badge">Truncated</span>
  {#if resolveState === 'resolved' && resolvedURL}
    <a
      class="artifact-link"
      href={resolvedURL}
      target="_blank"
      rel="noopener noreferrer"
      data-testid="artifact-link-resolved"
    >
      Open artifact ↗
    </a>
  {:else if resolveState === 'error'}
    <span class="artifact-error" data-testid="artifact-link-error">{errorMessage}</span>
    <button type="button" class="artifact-retry" onclick={() => void resolve()}>Retry</button>
  {:else}
    <button
      type="button"
      class="artifact-link"
      data-testid="open-artifact"
      disabled={resolveState === 'resolving'}
      onclick={() => void resolve()}
    >
      {resolveState === 'resolving' ? 'Resolving…' : 'Open artifact'}
    </button>
  {/if}
  <span class="artifact-meta">
    {payloadRef.artifact_ref.mime ?? 'binary'}
    {#if payloadRef.artifact_ref.size}· {payloadRef.artifact_ref.size} bytes{/if}
  </span>
</div>

<style>
  .truncated-payload {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .truncated-badge {
    display: inline-flex;
    align-items: center;
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-warning);
    background: var(--color-warning-soft);
  }

  .artifact-link {
    background: transparent;
    border: none;
    padding: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-accent);
    text-decoration: none;
    cursor: pointer;
  }

  .artifact-link:hover {
    text-decoration: underline;
  }

  .artifact-link:disabled {
    color: var(--color-text-muted);
    cursor: progress;
  }

  .artifact-error {
    font-size: var(--text-xs);
    color: var(--color-danger);
  }

  .artifact-retry {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .artifact-meta {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
