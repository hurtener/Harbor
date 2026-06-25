<script setup lang="ts">
import { ref, onMounted, onUnmounted } from "vue";
import { withBase } from "vitepress";
import { navLinks, hero, GITHUB } from "../landingSpec";

const scrolled = ref(false);
const open = ref(false);
function onScroll() {
  scrolled.value = window.scrollY > 8;
}
onMounted(() => {
  onScroll();
  window.addEventListener("scroll", onScroll, { passive: true });
});
onUnmounted(() => window.removeEventListener("scroll", onScroll));
</script>

<template>
  <header class="hb-nav" :class="{ 'hb-nav--scrolled': scrolled }">
    <div class="hb-nav__inner hb-wrap">
      <a class="hb-nav__brand" :href="withBase('/')" aria-label="Harbor home">
        <img :src="withBase('/harbor_logo.svg')" alt="" width="30" height="30" />
        <span>Harbor</span>
      </a>

      <nav class="hb-nav__links" aria-label="Primary">
        <a v-for="l in navLinks" :key="l.link" :href="withBase(l.link)">{{ l.text }}</a>
      </nav>

      <div class="hb-nav__actions">
        <a class="hb-nav__ghost" :href="GITHUB" target="_blank" rel="noreferrer">
          <svg width="18" height="18" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
            <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0016 8c0-4.42-3.58-8-8-8z"/>
          </svg>
          <span>GitHub</span>
        </a>
        <a class="hb-btn hb-btn--primary hb-btn--sm" :href="withBase(hero.ctas[0].link)">
          Get started
        </a>
        <button
          class="hb-nav__burger"
          type="button"
          :aria-expanded="open"
          aria-label="Toggle menu"
          @click="open = !open"
        >
          <span></span><span></span><span></span>
        </button>
      </div>
    </div>

    <nav v-show="open" class="hb-nav__mobile" aria-label="Mobile">
      <a v-for="l in navLinks" :key="l.link" :href="withBase(l.link)" @click="open = false">
        {{ l.text }}
      </a>
      <a :href="GITHUB" target="_blank" rel="noreferrer">GitHub</a>
    </nav>
  </header>
</template>

<style scoped>
.hb-nav {
  position: sticky;
  top: 0;
  z-index: 50;
  backdrop-filter: blur(10px);
  background: rgba(13, 17, 23, 0.7);
  border-bottom: 1px solid transparent;
  transition: border-color var(--hb-dur) var(--hb-ease),
    background var(--hb-dur) var(--hb-ease);
}
.hb-nav--scrolled {
  border-bottom-color: var(--hb-border);
  background: rgba(13, 17, 23, 0.86);
}
.hb-nav__inner {
  display: flex;
  align-items: center;
  gap: var(--hb-space-5);
  height: 60px;
}
.hb-nav__brand {
  display: inline-flex;
  align-items: center;
  gap: 0.55rem;
  font-weight: 680;
  font-size: 1.06rem;
  color: var(--hb-fg);
  text-decoration: none;
  letter-spacing: -0.01em;
}
.hb-nav__links {
  display: flex;
  gap: var(--hb-space-5);
  margin-left: var(--hb-space-4);
}
.hb-nav__links a {
  color: var(--hb-fg-muted);
  text-decoration: none;
  font-size: 0.92rem;
  font-weight: 500;
  transition: color var(--hb-dur-fast) var(--hb-ease);
}
.hb-nav__links a:hover {
  color: var(--hb-fg);
}
.hb-nav__actions {
  display: flex;
  align-items: center;
  gap: var(--hb-space-3);
  margin-left: auto;
}
.hb-nav__ghost {
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  color: var(--hb-fg-muted);
  text-decoration: none;
  font-size: 0.9rem;
  font-weight: 500;
}
.hb-nav__ghost:hover {
  color: var(--hb-fg);
}
.hb-nav__burger {
  display: none;
  flex-direction: column;
  gap: 4px;
  background: none;
  border: 0;
  cursor: pointer;
  padding: 6px;
}
.hb-nav__burger span {
  width: 20px;
  height: 2px;
  background: var(--hb-fg);
  border-radius: 2px;
}
.hb-nav__mobile {
  display: none;
  flex-direction: column;
  padding: var(--hb-space-3) var(--hb-space-5) var(--hb-space-5);
  border-bottom: 1px solid var(--hb-border);
  background: var(--hb-bg);
}
.hb-nav__mobile a {
  padding: 0.6rem 0;
  color: var(--hb-fg-muted);
  text-decoration: none;
  border-bottom: 1px solid var(--hb-border);
}
@media (max-width: 860px) {
  .hb-nav__links,
  .hb-nav__ghost span {
    display: none;
  }
  .hb-nav__burger {
    display: flex;
  }
  .hb-nav__mobile {
    display: flex;
  }
}
</style>
