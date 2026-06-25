<script setup lang="ts">
import { withBase } from "vitepress";
import { footerNav, GITHUB } from "../landingSpec";
</script>

<template>
  <footer class="hb-footer">
    <div class="hb-wrap hb-footer__inner">
      <div class="hb-footer__brand">
        <a class="hb-footer__logo" :href="withBase('/')" aria-label="Harbor home">
          <img :src="withBase('/harbor_logo.svg')" alt="" width="34" height="34" />
          <span>Harbor</span>
        </a>
        <p class="hb-footer__tag">
          A Go-native runtime for durable, steerable, event-driven AI agents.
        </p>
        <a class="hb-footer__gh" :href="GITHUB" target="_blank" rel="noreferrer">
          github.com/hurtener/Harbor ↗
        </a>
      </div>

      <nav class="hb-footer__cols" aria-label="Footer">
        <div v-for="g in footerNav" :key="g.group" class="hb-footer__col">
          <h3>{{ g.group }}</h3>
          <ul>
            <li v-for="l in g.links" :key="l.text">
              <a
                v-if="l.external"
                :href="l.link"
                target="_blank"
                rel="noreferrer"
              >{{ l.text }}</a>
              <a v-else :href="withBase(l.link)">{{ l.text }}</a>
            </li>
          </ul>
        </div>
      </nav>
    </div>

    <div class="hb-wrap hb-footer__bottom">
      <span>Apache-2.0 licensed — see LICENSE.</span>
      <span>Built with VitePress · the repo is the source of truth.</span>
    </div>
  </footer>
</template>

<style scoped>
.hb-footer {
  border-top: 1px solid var(--hb-border);
  background: var(--hb-bg);
  padding: var(--hb-space-10) var(--hb-space-5) var(--hb-space-6);
}
.hb-footer__inner {
  display: grid;
  grid-template-columns: 1.3fr 3fr;
  gap: var(--hb-space-8);
}
.hb-footer__logo {
  display: inline-flex;
  align-items: center;
  gap: 0.55rem;
  font-weight: 680;
  font-size: 1.1rem;
  color: var(--hb-fg);
  text-decoration: none;
}
.hb-footer__tag {
  margin: var(--hb-space-4) 0;
  max-width: 30ch;
  font-size: 0.88rem;
  line-height: 1.55;
  color: var(--hb-fg-dim);
}
.hb-footer__gh {
  font: 500 0.84rem var(--hb-font-mono);
  color: var(--hb-fg-muted);
  text-decoration: none;
}
.hb-footer__gh:hover {
  color: var(--hb-accent-bright);
}
.hb-footer__cols {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: var(--hb-space-5);
}
.hb-footer__col h3 {
  margin: 0 0 var(--hb-space-3);
  font-size: 0.78rem;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--hb-fg-dim);
  border: 0;
  padding: 0;
}
.hb-footer__col ul {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: var(--hb-space-2);
}
.hb-footer__col a {
  font-size: 0.88rem;
  color: var(--hb-fg-muted);
  text-decoration: none;
  transition: color var(--hb-dur-fast) var(--hb-ease);
}
.hb-footer__col a:hover {
  color: var(--hb-fg);
}
.hb-footer__bottom {
  display: flex;
  flex-wrap: wrap;
  justify-content: space-between;
  gap: var(--hb-space-3);
  margin-top: var(--hb-space-8);
  padding-top: var(--hb-space-5);
  border-top: 1px solid var(--hb-border);
  font-size: 0.82rem;
  color: var(--hb-fg-dim);
}
@media (max-width: 860px) {
  .hb-footer__inner {
    grid-template-columns: 1fr;
    gap: var(--hb-space-6);
  }
  .hb-footer__cols {
    grid-template-columns: 1fr 1fr;
  }
}
@media (max-width: 480px) {
  .hb-footer__cols {
    grid-template-columns: 1fr;
  }
}
</style>
