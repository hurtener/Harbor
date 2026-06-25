<script setup lang="ts">
import { withBase } from "vitepress";
import { finalCta as f } from "../landingSpec";
import CopyButton from "./CopyButton.vue";

const combined = `${f.install}\n${f.run}`;
</script>

<template>
  <section class="hb-final">
    <div class="hb-final__beam" aria-hidden="true"></div>
    <div class="hb-wrap hb-final__inner" v-reveal>
      <h2 class="hb-final__title">{{ f.headline }}</h2>
      <p class="hb-final__sub">{{ f.sub }}</p>

      <div class="hb-final__cmd">
        <pre><code><span class="p">$ </span>{{ f.install }}
<span class="p">$ </span>{{ f.run }}</code></pre>
        <CopyButton :text="combined" label="Copy commands" />
      </div>

      <div class="hb-final__cta">
        <a
          v-for="c in f.ctas"
          :key="c.link"
          class="hb-btn"
          :class="c.kind === 'primary' ? 'hb-btn--primary' : 'hb-btn--ghost'"
          :href="withBase(c.link)"
        >
          {{ c.label }}
        </a>
      </div>
    </div>
  </section>
</template>

<style scoped>
.hb-final {
  position: relative;
  overflow: hidden;
  padding: var(--hb-section-y) var(--hb-space-5);
  border-top: 1px solid var(--hb-border);
  background:
    radial-gradient(800px 360px at 50% 120%, rgba(43, 182, 204, 0.14), transparent 60%),
    var(--hb-bg-soft);
  text-align: center;
}
.hb-final__inner {
  max-width: 720px;
  margin: 0 auto;
}
.hb-final__title {
  font-size: var(--hb-fs-h2);
  font-weight: 700;
  letter-spacing: -0.02em;
  margin: 0;
  color: var(--hb-fg);
  border: 0;
  padding: 0;
}
.hb-final__sub {
  margin: var(--hb-space-4) auto 0;
  max-width: 56ch;
  font-size: var(--hb-fs-lead);
  line-height: 1.6;
  color: var(--hb-fg-muted);
}
.hb-final__cmd {
  display: inline-flex;
  align-items: center;
  gap: var(--hb-space-4);
  max-width: 100%;
  margin-top: var(--hb-space-6);
  padding: var(--hb-space-4) var(--hb-space-4) var(--hb-space-4) var(--hb-space-5);
  background: var(--hb-code-bg);
  border: 1px solid var(--hb-code-border);
  border-radius: var(--hb-radius);
  text-align: left;
}
.hb-final__cmd pre {
  margin: 0;
  min-width: 0;
  overflow-x: auto;
}
.hb-final__cmd :deep(.hb-copy) {
  flex-shrink: 0;
}
.hb-final__cmd code {
  font: var(--hb-fs-mono) / 1.7 var(--hb-font-mono);
  color: var(--hb-code-fg);
}
.hb-final__cmd .p {
  color: var(--hb-code-prompt);
  user-select: none;
}
.hb-final__cta {
  display: flex;
  flex-wrap: wrap;
  justify-content: center;
  gap: var(--hb-space-3);
  margin-top: var(--hb-space-6);
}
</style>
