<script lang="ts">
  // Harbor Console — Live Runtime right rail (Phase 108d Stage 3 extraction).
  //
  // The page's right rail: the shared `<DetailRail>` + its four `<RailCard>`s
  // (Session / Current step / Recent artifacts / Interventions). Extracted
  // verbatim from `routes/(console)/live-runtime/+page.svelte` — same DOM,
  // same testids, same child components. The page passes in the data the
  // cards render; this component holds no reactive orchestration of its own.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import DetailRail from '$lib/components/ui/DetailRail.svelte';
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import SessionDetailCard from '$lib/components/live-runtime/session-detail-card.svelte';
  import CurrentStepPanel from '$lib/components/live-runtime/current-step-panel.svelte';
  import RecentArtifactsPanel, {
    type RecentArtifact
  } from '$lib/components/live-runtime/recent-artifacts-panel.svelte';
  import InterventionsPanel, {
    type Intervention
  } from '$lib/components/live-runtime/interventions-panel.svelte';
  import type { RuntimeConnection } from '$lib/connection.js';

  let {
    connection,
    sessionStatusLabel,
    costUSD,
    lastError,
    currentStep,
    recentArtifacts,
    interventions
  }: {
    /** The resolved runtime connection (null when disconnected). */
    connection: RuntimeConnection | null;
    /** The derived session-level lifecycle label. */
    sessionStatusLabel: string;
    /** The aggregated session cost in USD. */
    costUSD: number;
    /** The most recent failure summary, or null when none. */
    lastError: string | null;
    /** The current planner step, or null. */
    currentStep: string | null;
    /** The recent-artifacts list. */
    recentArtifacts: RecentArtifact[];
    /** The interventions log. */
    interventions: Intervention[];
  } = $props();
</script>

<DetailRail>
  <RailCard title="Session">
    {#if connection !== null}
      <SessionDetailCard
        identity={connection.identity}
        agentName="default agent"
        sessionStatus={sessionStatusLabel}
        costUSD={costUSD}
        lastError={lastError}
        tenant={connection.identity.tenant}
      />
    {:else}
      <p class="rail-note">Not connected to a Runtime.</p>
    {/if}
  </RailCard>
  <RailCard title="Current step">
    <CurrentStepPanel step={currentStep} detail="Derived from the live planner event stream." />
  </RailCard>
  <RailCard title="Recent artifacts">
    <RecentArtifactsPanel artifacts={recentArtifacts} />
  </RailCard>
  <RailCard title="Interventions">
    <InterventionsPanel interventions={interventions} />
  </RailCard>
</DetailRail>

<style>
  .rail-note {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
