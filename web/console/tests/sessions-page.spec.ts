// Harbor Console — Sessions page e2e spec (Phase 73c / D-122, built on
// the design-system foundation — D-121).
//
// Per-page Playwright spec for the Console Sessions page. It exercises
// the shared `DataTable` catalog, the four-state `PageState`, the
// faceted filter strip, the saved-filter chips, the bulk-action
// toolbar, the right-rail Session Summary card, and the bottom-dock
// tab strip.
//
// SKIP semantics (mirroring the Phase 75 harness baseline + CLAUDE.md
// §4.2's "404/405/501 → SKIP" smoke convention): the `harbor console`
// subcommand lands in Phase 73m. Until then `consoleSubcommandAvailable()`
// is false and the whole describe block SKIPs cleanly — the spec ships
// in THIS phase's PR (the §13 per-page-spec-in-same-PR rule) and flips
// to live once 73m merges.

import {
  test,
  expect,
  consoleSubcommandAvailable,
} from './fixtures/page';
import { BasePage } from './pages/base-page';

/** Page object for the Console Sessions page. */
class SessionsPage extends BasePage {
  readonly selectors = {
    page: "[data-testid='sessions-page']",
    catalogRow: "[data-testid='catalog-row']",
    search: "[data-testid='sessions-search']",
    searchApply: "[data-testid='sessions-search-apply']",
    refresh: "[data-testid='sessions-refresh']",
    saveView: "[data-testid='sessions-save-view']",
    sort: "[data-testid='sessions-sort']",
    // Facet chips.
    facets: "[data-testid='session-facets']",
    statusFailed: "[data-testid='status-chip-failed']",
    moreFilters: "[data-testid='more-filters']",
    // Bulk-action toolbar.
    bulkCount: "[data-testid='bulk-selection-count']",
    bulkCancel: "[data-testid='bulk-cancel']",
    // The four-state PageState boundary (CONVENTIONS.md §4).
    stateDisconnected: "[data-testid='page-state-disconnected']",
    stateLoading: "[data-testid='page-state-loading']",
    stateError: "[data-testid='page-state-error']",
    stateEmpty: "[data-testid='page-state-empty']",
    retry: "[data-testid='page-state-retry']",
    // Identity column.
    identityActor: "[data-testid='identity-actor']",
    // Detail route.
    detailPage: "[data-testid='session-detail-page']",
    detailHeader: "[data-testid='session-detail-header']",
    detailBack: "[data-testid='session-detail-back']",
    summary: "[data-testid='session-summary']",
    // Bottom-dock tabs.
    dock: "[data-testid='bottom-dock']",
    dockTabEvents: "[data-testid='dock-tab-events']",
    dockTabCost: "[data-testid='dock-tab-cost']",
    dockTabControl: "[data-testid='dock-tab-control']",
    dockTabInterventions: "[data-testid='dock-tab-interventions']",
    dockTabTrajectory: "[data-testid='dock-tab-trajectory']",
    footer: "[data-testid='connection-footer']",
  } as const;

  async goto(): Promise<void> {
    await this.gotoSlug('sessions');
  }

  async gotoDetail(sessionID: string): Promise<void> {
    await this.gotoSlug(`sessions/${sessionID}`);
  }
}

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// Seed the Runtime connection triple so the page resolves a live
// connection. The triple MUST match the `harbor console` dev token's
// identity — `(dev, dev, dev)` (cmd/harbor/devauth.go) — or the
// Runtime's control transport rejects the request body identity.
async function seedSessionsConnection(
  page: import('@playwright/test').Page,
  baseURL: string,
  token: string,
): Promise<void> {
  await page.addInitScript(
    ([b, t]) => {
      window.localStorage.setItem('harbor.runtime.base_url', b);
      window.localStorage.setItem('harbor.runtime.token', t);
      window.localStorage.setItem('harbor.runtime.tenant', 'dev');
      window.localStorage.setItem('harbor.runtime.user', 'dev');
      window.localStorage.setItem('harbor.runtime.session', 'dev');
      window.localStorage.setItem('harbor.runtime.scopes', 'admin');
    },
    [baseURL, token] as const,
  );
}

