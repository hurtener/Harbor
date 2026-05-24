<script lang="ts">
  // Console Settings page (Phase 73m / D-129).
  //
  // The per-operator + per-runtime configuration surface. 12 cards:
  // Connected Runtimes / Per-Runtime Auth / API Tokens / Appearance /
  // Time & Locale / Keybindings / Notifications Routing / Runtime Info /
  // Governance Posture / Storage Drivers / LLM-Provider Posture / About.
  //
  // The page is a pure CONSUMER of upstream surfaces — 72f's runtime
  // posture methods, 72g's governance + LLM posture methods, 72h's
  // Console DB schema. The ONE net-new Protocol method it owns is
  // `auth.rotate_token` (the Per-Runtime Auth card's "Rotate token").
  //
  // Built against `docs/design/console/CONVENTIONS.md` (D-121): routes
  // under `(console)/` (served at `/settings`), renders inside the app
  // shell, composes the shared `ui/` inventory, routes all async state
  // through the four-state `<PageState>`, talks to the Runtime only
  // through `HarborClient` + `connection.ts`, uses design tokens only.
  import { onMount } from 'svelte';
  import {
    PageHeader,
    FilterBar,
    SavedViewChips,
    DataTable,
    DetailRail,
    RailCard,
    StatusChip,
    Pagination,
    PageState,
    type DataTableColumn
  } from '$lib/components/ui/index.js';
  // ConnectionFooter is rendered ONCE by the app shell
  // ((console)/+layout.svelte — CONVENTIONS.md §2). The per-page import was
  // duplicating the footer (post-83k walkthrough N2); removed.
  import SubNavRail from '$lib/components/settings/SubNavRail.svelte';
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
  import { SettingsSavedViews } from '$lib/settings/saved_views.svelte.js';

  const settings = new SettingsState();
  const db = new SettingsDBController();
  const rotate = new RotateTokenState();
  const savedViews = new SettingsSavedViews();

  /** The active section anchor — drives the sub-nav rail highlight. */
  let activeSection = $state<SettingsSectionId>('connected-runtimes');

  /** Pagination model over the 12 sections (depth-bar §5: real pagination). */
  let sectionPage = $state(1);
  let sectionPageSize = $state(6);

  /** The sections shown on the current pagination page. */
  const visibleSections = $derived(
    SETTINGS_SECTIONS.slice(
      (sectionPage - 1) * sectionPageSize,
      sectionPage * sectionPageSize
    )
  );

  /**
   * Phase 83p / D-158 — partition the visible sections by data
   * dependency. Console-local sections render unconditionally; runtime-
   * posture sections route through `<PageState>` so the disconnected /
   * error states show one consolidated placeholder.
   */
  const visibleConsoleLocal = $derived(
    visibleSections.filter((s) => s.group === 'console-local')
  );
  const visibleRuntimePosture = $derived(
    visibleSections.filter((s) => s.group === 'runtime-posture')
  );

  // Reference the section-helpers so Vite + svelte-check do NOT prune the
  // exports; the template branches above derive from the discriminator
  // already, but a future refactor that switches to the helpers should
  // not require re-adding the import. `void` discards the array; the
  // expression keeps the imports reachable without an unused identifier
  // (a pre-83r ESLint drift from Phase 83p — §17.6).
  void [consoleLocalSections, runtimePostureSections];

  /** The Connected-Runtimes table columns (the page's primary DataTable). */
  const RUNTIME_COLUMNS: DataTableColumn[] = [
    { key: 'name', label: 'Runtime' },
    { key: 'base_url', label: 'Base URL' },
    { key: 'transport', label: 'Transport' },
    { key: 'default', label: 'Default' }
  ];

  onMount(() => {
    void settings.load();
    void db.load();
    void savedViews.load();
  });

  function selectSection(id: SettingsSectionId): void {
    activeSection = id;
    settings.activeSection = id;
    // Page the section into the visible window.
    const idx = SETTINGS_SECTIONS.findIndex((s) => s.id === id);
    if (idx >= 0) {
      sectionPage = Math.floor(idx / sectionPageSize) + 1;
    }
    if (typeof document !== 'undefined') {
      document.getElementById('section-' + id)?.scrollIntoView({ behavior: 'smooth' });
    }
  }

  function applySavedView(id: string): void {
    const section = savedViews.sectionFor(id);
    if (section !== null) {
      selectSection(section);
    }
  }

  async function saveCurrentSection(): Promise<void> {
    const name = (
      typeof window !== 'undefined'
        ? window.prompt('Name this section bookmark', activeSection)
        : null
    )?.trim();
    if (name) {
      await savedViews.create(name, activeSection);
    }
  }

  function runtimeRowKey(row: unknown): string {
    return (row as { id: string }).id;
  }
</script>

<svelte:head>
  <title>Settings · Harbor Console</title>
</svelte:head>

