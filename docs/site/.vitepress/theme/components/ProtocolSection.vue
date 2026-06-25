<script setup lang="ts">
import { withBase } from "vitepress";
import { protocol as p } from "../landingSpec";
import Section from "./Section.vue";
</script>

<template>
  <Section id="protocol" :eyebrow="p.eyebrow" :headline="p.headline" :sub="p.sub">
    <div class="hb-proto" v-reveal>
      <ul class="hb-proto__families">
        <li v-for="f in p.families" :key="f.name">
          <code class="hb-proto__name">{{ f.name }}</code>
          <span class="hb-proto__detail">{{ f.detail }}</span>
        </li>
      </ul>

      <aside class="hb-proto__aside">
        <p class="hb-proto__adopt">{{ p.adoption }}</p>
        <a class="hb-btn hb-btn--ghost" :href="withBase(p.cta.link)">{{ p.cta.label }} →</a>
      </aside>
    </div>
  </Section>
</template>

<style scoped>
.hb-proto {
  display: grid;
  grid-template-columns: 1.1fr 0.9fr;
  gap: var(--hb-space-6);
  align-items: start;
}
.hb-proto > * {
  min-width: 0;
}
.hb-proto__families {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 1px;
  background: var(--hb-border);
  border: 1px solid var(--hb-border);
  border-radius: var(--hb-radius);
  overflow: hidden;
}
.hb-proto__families li {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  padding: var(--hb-space-4);
  background: var(--hb-panel);
}
.hb-proto__name {
  font: 600 0.84rem var(--hb-font-mono);
  color: var(--hb-accent-bright);
}
.hb-proto__detail {
  font-size: 0.86rem;
  line-height: 1.5;
  color: var(--hb-fg-muted);
}
.hb-proto__aside {
  display: flex;
  flex-direction: column;
  gap: var(--hb-space-5);
  padding: var(--hb-space-6);
  background: linear-gradient(180deg, var(--hb-panel), var(--hb-bg-soft));
  border: 1px solid var(--hb-border-strong);
  border-radius: var(--hb-radius-lg);
}
.hb-proto__adopt {
  margin: 0;
  font-size: 0.94rem;
  line-height: 1.65;
  color: var(--hb-fg-muted);
}
@media (max-width: 880px) {
  .hb-proto {
    grid-template-columns: 1fr;
  }
}
</style>
