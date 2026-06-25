<script setup lang="ts">
import { rigor as r } from "../landingSpec";
import Section from "./Section.vue";
</script>

<template>
  <Section id="rigor" :eyebrow="r.eyebrow" :headline="r.headline" :sub="r.sub">
    <div class="hb-stats" v-reveal>
      <div v-for="s in r.stats" :key="s.label" class="hb-stat">
        <span class="hb-stat__value">{{ s.value }}</span>
        <span class="hb-stat__label">{{ s.label }}</span>
      </div>
    </div>

    <ul class="hb-discipline" v-reveal>
      <li v-for="d in r.discipline" :key="d">
        <span class="hb-discipline__tick" aria-hidden="true">✓</span>{{ d }}
      </li>
    </ul>
  </Section>
</template>

<style scoped>
.hb-stats {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(150px, 1fr));
  gap: var(--hb-space-4);
}
.hb-stat {
  display: flex;
  flex-direction: column;
  gap: 0.35rem;
  padding: var(--hb-space-5);
  background: var(--hb-panel);
  border: 1px solid var(--hb-border-strong);
  border-radius: var(--hb-radius);
  text-align: center;
}
.hb-stat__value {
  font-size: clamp(1.9rem, 3.4vw, 2.6rem);
  font-weight: 720;
  line-height: 1;
  letter-spacing: -0.02em;
  color: var(--hb-accent-bright);
  font-variant-numeric: tabular-nums;
}
.hb-stat__label {
  font-size: 0.82rem;
  line-height: 1.35;
  color: var(--hb-fg-muted);
}
.hb-discipline {
  list-style: none;
  margin: var(--hb-space-6) 0 0;
  padding: 0;
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
  gap: var(--hb-space-3) var(--hb-space-5);
}
.hb-discipline li {
  display: flex;
  align-items: baseline;
  gap: var(--hb-space-3);
  font-size: 0.9rem;
  color: var(--hb-fg-muted);
}
.hb-discipline__tick {
  color: var(--hb-ok);
  font-weight: 700;
}
</style>