<section class="settings-page" data-testid="settings-page">
  <PageHeader
    title="Settings"
    subtitle="Per-operator preferences, runtime connections, and read-only runtime posture."
  />

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews.views}
        activeId={null}
        onselect={applySavedView}
        ondelete={(id) => void savedViews.remove(id)}
      />
    {/snippet}
    {#snippet search()}
      <input
        type="search"
        class="search-input"
        placeholder="Jump to a section…"
        data-testid="settings-search"
        oninput={(e) => {
          const q = (e.currentTarget as HTMLInputElement).value.toLowerCase();
          const hit = SETTINGS_SECTIONS.find((s) => s.label.toLowerCase().includes(q));
          if (hit && q.length > 0) {
            selectSection(hit.id);
          }
        }}
      />
    {/snippet}
    {#snippet actions()}
      <button
        type="button"
        class="bar-action"
        data-testid="settings-save-view"
        onclick={() => void saveCurrentSection()}
      >
        Bookmark section
      </button>
    {/snippet}
  </FilterBar>

  <div class="layout">
    <SubNavRail active={activeSection} onselect={selectSection} />

    <div class="main-col">
      <!--
        Phase 83p / D-158 — Console-local sections (Connected Runtimes,
        Per-Runtime Auth, API Tokens, Appearance, Time & Locale,
        Keybindings, Notifications Routing) render UNCONDITIONALLY. They
        read from the Console-side DB, not from the Runtime posture; the
        operator MUST be able to reach the Connected Runtimes form even
        when no Runtime is attached (it's the ONLY path to attach one).
      -->
      <div class="cards" data-testid="settings-cards-console-local">
        {#each visibleConsoleLocal as section (section.id)}
          <div id="section-{section.id}" class="card-anchor" data-testid="settings-section-{section.id}">
            <RailCard title={section.label}>
              {#if section.id === 'connected-runtimes'}
                <ConnectedRuntimesCard
                  runtimes={db.runtimes}
                  onadd={(name, url) => db.addRuntime(name, url)}
                  onremove={(id) => db.removeRuntime(id)}
                />
                <DataTable
                  columns={RUNTIME_COLUMNS}
                  rows={db.runtimes}
                  rowKey={runtimeRowKey}
                >
                  {#snippet row(r)}
                    {@const rt = r as { name: string; base_url: string; transport: string; is_default: number }}
                    <td>{rt.name}</td>
                    <td>{rt.base_url}</td>
                    <td>{rt.transport}</td>
                    <td>
                      {#if rt.is_default === 1}
                        <StatusChip kind="accent" label="default" />
                      {:else}
                        <span class="muted">—</span>
                      {/if}
                    </td>
                  {/snippet}
                  {#snippet empty()}
                    <p class="muted">No runtimes in the address book yet.</p>
                  {/snippet}
                </DataTable>
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
            </RailCard>
          </div>
        {/each}
      </div>

      <!--
        Phase 83p / D-158 — Runtime-posture sections (Runtime Info,
        Governance Posture, Storage Drivers, LLM-Provider Posture, About)
        DO depend on a live Runtime connection. They route through
        `<PageState>` so the disconnected / error states show ONE
        consolidated placeholder + Retry button instead of N empty cards.
      -->
      <PageState
        status={settings.status}
        error={settings.error}
        onretry={() => void settings.load()}
      >
        {#snippet empty()}
          <p class="empty-headline">No runtime posture to show.</p>
        {/snippet}

        <div class="cards" data-testid="settings-cards-runtime-posture">
          {#each visibleRuntimePosture as section (section.id)}
            <div id="section-{section.id}" class="card-anchor" data-testid="settings-section-{section.id}">
              <RailCard title={section.label}>
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
              </RailCard>
            </div>
          {/each}
        </div>

        <Pagination
          page={sectionPage}
          pageSize={sectionPageSize}
          total={SETTINGS_SECTIONS.length}
          pageSizeOptions={[6, 12]}
          onpage={(p) => (sectionPage = p)}
          onpagesize={(s) => {
            sectionPageSize = s;
            sectionPage = 1;
          }}
        />
      </PageState>
    </div>

    <DetailRail>
      <RailCard title="Active section">
        <p class="rail-line" data-testid="settings-active-section">
          {SETTINGS_SECTIONS.find((s) => s.id === activeSection)?.label ?? '—'}
        </p>
      </RailCard>
      <RailCard title="Runtime">
        {#if settings.posture.info}
          <p class="rail-line">{settings.posture.info.display_name || settings.posture.info.instance_id}</p>
          <p class="rail-sub">Protocol {settings.posture.info.protocol_version}</p>
        {:else}
          <p class="rail-sub">Not connected.</p>
        {/if}
      </RailCard>
      <RailCard title="LLM mode">
        {#if settings.mockMode}
          <StatusChip kind="danger" label="mock (dev-only)" />
        {:else if settings.posture.llm}
          <StatusChip kind="success" label="live" />
        {:else}
          <span class="muted">unknown</span>
        {/if}
      </RailCard>
    </DetailRail>
  </div>
</section>

<style>
  .settings-page {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-4);
  }
  .layout {
    display: flex;
    gap: var(--space-4);
    align-items: flex-start;
  }
  .main-col {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }
  .cards {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .card-anchor {
    scroll-margin-top: var(--space-4);
  }
  .search-input,
  .bar-action {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
    color: var(--color-text);
    font-size: var(--text-sm);
  }
  .bar-action {
    cursor: pointer;
  }
  .empty-headline {
    color: var(--color-text-muted);
  }
  .muted {
    color: var(--color-text-muted);
  }
  .rail-line {
    margin: var(--space-0);
    color: var(--color-text);
    font-size: var(--text-sm);
  }
  .rail-sub {
    margin: var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
</style>
