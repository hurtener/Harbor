<script setup lang="ts">
import { architecture as a } from "../landingSpec";
import Section from "./Section.vue";
</script>

<template>
  <Section
    id="architecture"
    alt
    :eyebrow="a.eyebrow"
    :headline="a.headline"
    :sub="a.sub"
  >
    <div class="hb-arch" v-reveal role="img" aria-label="Harbor's four layers: client applications talk to the headless Runtime only through the versioned Harbor Protocol seam.">
      <div class="hb-arch__clients">
        <div v-for="c in a.clients" :key="c" class="hb-arch__client">{{ c }}</div>
      </div>

      <div class="hb-arch__seam">
        <span class="hb-arch__seam-beam" aria-hidden="true"></span>
        <span class="hb-arch__seam-label">Harbor Protocol · 0.1.0 — the only contract clients get</span>
      </div>

      <div class="hb-arch__runtime">
        <span class="hb-arch__runtime-tag">Headless Runtime</span>
        <span class="hb-arch__runtime-body">tasks · planner runtime · tools · memory · sessions · events · skills · artifacts · pause / resume</span>
      </div>
      <div class="hb-arch__cli">CLI — the single <code>harbor</code> binary, 11 subcommands</div>
    </div>

    <div class="hb-arch__layers" v-reveal>
      <article v-for="l in a.layers" :key="l.name" class="hb-arch__layer">
        <h3>{{ l.name }}</h3>
        <p class="hb-arch__role">{{ l.role }}</p>
        <p class="hb-arch__detail">{{ l.detail }}</p>
      </article>
    </div>
  </Section>
</template>

<style scoped>
.hb-arch {
  display: flex;
  flex-direction: column;
  gap: var(--hb-space-3);
  padding: var(--hb-space-6);
  background: var(--hb-panel);
  border: 1px solid var(--hb-border-strong);
  border-radius: var(--hb-radius-lg);
  box-shadow: var(--hb-shadow);
}
.hb-arch__clients {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: var(--hb-space-3);
}
.hb-arch__client {
  text-align: center;
  padding: var(--hb-space-4);
  background: var(--hb-bg-soft);
  border: 1px solid var(--hb-border);
  border-radius: var(--hb-radius);
  font-weight: 600;
  font-size: 0.92rem;
  color: var(--hb-fg);
}
.hb-arch__seam {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  height: 2px;
  margin: var(--hb-space-4) 0;
  background: var(--hb-accent-line);
}
.hb-arch__seam-beam {
  position: absolute;
  inset: 0;
  background: linear-gradient(90deg, transparent, var(--hb-accent-bright), transparent);
  background-size: 40% 100%;
  background-repeat: no-repeat;
  animation: hb-seam 3.6s linear infinite;
}
@keyframes hb-seam {
  from { background-position: -40% 0; }
  to { background-position: 140% 0; }
}
.hb-arch__seam-label {
  position: relative;
  background: var(--hb-panel);
  padding: 0 0.8rem;
  font: 600 0.75rem var(--hb-font-mono);
  color: var(--hb-accent-bright);
  white-space: nowrap;
}
.hb-arch__runtime {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: var(--hb-space-3);
  padding: var(--hb-space-5);
  background: linear-gradient(180deg, var(--hb-panel-2), var(--hb-bg-soft));
  border: 1px solid var(--hb-accent-line);
  border-radius: var(--hb-radius);
}
.hb-arch__runtime-tag {
  font-weight: 700;
  color: var(--hb-fg);
}
.hb-arch__runtime-body {
  font: 0.84rem var(--hb-font-mono);
  color: var(--hb-fg-muted);
}
.hb-arch__cli {
  text-align: center;
  padding: var(--hb-space-3);
  font-size: 0.88rem;
  color: var(--hb-fg-muted);
  background: var(--hb-bg-soft);
  border: 1px solid var(--hb-border);
  border-radius: var(--hb-radius);
}
.hb-arch__cli code {
  color: var(--hb-accent-bright);
  font-family: var(--hb-font-mono);
}
.hb-arch__layers {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: var(--hb-space-4);
  margin-top: var(--hb-space-6);
}
.hb-arch__layer {
  padding: var(--hb-space-4);
  border-left: 2px solid var(--hb-accent);
  background: var(--hb-bg-soft);
  border-radius: 0 var(--hb-radius) var(--hb-radius) 0;
}
.hb-arch__layer h3 {
  margin: 0;
  font-size: 1.02rem;
  color: var(--hb-fg);
  border: 0;
  padding: 0;
}
.hb-arch__role {
  margin: 0.15rem 0 0.5rem;
  font: 600 0.74rem var(--hb-font-mono);
  color: var(--hb-accent-bright);
}
.hb-arch__detail {
  margin: 0;
  font-size: 0.88rem;
  line-height: 1.55;
  color: var(--hb-fg-muted);
}
@media (max-width: 880px) {
  .hb-arch__layers {
    grid-template-columns: 1fr 1fr;
  }
  .hb-arch__seam-label {
    font-size: 0.66rem;
  }
}
@media (max-width: 540px) {
  .hb-arch__clients,
  .hb-arch__layers {
    grid-template-columns: 1fr;
  }
}
@media (prefers-reduced-motion: reduce) {
  .hb-arch__seam-beam {
    animation: none;
  }
}
</style>
