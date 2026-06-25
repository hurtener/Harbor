<script setup lang="ts">
import Section from "./Section.vue";

defineProps<{
  id?: string;
  eyebrow?: string;
  headline?: string;
  sub?: string;
  alt?: boolean;
  note?: string;
  cards: { title: string; lead?: string; body: string }[];
}>();
</script>

<template>
  <Section :id="id" :eyebrow="eyebrow" :headline="headline" :sub="sub" :alt="alt">
    <div class="hb-cards" v-reveal>
      <article v-for="(c, i) in cards" :key="c.title" class="hb-card" :style="{ '--d': i * 70 + 'ms' }">
        <span class="hb-card__num" aria-hidden="true">{{ String(i + 1).padStart(2, "0") }}</span>
        <h3 class="hb-card__title">{{ c.title }}</h3>
        <p v-if="c.lead" class="hb-card__lead">{{ c.lead }}</p>
        <p class="hb-card__body">{{ c.body }}</p>
      </article>
    </div>
    <p v-if="note" class="hb-cards__note" v-reveal>{{ note }}</p>
  </Section>
</template>

<style scoped>
.hb-cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: var(--hb-space-5);
}
.hb-card {
  position: relative;
  padding: var(--hb-space-6);
  background: var(--hb-panel);
  border: 1px solid var(--hb-border-strong);
  border-radius: var(--hb-radius-lg);
  overflow: hidden;
  transition: border-color var(--hb-dur) var(--hb-ease),
    transform var(--hb-dur) var(--hb-ease);
}
.hb-card::before {
  content: "";
  position: absolute;
  inset: 0 0 auto;
  height: 2px;
  background: linear-gradient(90deg, var(--hb-accent), transparent);
  opacity: 0;
  transition: opacity var(--hb-dur) var(--hb-ease);
}
.hb-card:hover {
  border-color: var(--hb-accent-line);
  transform: translateY(-3px);
}
.hb-card:hover::before {
  opacity: 1;
}
.hb-card__num {
  font: 700 0.78rem var(--hb-font-mono);
  color: var(--hb-accent);
}
.hb-card__title {
  margin: var(--hb-space-2) 0 0;
  font-size: 1.18rem;
  color: var(--hb-fg);
  border: 0;
  padding: 0;
  letter-spacing: -0.01em;
}
.hb-card__lead {
  margin: var(--hb-space-3) 0 0;
  font-weight: 600;
  color: var(--hb-accent-bright);
  font-size: 0.92rem;
}
.hb-card__body {
  margin: var(--hb-space-3) 0 0;
  font-size: 0.94rem;
  line-height: 1.62;
  color: var(--hb-fg-muted);
}
.hb-cards__note {
  margin: var(--hb-space-6) 0 0;
  max-width: 760px;
  font-size: 0.92rem;
  line-height: 1.6;
  color: var(--hb-fg-dim);
}
</style>
