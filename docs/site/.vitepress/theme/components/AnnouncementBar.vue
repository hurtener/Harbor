<script setup lang="ts">
import { ref, onMounted } from "vue";
import { withBase } from "vitepress";
import { announcement } from "../landingSpec";

const KEY = "hb-ann-v1.6.0";
const show = ref(false);
onMounted(() => {
  try {
    show.value = localStorage.getItem(KEY) !== "1";
  } catch {
    show.value = true;
  }
});
function dismiss() {
  show.value = false;
  try {
    localStorage.setItem(KEY, "1");
  } catch {
    /* storage unavailable — bar just reappears next load, harmless */
  }
}
</script>

<template>
  <div v-if="show" class="hb-ann">
    <a class="hb-ann__link" :href="withBase(announcement.link)">
      <span class="hb-ann__tag">New</span>
      <span>{{ announcement.text }}</span>
      <span class="hb-ann__arrow" aria-hidden="true">→</span>
    </a>
    <button class="hb-ann__x" type="button" aria-label="Dismiss announcement" @click="dismiss">
      ×
    </button>
  </div>
</template>

<style scoped>
.hb-ann {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--hb-space-3);
  padding: 0.5rem 2.5rem;
  background: linear-gradient(90deg, var(--hb-panel), var(--hb-panel-2));
  border-bottom: 1px solid var(--hb-border);
  font-size: 0.85rem;
}
.hb-ann__link {
  display: inline-flex;
  align-items: center;
  gap: var(--hb-space-3);
  color: var(--hb-fg-muted);
  text-decoration: none;
  font-weight: 500;
}
.hb-ann__link:hover {
  color: var(--hb-fg);
}
.hb-ann__link:hover .hb-ann__arrow {
  transform: translateX(3px);
}
.hb-ann__tag {
  font: 700 0.66rem var(--hb-font-mono);
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--hb-bg);
  background: var(--hb-accent-bright);
  border-radius: 999px;
  padding: 0.1rem 0.5rem;
}
.hb-ann__arrow {
  color: var(--hb-accent-bright);
  transition: transform var(--hb-dur-fast) var(--hb-ease);
}
.hb-ann__x {
  position: absolute;
  right: 0.75rem;
  top: 50%;
  transform: translateY(-50%);
  background: none;
  border: 0;
  color: var(--hb-fg-dim);
  font-size: 1.2rem;
  line-height: 1;
  cursor: pointer;
  padding: 0.2rem 0.4rem;
}
.hb-ann__x:hover {
  color: var(--hb-fg);
}
.hb-ann__x:focus-visible {
  outline: 2px solid var(--hb-accent-bright);
  outline-offset: 2px;
}
@media (max-width: 640px) {
  .hb-ann__tag {
    display: none;
  }
}
</style>
