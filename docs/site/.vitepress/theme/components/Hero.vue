<script setup lang="ts">
import { withBase } from "vitepress";
import { hero } from "../landingSpec";
import CodeBlock from "./CodeBlock.vue";
import CopyButton from "./CopyButton.vue";

const terminalCode = hero.terminal.lines.join("\n");
</script>

<template>
  <section class="hb-hero">
    <div class="hb-hero__beam" aria-hidden="true"></div>
    <div class="hb-wrap hb-hero__grid">
      <div class="hb-hero__copy" v-reveal>
        <p class="hb-eyebrow">
          <span class="hb-eyebrow__beam" aria-hidden="true"></span>{{ hero.eyebrow }}
        </p>
        <h1 class="hb-hero__title">{{ hero.tagline }}</h1>
        <p class="hb-hero__lead">{{ hero.lead }}</p>

        <div class="hb-hero__cta">
          <a
            v-for="c in hero.ctas"
            :key="c.link"
            class="hb-btn"
            :class="c.kind === 'primary' ? 'hb-btn--primary' : 'hb-btn--ghost'"
            :href="withBase(c.link)"
          >
            {{ c.label }}
          </a>
        </div>

        <div class="hb-hero__install">
          <code>{{ hero.install }}</code>
          <CopyButton :text="hero.install" label="Copy install command" />
        </div>
      </div>

      <div class="hb-hero__demo" v-reveal>
        <CodeBlock :code="terminalCode" lang="bash" :file="hero.terminal.file" dots />
      </div>
    </div>
  </section>
</template>

<style scoped>
.hb-hero {
  position: relative;
  overflow: hidden;
  padding: clamp(3rem, 7vw, 6rem) var(--hb-space-5) var(--hb-section-y);
  background:
    radial-gradient(1100px 460px at 78% -8%, rgba(43, 182, 204, 0.13), transparent 60%),
    radial-gradient(800px 420px at 8% 4%, rgba(72, 214, 210, 0.07), transparent 55%);
}
.hb-hero__beam {
  position: absolute;
  inset: 0;
  pointer-events: none;
  background: linear-gradient(
    100deg,
    transparent 40%,
    var(--hb-accent-line) 50%,
    transparent 60%
  );
  background-size: 280% 100%;
  opacity: 0.35;
  animation: hb-sweep var(--hb-dur-slow) var(--hb-ease) 1 both;
}
@keyframes hb-sweep {
  from { background-position: 120% 0; }
  to { background-position: -40% 0; }
}
.hb-hero__grid {
  position: relative;
  display: grid;
  /* minmax(0, …) lets the tracks ignore the terminal's min-content (the long
     `go install …` line in `white-space: pre`) so the code scrolls inside its
     own box instead of forcing the whole grid wider than the viewport. */
  grid-template-columns: minmax(0, 1.05fr) minmax(0, 0.95fr);
  gap: clamp(2rem, 4vw, 3.5rem);
  align-items: center;
}
.hb-hero__grid > * {
  min-width: 0;
}
.hb-hero__title {
  font-size: var(--hb-fs-display);
  font-weight: 720;
  line-height: 1.04;
  letter-spacing: -0.03em;
  margin: var(--hb-space-4) 0 0;
  color: var(--hb-fg);
  border: 0;
  padding: 0;
}
.hb-hero__lead {
  font-size: var(--hb-fs-lead);
  line-height: 1.62;
  color: var(--hb-fg-muted);
  margin: var(--hb-space-5) 0 0;
  max-width: 38ch;
}
.hb-hero__cta {
  display: flex;
  flex-wrap: wrap;
  gap: var(--hb-space-3);
  margin-top: var(--hb-space-6);
}
.hb-hero__install {
  display: inline-flex;
  align-items: center;
  gap: var(--hb-space-3);
  margin-top: var(--hb-space-5);
  padding: 0.5rem 0.5rem 0.5rem 0.9rem;
  background: var(--hb-code-bg);
  border: 1px solid var(--hb-code-border);
  border-radius: var(--hb-radius);
  max-width: 100%;
  overflow: hidden;
}
.hb-hero__install code {
  font: var(--hb-fs-mono) / 1 var(--hb-font-mono);
  color: var(--hb-code-fg);
  white-space: nowrap;
  overflow-x: auto;
}
.hb-hero__copy,
.hb-hero__demo {
  min-width: 0; /* allow the grid items to shrink below their content width */
}
.hb-hero__demo :deep(.hb-code) {
  box-shadow: var(--hb-shadow-lg);
  max-width: 100%;
}
@media (max-width: 880px) {
  .hb-hero__grid {
    grid-template-columns: minmax(0, 1fr);
  }
  .hb-hero__lead {
    max-width: none;
  }
}
@media (prefers-reduced-motion: reduce) {
  .hb-hero__beam {
    animation: none;
  }
}
</style>
