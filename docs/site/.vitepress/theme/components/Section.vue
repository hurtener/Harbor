<script setup lang="ts">
defineProps<{
  eyebrow?: string;
  headline?: string;
  sub?: string;
  alt?: boolean;
  id?: string;
}>();
</script>

<template>
  <section class="hb-section" :class="{ 'hb-section--alt': alt }" :id="id">
    <div class="hb-wrap">
      <header v-if="eyebrow || headline || sub" class="hb-section__head" v-reveal>
        <p v-if="eyebrow" class="hb-eyebrow">
          <span class="hb-eyebrow__beam" aria-hidden="true"></span>{{ eyebrow }}
        </p>
        <h2 v-if="headline" class="hb-section__title">{{ headline }}</h2>
        <p v-if="sub" class="hb-section__sub">{{ sub }}</p>
      </header>
      <slot />
    </div>
  </section>
</template>

<style scoped>
.hb-section {
  padding: var(--hb-section-y) var(--hb-space-5);
  border-top: 1px solid var(--hb-border);
}
.hb-section--alt {
  background: var(--hb-bg-soft);
}
.hb-section__head {
  max-width: 760px;
  margin: 0 0 var(--hb-space-8);
}
.hb-section__title {
  font-size: var(--hb-fs-h2);
  font-weight: 680;
  line-height: 1.1;
  letter-spacing: -0.02em;
  margin: var(--hb-space-3) 0 0;
  color: var(--hb-fg);
  border: 0;
  padding: 0;
}
.hb-section__sub {
  font-size: var(--hb-fs-lead);
  line-height: 1.6;
  color: var(--hb-fg-muted);
  margin: var(--hb-space-4) 0 0;
}
</style>
