<script setup lang="ts">
import { withBase } from "vitepress";
import Section from "./Section.vue";

defineProps<{
  id?: string;
  eyebrow?: string;
  headline?: string;
  sub?: string;
  alt?: boolean;
  tiles: { title: string; body: string; kicker?: string; link?: string }[];
}>();
</script>

<template>
  <Section :id="id" :eyebrow="eyebrow" :headline="headline" :sub="sub" :alt="alt">
    <div class="hb-grid" v-reveal data-stagger>
      <article
        v-for="(t, i) in tiles"
        :key="t.title"
        class="hb-tile"
        :style="{ '--hb-i': i }"
      >
        <span v-if="t.kicker" class="hb-tile__kicker">{{ t.kicker }}</span>
        <span v-else class="hb-tile__mark" aria-hidden="true"></span>
        <h3 class="hb-tile__title">
          <a v-if="t.link" class="hb-tile__link" :href="withBase(t.link)">{{ t.title }}</a>
          <template v-else>{{ t.title }}</template>
        </h3>
        <p class="hb-tile__body">{{ t.body }}</p>
      </article>
    </div>
  </Section>
</template>

<style scoped>
.hb-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
  gap: var(--hb-space-4);
}
.hb-tile {
  position: relative;
  padding: var(--hb-space-5);
  background: var(--hb-bg-soft);
  border: 1px solid var(--hb-border);
  border-radius: var(--hb-radius);
  transition: border-color var(--hb-dur) var(--hb-ease),
    background var(--hb-dur) var(--hb-ease);
}
.hb-tile:hover {
  border-color: var(--hb-accent-line);
  background: var(--hb-panel);
}
.hb-tile__mark {
  display: block;
  width: 26px;
  height: 2px;
  border-radius: 2px;
  background: var(--hb-accent);
  margin-bottom: var(--hb-space-4);
}
.hb-tile__kicker {
  display: inline-block;
  margin-bottom: var(--hb-space-3);
  font: 600 0.72rem var(--hb-font-mono);
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--hb-accent-bright);
}
.hb-tile__title {
  margin: 0;
  font-size: 1.04rem;
  color: var(--hb-fg);
  border: 0;
  padding: 0;
}
/* The whole tile is the link target; the anchor text stays the title. */
.hb-tile__link {
  color: inherit;
  text-decoration: none;
}
.hb-tile__link::after {
  content: "";
  position: absolute;
  inset: 0;
  border-radius: inherit;
}
.hb-tile__link:focus-visible {
  outline: none;
}
.hb-tile:has(.hb-tile__link:focus-visible) {
  outline: 2px solid var(--hb-accent);
  outline-offset: 2px;
}
.hb-tile:has(.hb-tile__link)::before {
  content: "→";
  position: absolute;
  top: var(--hb-space-5);
  right: var(--hb-space-5);
  color: var(--hb-accent);
  opacity: 0;
  transition: opacity var(--hb-dur) var(--hb-ease);
}
.hb-tile:has(.hb-tile__link):hover::before {
  opacity: 1;
}
.hb-tile__body {
  margin: var(--hb-space-3) 0 0;
  font-size: 0.9rem;
  line-height: 1.6;
  color: var(--hb-fg-muted);
}
</style>
