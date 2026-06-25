<script setup lang="ts">
import { computed } from "vue";
import { highlight } from "../highlight";
import CopyButton from "./CopyButton.vue";

const props = defineProps<{
  code: string;
  lang: string;
  file?: string;
  dots?: boolean;
}>();

const html = computed(() => highlight(props.code, props.lang));
</script>

<template>
  <div class="hb-code">
    <div class="hb-code__bar">
      <span v-if="dots" class="hb-code__dots" aria-hidden="true">
        <i></i><i></i><i></i>
      </span>
      <span class="hb-code__file">{{ file }}</span>
      <CopyButton :text="code" />
    </div>
    <pre class="hb-code__pre"><code v-html="html" /></pre>
  </div>
</template>

<style scoped>
.hb-code {
  background: var(--hb-code-bg);
  border: 1px solid var(--hb-code-border);
  border-radius: var(--hb-radius);
  overflow: hidden;
  box-shadow: var(--hb-shadow);
}
.hb-code__bar {
  display: flex;
  align-items: center;
  gap: var(--hb-space-3);
  padding: 0.55rem 0.75rem;
  background: var(--hb-code-bar);
  border-bottom: 1px solid var(--hb-code-border);
}
.hb-code__dots {
  display: inline-flex;
  gap: 0.4rem;
}
.hb-code__dots i {
  width: 0.66rem;
  height: 0.66rem;
  border-radius: 50%;
  background: var(--hb-code-border);
}
.hb-code__file {
  flex: 1;
  font: 500 0.74rem var(--hb-font-mono);
  color: var(--hb-code-muted);
}
.hb-code__pre {
  margin: 0;
  padding: var(--hb-space-4) var(--hb-space-5);
  overflow-x: auto;
  font: var(--hb-fs-mono) / 1.7 var(--hb-font-mono);
  color: var(--hb-code-fg);
}
.hb-code__pre :deep(.ln) {
  display: block;
  white-space: pre;
}
.hb-code__pre :deep(.tok-com) {
  color: #5f7a86;
  font-style: italic;
}
.hb-code__pre :deep(.tok-str) {
  color: #8be9e6;
}
.hb-code__pre :deep(.tok-kw) {
  color: var(--hb-code-kw);
}
.hb-code__pre :deep(.tok-fn) {
  color: #d6e8ee;
}
.hb-code__pre :deep(.tok-num) {
  color: var(--hb-beacon);
}
.hb-code__pre :deep(.tok-prompt) {
  color: var(--hb-code-prompt);
  user-select: none;
}
</style>
