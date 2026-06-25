<script setup lang="ts">
import { ref } from "vue";

const props = defineProps<{ text: string; label?: string }>();
const copied = ref(false);

async function copy() {
  try {
    await navigator.clipboard.writeText(props.text);
  } catch {
    return; // clipboard blocked (e.g. insecure context) — fail quietly, no fake success
  }
  copied.value = true;
  setTimeout(() => (copied.value = false), 1200);
}
</script>

<template>
  <button
    class="hb-copy"
    type="button"
    :aria-label="copied ? 'Copied' : label || 'Copy to clipboard'"
    @click="copy"
  >
    <span aria-hidden="true">{{ copied ? "✓" : "Copy" }}</span>
    <span class="sr-only" role="status" aria-live="polite">{{ copied ? "Copied" : "" }}</span>
  </button>
</template>

<style scoped>
.hb-copy {
  font: 600 0.72rem var(--hb-font-mono);
  letter-spacing: 0.02em;
  color: var(--hb-fg-muted);
  background: rgba(255, 255, 255, 0.04);
  border: 1px solid var(--hb-border-strong);
  border-radius: var(--hb-radius-sm);
  padding: 0.28rem 0.55rem;
  cursor: pointer;
  transition: color var(--hb-dur-fast) var(--hb-ease),
    border-color var(--hb-dur-fast) var(--hb-ease),
    background var(--hb-dur-fast) var(--hb-ease);
}
.hb-copy:hover {
  color: var(--hb-fg);
  border-color: var(--hb-accent);
  background: var(--hb-accent-weak);
}
.hb-copy:focus-visible {
  outline: 2px solid var(--hb-accent-bright);
  outline-offset: 2px;
}
.sr-only {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}
</style>
