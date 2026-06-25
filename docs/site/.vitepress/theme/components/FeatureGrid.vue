<script setup lang="ts">
import Section from "./Section.vue";

defineProps<{
  id?: string;
  eyebrow?: string;
  headline?: string;
  sub?: string;
  alt?: boolean;
  tiles: { title: string; body: string }[];
}>();
</script>

<template>
  <Section :id="id" :eyebrow="eyebrow" :headline="headline" :sub="sub" :alt="alt">
    <div class="hb-grid" v-reveal>
      <article v-for="t in tiles" :key="t.title" class="hb-tile">
        <span class="hb-tile__mark" aria-hidden="true"></span>
        <h3 class="hb-tile__title">{{ t.title }}</h3>
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
.hb-tile__title {
  margin: 0;
  font-size: 1.04rem;
  color: var(--hb-fg);
  border: 0;
  padding: 0;
}
.hb-tile__body {
  margin: var(--hb-space-3) 0 0;
  font-size: 0.9rem;
  line-height: 1.6;
  color: var(--hb-fg-muted);
}
</style>
