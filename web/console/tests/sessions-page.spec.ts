// Harbor Console — Sessions page e2e spec (Phase 108g / D-179; rebuilt
// from the Phase 73c / D-122 spec for the carded, fully-wired rebuild).
//
// Per-page Playwright spec for the Console Sessions page. It exercises
// the carded catalog (`DataTable` inside a `.panel.card`), the four-state
// `PageState`, the calm toolbar (search / sort / refresh), the faceted
// filter strip, the saved-filter chips, the REAL bulk-action toolbar
// (bulk Cancel / Pause wired to the shipped control verbs — D-179, no
// longer a disabled placeholder), the right-rail Session Summary card,
// and the bottom-dock tab strip whose tabs render real session-filtered
// event data.
//
// SKIP semantics (CLAUDE.md §4.2): the spec gates the whole describe
// block on `consoleSubcommandAvailable()`. Data-dependent assertions
// additionally skip when the embedded runtime boots with no seeded
// sessions (§17.6) — the structural tests run unconditionally.

import { test, expect, consoleSubcommandAvailable } from './fixtures/page';
import { BasePage } from './pages/base-page';

/** Page object for the Console Sessions page. */
class SessionsPage extends BasePage {
  readonly selectors = {
    page: "[data-testid='sessions-page']",
    catalogRow: "[data-testid='catalog-row']",
    search: "[data-testid='sessions-search']",
    refresh: "[data-testid='sessions-refresh']",
    saveView: "[data-testid='sessions-save-view']",
    sort: "[data-testid='sessions-sort']",
    // Facet chips (SessionFacetChips — unchanged).
    facets: "[data-testid='session-facets']",
    statusFailed: "[data-testid='status-chip-failed']",
    moreFilters: "[data-testid='more-filters']",
    // Bulk-action toolbar (now wired — D-179).
    bulkBar: "[data-testid='bulk-bar']",
    bulkCancel: "[data-testid='bulk-cancel']",
    bulkPause: "[data-testid='bulk-pause']",
    // The four-state PageState boundary (CONVENTIONS.md §4).
    stateDisconnected: "[data-testid='page-state-disconnected']",
    stateLoading: "[data-testid='page-state-loading']",
    stateError: "[data-testid='page-state-error']",
    stateEmpty: "[data-testid='page-state-empty']",
    retry: "[data-testid='page-state-retry']",
    identityActor: "[data-testid='identity-actor']",
    // Detail route.
    detailPage: "[data-testid='session-detail-page']",
    detailHeader: "[data-testid='session-detail-header']",
    summary: "[data-testid='session-summary']",
    continue: "[data-testid='session-continue']",
    clone: "[data-testid='session-clone']",
    cancel: "[data-testid='session-cancel']",
    convertEval: "[data-testid='session-convert-eval']",
    // Bottom-dock tabs + live stream marker.
    dock: "[data-testid='bottom-dock']",
    dockStreamState: "[data-testid='dock-stream-state']",
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
// identity — `(dev, dev, dev)` (cmd/harbor/devauth.go).
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
    'harbor console subcommand absent or bin/harbor not built',
  );

  test('the catalog renders inside the carded shell', async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    await expect(page.locator(sessions.selectors.page)).toBeVisible();
    await expect(page.locator(sessions.selectors.footer)).toBeVisible();
    const rows = await page.locator(sessions.selectors.catalogRow).count();
    test.skip(rows === 0, 'no sessions seeded in the embedded runtime (§17.6)');
    await expect(page.locator(sessions.selectors.catalogRow).first()).toBeVisible();
  });

  test('the catalog carries the lean registry-owned column set (no Cost — D-179)', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    const rows = await page.locator(sessions.selectors.catalogRow).count();
    test.skip(rows === 0, 'no sessions seeded in the embedded runtime (§17.6)');
    for (const header of ['Session', 'Status', 'Agent', 'Identity', 'Started', 'Last activity', 'Events', 'Duration']) {
      await expect(page.getByRole('columnheader', { name: header })).toBeVisible();
    }
    // Cost lives in the detail Cost History tab (no per-session list
    // aggregate wire — D-179); Priority never renders (D-065).
    await expect(page.getByRole('columnheader', { name: 'Cost' })).toHaveCount(0);
    await expect(page.getByRole('columnheader', { name: 'Priority' })).toHaveCount(0);
  });

  test('the toolbar — sort, search, saved view — renders and works', async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    await page.locator(sessions.selectors.sort).selectOption('cost_desc');
    await page.locator(sessions.selectors.search).fill('agent');
    await page.locator(sessions.selectors.search).press('Enter');
    await expect(page.locator(sessions.selectors.saveView)).toBeVisible();
  });

  test('the status=Failed facet re-invokes the listing', async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    await page.locator(sessions.selectors.statusFailed).click();
    await expect(page.locator(sessions.selectors.facets)).toBeVisible();
  });

  test('the bulk-action toolbar appears + bulk Cancel is wired (not a disabled placeholder)', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    const checkbox = page.locator("[data-testid='sessions-page'] input[type='checkbox']");
    if ((await checkbox.count()) > 1) {
      await checkbox.nth(1).check();
      await expect(page.locator(sessions.selectors.bulkBar)).toBeVisible();
      // D-179: with the admin/control scope seeded, bulk Cancel is
      // ENABLED (wired to the shipped `cancel` verb) — not the old
      // permanently-disabled placeholder.
      await expect(page.locator(sessions.selectors.bulkCancel)).toBeEnabled();
      await expect(page.locator(sessions.selectors.bulkPause)).toBeEnabled();
    }
  });

  test('selecting a row opens the carded detail + Session Summary', async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    const rows = await page.locator(sessions.selectors.catalogRow).count();
    test.skip(rows === 0, 'no sessions seeded in the embedded runtime (§17.6)');
    await page.locator(sessions.selectors.catalogRow).first().click();
    await expect(page.locator(sessions.selectors.detailPage)).toBeVisible();
    await expect(page.locator(sessions.selectors.detailHeader)).toBeVisible();
    await expect(page.locator(sessions.selectors.summary)).toBeVisible();
    // The real action set renders; Convert-to-Evaluation stays disabled (D-064).
    await expect(page.locator(sessions.selectors.continue)).toBeVisible();
    await expect(page.locator(sessions.selectors.convertEval)).toBeDisabled();
  });

  test('the bottom-dock cycles all five tabs + shows the live-stream marker', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedSessionsConnection(page, runtime.baseURL, runtime.token);
    const sessions = new SessionsPage(page, runtime.baseURL);
    await sessions.goto();
    const rows = await page.locator(sessions.selectors.catalogRow).count();
    test.skip(rows === 0, 'no sessions seeded in the embedded runtime (§17.6)');
    await page.locator(sessions.selectors.catalogRow).first().click();
    await expect(page.locator(sessions.selectors.dock)).toBeVisible();
    await expect(page.locator(sessions.selectors.dockStreamState)).toBeVisible();
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
});
