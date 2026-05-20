<script lang="ts">
  // Harbor Console — Overview page placeholder (D-121).
  //
  // The Overview page is the root redirect target and the first sidebar
  // entry. The full page lands in Phase 73a; this placeholder exists so
  // the `/` → `/overview` redirect has a real target and the app shell
  // renders. Design tokens only (CLAUDE.md §4.5).
  import PageHeader from '$lib/components/ui/PageHeader.svelte';
</script>

<section class="overview" data-testid="overview-placeholder">
  <PageHeader title="Overview" subtitle="Runtime health, activity, and topology at a glance." />
  <p class="placeholder">Overview — lands in Phase 73a.</p>
</section>

<style>
  .overview {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .placeholder {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
