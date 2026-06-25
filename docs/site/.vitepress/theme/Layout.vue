<script setup lang="ts">
import { useData } from "vitepress";
import DefaultTheme from "vitepress/theme";
import { computed } from "vue";
import Landing from "./components/Landing.vue";

const { Layout: DefaultLayout } = DefaultTheme;
const { frontmatter } = useData();
const isLanding = computed(() => frontmatter.value.layout === "landing");
</script>

<template>
  <Landing v-if="isLanding" />
  <DefaultLayout v-else />
</template>
