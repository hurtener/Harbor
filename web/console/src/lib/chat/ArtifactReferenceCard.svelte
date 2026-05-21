<script lang="ts">
  // Chat module — artifact-reference card (Phase 73n / D-130).
  //
  // Renders a `ChatArtifactRef` attached to a chat message. Heavy
  // content ALWAYS flows by reference (D-026) — this card never inlines
  // the artifact bytes. It resolves the artifact to a presigned URL via
  // the injected `ChatProtocolClient.resolveArtifact`, then DISPATCHES
  // the artifact's MIME through the Phase 73l canonical renderer
  // registry (`dispatchRenderer`) to pick the preview component.
  //
  // This is the chat module's concrete consumer of the renderer
  // registry extension: an artifact whose MIME the registry knows
  // renders inline-preview; an unknown MIME falls through to the
  // registry's `fallbackRenderer` (a Download placeholder).
  import { dispatchRenderer } from './renderers/index.js';
  import type { ChatArtifactRef, ChatProtocolClient } from './types.js';

  let {
    artifact,
    client,
    preview = false
  }: {
    artifact: ChatArtifactRef;
    client: ChatProtocolClient;
    /** When true, the card resolves + renders an inline preview. */
    preview?: boolean;
  } = $props();

  let resolvedURL = $state('');
  let resolveError = $state('');
  let resolving = $state(false);

  const descriptor = $derived(dispatchRenderer(artifact.mime));

  async function resolve(): Promise<void> {
    resolving = true;
    resolveError = '';
    try {
      resolvedURL = await client.resolveArtifact(artifact.id);
    } catch (e) {
      resolveError = e instanceof Error ? e.message : 'failed to resolve artifact';
    } finally {
      resolving = false;
    }
  }

  $effect(() => {
    if (preview && resolvedURL === '' && !resolving && resolveError === '') {
      void resolve();
    }
  });
</script>

<div
  class="artifact-card"
  data-testid="artifact-reference-card"
  data-artifact-id={artifact.id}
  data-renderer-source={descriptor.source}
>
  <div class="artifact-head">
    <span class="artifact-icon" aria-hidden="true">⎙</span>
    <span class="artifact-name">{artifact.filename}</span>
    <span class="artifact-mime">{artifact.mime}</span>
    {#if artifact.sizeBytes !== undefined}
      <span class="artifact-size">{artifact.sizeBytes} B</span>
    {/if}
  </div>
  <p class="artifact-hint">Heavy content — referenced, not inlined (D-026).</p>

  {#if preview}
    {#if resolving}
      <p class="status">Resolving preview…</p>
    {:else if resolveError !== ''}
      <p class="status status-error">Could not resolve: {resolveError}</p>
    {:else if resolvedURL !== ''}
      {@const Renderer = descriptor.component}
      <div class="artifact-preview">
        <Renderer mime={artifact.mime} src={resolvedURL} filename={artifact.filename} />
      </div>
      <a class="artifact-download" href={resolvedURL} target="_blank" rel="noopener noreferrer">
        Download
      </a>
    {/if}
  {/if}
</div>

<style>
  .artifact-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-3);
    background: var(--color-surface);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  .artifact-head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .artifact-icon {
    color: var(--color-text-muted);
  }

  .artifact-name {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .artifact-mime {
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--color-text-muted);
  }

  .artifact-size {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    margin-left: auto;
  }

  .artifact-hint {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .artifact-preview {
    margin-top: var(--space-2);
  }

  .artifact-download {
    margin-top: var(--space-1);
    font-size: var(--text-sm);
    color: var(--color-accent);
    text-decoration: none;
  }

  .status {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .status-error {
    color: var(--color-danger);
  }
</style>
