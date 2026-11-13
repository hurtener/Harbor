<script setup lang="ts">
import { withBase } from "vitepress";
import { whatsNew as w } from "../landingSpec";
import Section from "./Section.vue";
</script>

<template>
  <Section id="whats-new" alt :eyebrow="w.eyebrow">
    <div class="hb-new" v-reveal>
      <div class="hb-new__head">
        <h2 class="hb-new__title">{{ w.headline }}</h2>
        <span class="hb-new__date">{{ w.date }}</span>
      </div>
      <ul class="hb-new__list">
        <li v-for="b in w.bullets" :key="b.title">
          <h3>{{ b.title }}</h3>
          <p v-if="b.body">{{ b.body }}</p>
        </li>
      </ul>
      <dl v-if="w.earlier.length" class="hb-new__earlier">
        <div v-for="e in w.earlier" :key="e.version">
          <dt>{{ e.version }}</dt>
          <dd>{{ e.summary }}</dd>
        </div>
      </dl>
      <a class="hb-btn hb-btn--ghost" :href="withBase(w.cta.link)">{{ w.cta.label }} →</a>
    </div>
  </Section>
</template>

<style scoped>
.hb-new {
  padding: var(--hb-space-8);
  background:
    radial-gradient(700px 300px at 90% -20%, rgba(43, 182, 204, 0.1), transparent 60%),
    var(--hb-panel);
  border: 1px solid var(--hb-border-strong);
  border-radius: var(--hb-radius-lg);
}
.hb-new__head {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: var(--hb-space-4);
}
.hb-new__title {
  margin: 0;
  font-size: var(--hb-fs-h2);
  font-weight: 680;
  letter-spacing: -0.02em;
  color: var(--hb-fg);
  border: 0;
  padding: 0;
}
.hb-new__date {
  font: 600 0.8rem var(--hb-font-mono);
  color: var(--hb-accent-bright);
}
.hb-new__list {
  list-style: none;
  margin: var(--hb-space-6) 0;
  padding: 0;
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--hb-space-5);
}
.hb-new__list h3 {
  margin: 0;
  font-size: 1rem;
  color: var(--hb-fg);
  border: 0;
  padding: 0;
}
.hb-new__list h3::before {
  content: "›";
  color: var(--hb-accent);
  margin-right: 0.5rem;
  font-weight: 700;
}
.hb-new__list p {
  margin: var(--hb-space-2) 0 0;
  font-size: 0.88rem;
  line-height: 1.58;
  color: var(--hb-fg-muted);
}
.hb-new__earlier {
  margin: 0 0 var(--hb-space-6);
  padding-top: var(--hb-space-5);
  border-top: 1px solid var(--hb-border);
  display: grid;
  gap: var(--hb-space-2);
}
.hb-new__earlier div {
  display: flex;
  gap: var(--hb-space-4);
  align-items: baseline;
}
.hb-new__earlier dt {
  flex: none;
  min-width: 4.5rem;
  font: 600 0.8rem var(--hb-font-mono);
  color: var(--hb-accent-bright);
}
.hb-new__earlier dd {
  margin: 0;
  font-size: 0.88rem;
  line-height: 1.5;
  color: var(--hb-fg-muted);
}
@media (max-width: 760px) {
  .hb-new__list {
    grid-template-columns: 1fr;
  }
  .hb-new__earlier div {
    flex-direction: column;
    gap: 0;
  }
}
</style>
