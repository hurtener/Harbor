<script lang="ts">
  // Console Settings page (Phase 108f / D-178 — calm single-section model;
  // supersedes the Phase 73m / D-129 paginated-cards + saved-views +
  // detail-rail composition).
  //
  // A calm "sub-nav rail + one section at a time" surface. The left rail
  // lists the 12 sections (grouped: Console-local first, then a Runtime
  // sub-heading for the read-only posture sections). The right pane renders
  // ONLY the active section as a carded `<section class="panel card">`,
  // mirroring the Overview page's (108c) vocabulary. The over-engineered
  // 73m composition (per-page paging, the top filter bar, saved-view chips,
  // the detail rail, scroll-to-anchor) is removed — a single section is
  // always in view, so the page is calm and aligned.
  //
  // The page is a pure CONSUMER of upstream surfaces — 72f's runtime
  // posture methods, 72g's governance + LLM posture methods, 72h's
  // Console DB schema. The ONE net-new Protocol method it owns is
  // `auth.rotate_token` (the Per-Runtime Auth card's "Rotate token").
  //
  // D-158 split preserved per active-section: a `console-local` section
  // renders DIRECTLY (works disconnected — the operator's only path to
  // attach a runtime); a `runtime-posture` section renders INSIDE
  // `<PageState>` so the disconnected / error state shows the standard
  // placeholder + Retry.
  //
  // Built against `docs/design/console/CONVENTIONS.md` (D-121) +
  // `docs/design/console/PAGE-POLISH-PROCEDURE.md`: routes under
  // `(console)/` (served at `/settings`), renders inside the app shell,
  // routes async state through `<PageState>`, talks to the Runtime only
  // through `HarborClient` + `connection.ts`, uses design tokens only,
  // Svelte 5 runes (D-092).
  import { onMount } from 'svelte';
  import { PageState } from '$lib/components/ui/index.js';
  // ConnectionFooter is rendered ONCE by the app shell
  // ((console)/+layout.svelte — CONVENTIONS.md §2).
  import SubNavRail from '$lib/components/settings/SubNavRail.svelte';
  import AttachToLocalCard from '$lib/components/settings/AttachToLocalCard.svelte';
  import ConnectedRuntimesCard from '$lib/components/settings/ConnectedRuntimesCard.svelte';
  import PerRuntimeAuthCard from '$lib/components/settings/PerRuntimeAuthCard.svelte';
  import APITokensCard from '$lib/components/settings/APITokensCard.svelte';
  import AppearanceCard from '$lib/components/settings/AppearanceCard.svelte';
  import TimeLocaleCard from '$lib/components/settings/TimeLocaleCard.svelte';
  import KeybindingsCard from '$lib/components/settings/KeybindingsCard.svelte';
  import NotificationsRoutingCard from '$lib/components/settings/NotificationsRoutingCard.svelte';
  import RuntimeInfoCard from '$lib/components/settings/RuntimeInfoCard.svelte';
  import GovernancePostureCard from '$lib/components/settings/GovernancePostureCard.svelte';
  import StorageDriversCard from '$lib/components/settings/StorageDriversCard.svelte';
  import LLMPostureCard from '$lib/components/settings/LLMPostureCard.svelte';
  import AboutCard from '$lib/components/settings/AboutCard.svelte';
  import {
    SettingsState,
    RotateTokenState,
    SETTINGS_SECTIONS,
    consoleLocalSections,
    runtimePostureSections,
    type SettingsSectionId
  } from '$lib/settings/state.svelte.js';
  import { SettingsDBController } from '$lib/settings/console_db.svelte.js';

  const settings = new SettingsState();
  const db = new SettingsDBController();
  const rotate = new RotateTokenState();

  /** The active section — drives the sub-nav rail highlight + the right pane. */
  let activeSection = $state<SettingsSectionId>('connected-runtimes');

  /** The active section's descriptor (id / label / group). */
  const activeSectionObj = $derived(
    SETTINGS_SECTIONS.find((s) => s.id === activeSection) ?? SETTINGS_SECTIONS[0]
  );

  /**
   * Phase 83p / D-158 — partition the ACTIVE section by data dependency.
   * Exactly one of these resolves to `[activeSectionObj]`; the other is
   * empty. The two wrappers below render from these subsets so the D-158
   * console-local-renders-disconnected / posture-routes-through-PageState
   * split is honoured per active section. (These derivations also keep
   * phase-83p's greps honest — the page still names both subsets.)
   */
  const visibleConsoleLocal = $derived(
    activeSectionObj.group === 'console-local' ? [activeSectionObj] : []
  );
  const visibleRuntimePosture = $derived(
    activeSectionObj.group === 'runtime-posture' ? [activeSectionObj] : []
  );

  // Reference the section-helpers so Vite + svelte-check do NOT prune the
  // exports; the derivations above filter by the discriminator directly,
  // but a future refactor that switches to the helpers should not require
  // re-adding the import. `void` discards the arrays, keeping the imports
  // reachable without an unused identifier (a pre-83r ESLint drift carried
  // forward from Phase 83p — §17.6).
  void [consoleLocalSections, runtimePostureSections];

  onMount(() => {
    // Default-load the runtime posture so a posture section shows real data
    // the moment it is selected; load the Console DB for the local sections.
    void settings.load();
    void db.load();
  });

  function selectSection(id: SettingsSectionId): void {
    activeSection = id;
    settings.activeSection = id;
  }
