<script lang="ts">
  // Audio MIME renderer — Phase 73l canonical renderer registry. Plays
  // the artifact audio from its resolved (presigned, D-026) src via the
  // browser's native audio element. The Console never inlines the audio
  // bytes; the src is a presigned URL the browser streams directly.
  import type { RendererProps } from './index.js';

  let { mime, src, filename }: RendererProps = $props();
</script>

<div class="audio-renderer" data-renderer-source="audio" data-mime={mime}>
  {#if filename}
    <p class="filename">{filename}</p>
  {/if}
  <!-- eslint-disable-next-line svelte/valid-compile -->
  <audio controls {src}>
    <track kind="captions" />
    Your browser does not support inline audio playback.
  </audio>
</div>

<style>
  .audio-renderer {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    width: 100%;
  }

  .filename {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    margin: var(--space-0);
  }

  audio {
    width: 100%;
  }
</style>
