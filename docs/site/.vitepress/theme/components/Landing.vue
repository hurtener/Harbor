<script setup lang="ts">
import {
  nonNegotiables,
  durable,
  capabilities,
  planner,
} from "../landingSpec";
import AnnouncementBar from "./AnnouncementBar.vue";
import SiteNav from "./SiteNav.vue";
import Hero from "./Hero.vue";
import CodeShowcase from "./CodeShowcase.vue";
import ArchDiagram from "./ArchDiagram.vue";
import PrincipleCards from "./PrincipleCards.vue";
import FeatureGrid from "./FeatureGrid.vue";
import ProtocolSection from "./ProtocolSection.vue";
import ComparisonTable from "./ComparisonTable.vue";
import StatBand from "./StatBand.vue";
import WhatsNew from "./WhatsNew.vue";
import FinalCta from "./FinalCta.vue";
import SiteFooter from "./SiteFooter.vue";
</script>

<template>
  <div class="landing">
    <AnnouncementBar />
    <SiteNav />
    <main id="main">
      <Hero />
      <CodeShowcase />
      <ArchDiagram />
      <PrincipleCards
        id="non-negotiables"
        :eyebrow="nonNegotiables.eyebrow"
        :headline="nonNegotiables.headline"
        :sub="nonNegotiables.sub"
        :cards="nonNegotiables.cards"
      />
      <FeatureGrid
        id="durable"
        alt
        :eyebrow="durable.eyebrow"
        :headline="durable.headline"
        :sub="durable.sub"
        :tiles="durable.tiles"
      />
      <ProtocolSection />
      <FeatureGrid
        id="capabilities"
        alt
        :eyebrow="capabilities.eyebrow"
        :headline="capabilities.headline"
        :sub="capabilities.sub"
        :tiles="capabilities.tiles"
      />
      <ComparisonTable />
      <PrincipleCards
        id="planner"
        :eyebrow="planner.eyebrow"
        :headline="planner.headline"
        :sub="planner.sub"
        :cards="planner.cards"
        :note="planner.roadmap"
      />
      <StatBand />
      <WhatsNew />
      <FinalCta />
    </main>
    <SiteFooter />
  </div>
</template>