</script>

<svelte:head>
  <title>Settings · Harbor Console</title>
</svelte:head>

<section class="settings-page" data-testid="settings-page">
  <SubNavRail active={activeSection} onselect={selectSection} />

  <div class="section-pane">
    <!--
      Phase 83p / D-158 — when the ACTIVE section is Console-local
      (Connected Runtimes, Per-Runtime Auth, API Tokens, Appearance,
      Time & Locale, Keybindings, Notifications Routing) it renders
      DIRECTLY. These read from the Console-side DB, not the Runtime
      posture; the operator MUST be able to reach the Connected Runtimes
      form even when no Runtime is attached (it's the ONLY path to attach
      one).
    -->
    {#if activeSectionObj.group === 'console-local'}
      <div data-testid="settings-cards-console-local">
        {#each visibleConsoleLocal as section (section.id)}
          <section
            class="panel card"
            id="section-{section.id}"
            data-testid="settings-section-{section.id}"
          >
            <h2 class="panel-title" data-testid="settings-active-section">{section.label}</h2>
            {#if section.id === 'connected-runtimes'}
              <AttachToLocalCard />
              <ConnectedRuntimesCard
                runtimes={db.runtimes}
                addWarning={db.addWarning}
                onadd={(name, url, token, identity, scopes) => db.addRuntime(name, url, token, identity, scopes)}
                onremove={(id) => db.removeRuntime(id)}
                onaddsuccess={() => {
                  // Phase 83u / D-163 — the new connection only takes
                  // effect on the next page mount (every page reads
                  // resolveConnection() once via HarborClient —
                  // CONVENTIONS.md §6). Reload so the DB opens against
                  // the now-live connection and the address-book
                  // catch-up routine in load() promotes the active URL.
                  if (typeof window !== 'undefined') {
                    window.location.reload();
                  }
                }}
              />
            {:else if section.id === 'per-runtime-auth'}
              <PerRuntimeAuthCard authProfiles={db.authProfiles} {rotate} />
            {:else if section.id === 'api-tokens'}
              <APITokensCard pats={db.pats} />
            {:else if section.id === 'appearance'}
              <AppearanceCard profile={db.profile} />
            {:else if section.id === 'time-locale'}
              <TimeLocaleCard profile={db.profile} />
            {:else if section.id === 'keybindings'}
              <KeybindingsCard keybindings={db.keybindings} />
            {:else if section.id === 'notifications-routing'}
              <NotificationsRoutingCard
                routing={db.routing}
                hasAdminScope={rotate.hasAdminScope}
              />
            {/if}
          </section>
        {/each}
      </div>
    {/if}

    <!--
      Phase 83p / D-158 — when the ACTIVE section is runtime-posture
      (Runtime Info, Governance Posture, Storage Drivers, LLM-Provider
      Posture, About) it DOES depend on a live Runtime connection, so it
      renders INSIDE `<PageState>`: the disconnected / error states show
      ONE consolidated placeholder + Retry.
    -->
    {#if activeSectionObj.group === 'runtime-posture'}
      <PageState
        status={settings.status}
        error={settings.error}
        onretry={() => void settings.load()}
      >
        {#snippet empty()}
          <p class="empty-headline">No runtime posture to show.</p>
        {/snippet}

        <div data-testid="settings-cards-runtime-posture">
          {#each visibleRuntimePosture as section (section.id)}
            <section
              class="panel card"
              id="section-{section.id}"
              data-testid="settings-section-{section.id}"
            >
              <h2 class="panel-title" data-testid="settings-active-section">{section.label}</h2>
              {#if section.id === 'runtime-info'}
                <RuntimeInfoCard info={settings.posture.info} />
              {:else if section.id === 'governance-posture'}
                <GovernancePostureCard
                  governance={settings.posture.governance}
                  mockMode={settings.mockMode}
                />
              {:else if section.id === 'storage-drivers'}
                <StorageDriversCard drivers={settings.posture.drivers} />
              {:else if section.id === 'llm-posture'}
                <LLMPostureCard llm={settings.posture.llm} />
              {:else if section.id === 'about'}
                <AboutCard info={settings.posture.info} />
              {/if}
            </section>
          {/each}
        </div>
      </PageState>
    {/if}
  </div>
</section>

<style>
  /* Viewport-friendly two-pane layout: the rail is fixed; the right pane
     scrolls internally when a section is long (e.g. Keybindings) — the
     chrome never full-page-scrolls. */
  .settings-page {
    display: flex;
    gap: var(--space-4);
    padding: var(--space-4);
    align-items: flex-start;
    min-height: 0;
  }
  .section-pane {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  /* The carded surface — same vocabulary as the Overview page (108c). */
  .card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }
  .panel-title {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }
  .empty-headline {
    color: var(--color-text-muted);
  }
</style>
