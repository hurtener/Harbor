<script setup lang="ts">
import { ref } from "vue";
import { codeShowcase } from "../landingSpec";
import Section from "./Section.vue";
import CodeBlock from "./CodeBlock.vue";

const active = ref(0);
const tabRefs = ref<HTMLButtonElement[]>([]);

function onKey(e: KeyboardEvent) {
  const n = codeShowcase.tabs.length;
  if (e.key === "ArrowRight") active.value = (active.value + 1) % n;
  else if (e.key === "ArrowLeft") active.value = (active.value - 1 + n) % n;
  else return;
  e.preventDefault();
  tabRefs.value[active.value]?.focus();
}
</script>

<template>
  <Section
    id="quickstart"
    :eyebrow="codeShowcase.eyebrow"
    :headline="codeShowcase.headline"
    :sub="codeShowcase.sub"
  >
    <div class="hb-show" v-reveal>
      <div class="hb-show__tabs" role="tablist" aria-label="Code examples" @keydown="onKey">
        <button
          v-for="(t, i) in codeShowcase.tabs"
          :key="t.key"
          :ref="(el) => { if (el) tabRefs[i] = el as HTMLButtonElement; }"
          class="hb-show__tab"
          :class="{ 'is-active': active === i }"
          role="tab"
          type="button"
          :aria-selected="active === i"
          :tabindex="active === i ? 0 : -1"
          @click="active = i"
        >
          {{ t.label }}
        </button>
      </div>

      <div
        v-for="(t, i) in codeShowcase.tabs"
        v-show="active === i"
        :key="t.key"
        role="tabpanel"
        class="hb-show__panel"
      >
        <CodeBlock :code="t.code" :lang="t.lang" :file="t.file" dots />
      </div>

      <p class="hb-show__foot">{{ codeShowcase.footnote }}</p>
    </div>
  </Section>
</template>

<style scoped>
.hb-show {
  max-width: 880px;
}
.hb-show__tabs {
  display: inline-flex;
  gap: 0.25rem;
  padding: 0.25rem;
  background: var(--hb-panel);
  border: 1px solid var(--hb-border);
  border-radius: 999px;
  margin-bottom: var(--hb-space-4);
}
.hb-show__tab {
  font: 600 0.86rem/1 inherit;
  color: var(--hb-fg-muted);
  background: none;
  border: 0;
  border-radius: 999px;
  padding: 0.5rem 1.1rem;
  cursor: pointer;
  transition: color var(--hb-dur-fast) var(--hb-ease),
    background var(--hb-dur-fast) var(--hb-ease);
}
.hb-show__tab.is-active {
  color: var(--hb-bg);
  background: var(--hb-accent);
}
.hb-show__tab:focus-visible {
  outline: 2px solid var(--hb-accent-bright);
  outline-offset: 2px;
}
.hb-show__panel {
  animation: hb-fade var(--hb-dur) var(--hb-ease);
}
@keyframes hb-fade {
  from { opacity: 0; transform: translateY(6px); }
  to { opacity: 1; transform: translateY(0); }
}
.hb-show__foot {
  margin-top: var(--hb-space-4);
  font-size: 0.9rem;
  line-height: 1.6;
  color: var(--hb-fg-dim);
}
@media (prefers-reduced-motion: reduce) {
  .hb-show__panel {
    animation: none;
  }
}
</style>
