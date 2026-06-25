<script setup lang="ts">
import { persistence as p } from "../landingSpec";
import Section from "./Section.vue";
</script>

<template>
  <Section id="persistence" alt :eyebrow="p.eyebrow" :headline="p.headline" :sub="p.sub">
    <div class="hb-persist" v-reveal>
      <div class="hb-matrix-scroll">
      <table class="hb-matrix">
        <thead>
          <tr>
            <th scope="col"></th>
            <th v-for="c in p.matrix.columns" :key="c" scope="col">{{ c }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="row in p.matrix.rows" :key="row.feature">
            <th scope="row">{{ row.feature }}</th>
            <td v-for="(cell, i) in row.cells" :key="i">
              <span v-if="cell === true" class="hb-matrix__yes" aria-label="yes">✓</span>
              <span v-else>{{ cell }}</span>
            </td>
          </tr>
        </tbody>
      </table>
      </div>

      <div class="hb-deploy">
        <article v-for="d in p.deploy" :key="d.cmd" class="hb-deploy__card">
          <code class="hb-deploy__cmd">{{ d.cmd }}</code>
          <p>{{ d.body }}</p>
        </article>
      </div>
    </div>
    <p class="hb-persist__note" v-reveal>{{ p.note }}</p>
  </Section>
</template>

<style scoped>
.hb-persist {
  display: grid;
  grid-template-columns: 1.15fr 0.85fr;
  gap: var(--hb-space-6);
  align-items: start;
}
.hb-persist > * {
  min-width: 0;
}
.hb-matrix-scroll {
  overflow-x: auto;
}
.hb-matrix {
  width: 100%;
  border-collapse: collapse;
  font-size: 0.9rem;
  background: var(--hb-panel);
  border: 1px solid var(--hb-border-strong);
  border-radius: var(--hb-radius);
  overflow: hidden;
}
.hb-matrix th,
.hb-matrix td {
  padding: 0.7rem 0.9rem;
  text-align: center;
  border-bottom: 1px solid var(--hb-border);
}
.hb-matrix thead th {
  font: 600 0.78rem var(--hb-font-mono);
  color: var(--hb-accent-bright);
  background: var(--hb-bg-soft);
}
.hb-matrix tbody th[scope="row"] {
  text-align: left;
  color: var(--hb-fg);
  font-weight: 600;
}
.hb-matrix td {
  color: var(--hb-fg-muted);
}
.hb-matrix tr:last-child th,
.hb-matrix tr:last-child td {
  border-bottom: 0;
}
.hb-matrix__yes {
  color: var(--hb-ok);
  font-weight: 700;
}
.hb-deploy {
  display: flex;
  flex-direction: column;
  gap: var(--hb-space-4);
}
.hb-deploy__card {
  padding: var(--hb-space-4) var(--hb-space-5);
  background: var(--hb-bg-soft);
  border: 1px solid var(--hb-border);
  border-left: 2px solid var(--hb-accent);
  border-radius: 0 var(--hb-radius) var(--hb-radius) 0;
}
.hb-deploy__cmd {
  font: 700 0.86rem var(--hb-font-mono);
  color: var(--hb-accent-bright);
}
.hb-deploy__card p {
  margin: var(--hb-space-2) 0 0;
  font-size: 0.88rem;
  line-height: 1.55;
  color: var(--hb-fg-muted);
}
.hb-persist__note {
  margin: var(--hb-space-5) 0 0;
  font: 600 0.92rem var(--hb-font-mono);
  color: var(--hb-fg-dim);
}
@media (max-width: 880px) {
  .hb-persist {
    grid-template-columns: 1fr;
  }
}
</style>