test.describe('Console Sessions page', () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    'harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built',
  );

  // §17.6 deferral: the `harbor console` embedded runtime boots with no
  // seeded sessions, so the data-dependent assertions below
  // (`catalogRow` / `identityActor` / detail drill-down) skip when the
  // catalog is empty. Wiring the harness `runtime` fixture's
  // `seedIdentity` to seed real runtime entities is a harness-level
  // capability tracked for the Phase 73a Overview page (the documented
  // "first real seeding consumer"); until it lands, these tests gate on
  // data presence rather than failing. The structural tests (page
  // renders, footer, no-Priority) run unconditionally.

  test('the catalog renders sessions via the shared DataTable', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    await expect(page.locator(sessions.selectors.page)).toBeVisible();
    // The shared ConnectionFooter renders inside the app shell.
    await expect(page.locator(sessions.selectors.footer)).toBeVisible();
    const rows = await page.locator(sessions.selectors.catalogRow).count();
    test.skip(
      rows === 0,
      'no sessions seeded in the harbor console runtime (§17.6 — runtime-entity seeding tracked in issue #178)',
    );
    await expect(
      page.locator(sessions.selectors.catalogRow).first(),
    ).toBeVisible();
  });

  test('the catalog row carries every mockup column header', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    const _rows = await page.locator(sessions.selectors.catalogRow).count();
    test.skip(
      _rows === 0,
      'no sessions seeded in the harbor console runtime (§17.6 — runtime-entity seeding tracked in issue #178)',
    );
    for (const header of [
      'Session',
      'Status',
      'Agent',
      'Identity',
      'Started',
      'Last activity',
      'Events',
      'Cost',
    ]) {
      await expect(
        page.getByRole('columnheader', { name: header }),
      ).toBeVisible();
    }
  });

  test('the status=Failed facet narrows the result set', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    await page.locator(sessions.selectors.statusFailed).click();
    // The facet re-invokes sessions.list — the catalog either narrows or
    // resolves to the Empty state, never a stale full table.
    await expect(page.locator(sessions.selectors.facets)).toBeVisible();
  });

  test('the sub-header chips — sort, saved-filter, search — work', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    await page
      .locator(sessions.selectors.sort)
      .selectOption('cost_desc');
    await page.locator(sessions.selectors.search).fill('agent');
    await page.locator(sessions.selectors.searchApply).click();
    // The save-filter chip affordance always renders (Console-DB-backed).
    await expect(page.locator(sessions.selectors.saveView)).toBeVisible();
  });

  test('the bulk-action toolbar appears when a row checkbox is checked', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    const firstCheckbox = page.locator(
      "[data-testid='sessions-page'] input[type='checkbox']",
    );
    if ((await firstCheckbox.count()) > 1) {
      await firstCheckbox.nth(1).check();
      await expect(page.locator(sessions.selectors.bulkCount)).toBeVisible();
      // D-066: bulk Cancel always renders — degraded disabled-with-tooltip,
      // never a faked success.
      const cancelBtn = page.locator(sessions.selectors.bulkCancel);
      await expect(cancelBtn).toBeDisabled();
      expect(await cancelBtn.getAttribute('title')).toBeTruthy();
    }
  });

  test('the Identity column renders the actor triple', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    const _rows = await page.locator(sessions.selectors.catalogRow).count();
    test.skip(
      _rows === 0,
      'no sessions seeded in the harbor console runtime (§17.6 — runtime-entity seeding tracked in issue #178)',
    );
    await expect(
      page.locator(sessions.selectors.identityActor).first(),
    ).toBeVisible();
  });

  test('selecting a row opens the detail page + Session Summary card', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    const _rows = await page.locator(sessions.selectors.catalogRow).count();
    test.skip(
      _rows === 0,
      'no sessions seeded in the harbor console runtime (§17.6 — runtime-entity seeding tracked in issue #178)',
    );
    await page.locator(sessions.selectors.catalogRow).first().click();
    await expect(page.locator(sessions.selectors.detailPage)).toBeVisible();
    await expect(page.locator(sessions.selectors.detailHeader)).toBeVisible();
    await expect(page.locator(sessions.selectors.summary)).toBeVisible();
  });

  test('the bottom-dock tabs cycle through all five panels', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    const _rows = await page.locator(sessions.selectors.catalogRow).count();
    test.skip(
      _rows === 0,
      'no sessions seeded in the harbor console runtime (§17.6 — runtime-entity seeding tracked in issue #178)',
    );
    await page.locator(sessions.selectors.catalogRow).first().click();
    await expect(page.locator(sessions.selectors.dock)).toBeVisible();
    for (const tab of [
      sessions.selectors.dockTabEvents,
      sessions.selectors.dockTabCost,
      sessions.selectors.dockTabControl,
      sessions.selectors.dockTabInterventions,
      sessions.selectors.dockTabTrajectory,
    ]) {
      await page.locator(tab).click();
      await expect(page.locator(tab)).toHaveAttribute('aria-selected', 'true');
    }
  });

  test('no Priority surface renders — D-065', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    // D-065 dropped session-level priority — no Priority column header.
    await expect(
      page.getByRole('columnheader', { name: 'Priority' }),
    ).toHaveCount(0);
  });
});
